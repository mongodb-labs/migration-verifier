package partitions

import (
	"context"
	"fmt"

	"github.com/10gen/mongosync/internal/logger"
	"github.com/10gen/mongosync/internal/mongosync/constants"
	"github.com/10gen/mongosync/internal/mongosync/retry"
	"github.com/10gen/mongosync/internal/mongosync/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/10gen/mongosync/internal/mongosync/resumedata/phase"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// PartitionKey represents the _id of a partition document stored in the destination.
type PartitionKey struct {
	SourceUUID  util.UUID   `bson:"srcUUID"`
	MongosyncID string      `bson:"id"`
	Lower       interface{} `bson:"lowerBound"`
}

// Namespace stores the database and collection name of the namespace being copied.
type Namespace struct {
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

// Partition represents a range of documents in a namespace, bounded by the _id field.
//
// A valid partition must have a non-nil lower bound (in its PartitionKey) and a non-nil upper bound.
type Partition struct {
	Key PartitionKey `bson:"_id"`
	Ns  *Namespace   `bson:"namespace"`

	// The current copying status of the partition.
	Phase phase.PartitionPhase `bson:"partitionPhase"`

	// The upper index key bound for the partition.
	Upper interface{} `bson:"upperBound"`

	// Set to true if the partition is for a capped collection. If so, this partition's
	// upper/lower bounds should be set to the minKey and maxKey of the collection.
	IsCapped bool `bson:"isCapped"`
}

// String returns a string representation of the partition.
func (p *Partition) String() string {
	return fmt.Sprintf(
		"{db: %s, coll: %s, collUUID: %s, mongosyncID: %s, lower: %s, upper: %s, phase: %s}",
		p.Ns.DB, p.Ns.Coll, p.Key.SourceUUID, p.Key.MongosyncID, p.GetLowerBoundString(), p.GetUpperBoundString(), p.Phase)
}

// Persist writes the partition as a document to the destination cluster.
func (p *Partition) Persist(
	ctx context.Context,
	// TODO (REP-1281) - Having this method take a logger and retryer instead
	// of having them as part of the Partition struct is kind of gross. But we
	// construct these partition structs directly in _many_ places, so adding
	// these fields would be painful. I think the right fix is to add a
	// constructor for partitions that requires the logger and retryer.
	logger *logger.Logger,
	retryer retry.Retryer,
	dstClient util.DestinationClient,
) error {
	dstColl := dstClient.Database(constants.MongosyncDB).Collection(constants.PartitionsColl)
	bytes, err := bson.Marshal(p)
	if err != nil {
		return errors.Wrapf(err, "error marshalling partition")
	}

	upsert := true
	err = retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(ri *retry.Info) error {
			ri.Log(
				logger.Logger,
				"replaceOne",
				dstClient.Type(),
				constants.MongosyncDB,
				dstColl.Name(),
				fmt.Sprintf("persisting partition %s", p),
			)
			_, driverErr := dstColl.ReplaceOne(ctx, bson.D{{"_id", p.Key}}, bytes, &options.ReplaceOptions{Upsert: &upsert})
			return driverErr
		})
	if err != nil {
		return errors.Wrapf(err, "error persisting partition")
	}

	return nil
}

// GetLowerBoundString returns the string representation of this partition's lower bound.
func (p *Partition) GetLowerBoundString() string {
	return p.getIndexKeyBoundString(p.Key.Lower)
}

// GetUpperBoundString returns the string representation of this partition's upper bound.
func (p *Partition) GetUpperBoundString() string {
	return p.getIndexKeyBoundString(p.Upper)
}

// getIndexKeyBoundString returns the string representation of the given index key bound.
func (p *Partition) getIndexKeyBoundString(bound interface{}) string {
	switch b := bound.(type) {
	case bson.RawValue:
		return b.String()
	case primitive.MinKey:
		return `{"$minKey":1}`
	case primitive.MaxKey:
		return `{"$maxKey":1}`
	default:
		return fmt.Sprintf("%v", b)
	}
}

// lowerBoundFromCurrent takes the current value of a cursor and returns the value to save as
// the lower bound for the cursor. For capped collections, this is `nil`. For others it's the
// value of the `_id` field.
func (p *Partition) lowerBoundFromCurrent(current bson.Raw) (interface{}, error) {
	if p.IsCapped {
		return nil, nil
	}

	if len(current) == 0 {
		return nil, nil
	}

	var doc bson.M
	err := bson.Unmarshal(current, &doc)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshaling raw document to bson.M")
	}

	if id, ok := doc["_id"]; ok {
		return id, nil
	}

	return nil, errors.New("could not find an '_id' element in the raw document")
}

// FindCmd constructs the Find command for reading documents from the partition. For capped
// collections, the sort order will be `$natural` and the `lowerBound` argument is ignored. For
// all other collections, the collection will be sorted by the `_id` field. The `lowerBound`
// argument will determine the starting point for the find. If it is `nil`, then the value of
// `p.Key.Lower`.
func (p *Partition) FindCmd(
	// TODO (REP-1281)
	logger *logger.Logger,
	startAt *primitive.Timestamp,
	// We only use this for testing.
	batchSize ...int,
) bson.D {
	// Get the bounded query filter from the partition to be used in the Find command.
	findCmd := bson.D{
		{"find", p.Ns.Coll},
		{"collectionUUID", p.Key.SourceUUID},
		{"readConcern", bson.D{
			{"level", "majority"},
			// Start the cursor after the global state's ChangeStreamStartAtTs. Otherwise,
			// there may be changes made by collection copy prior to change event application's
			// start time that are not accounted for, leading to potential data
			// inconsistencies.
			{"afterClusterTime", startAt},
		}},
		// The cursor should not have a timeout.
		{"noCursorTimeout", true},
	}
	if len(batchSize) > 0 {
		findCmd = append(findCmd, bson.E{"batchSize", batchSize[0]})
	}
	if p.IsCapped {
		// For capped collections, sort the documents by their natural order. We deliberately
		// exclude the ID filter to ensure that documents are inserted in the correct order.
		sort := bson.E{"sort", bson.D{{"$natural", 1}}}
		findCmd = append(findCmd, sort)
	} else {
		// For non-capped collections, the cursor should use the ID filter and the _id index.
		// Get the bounded query filter from the partition to be used in the Find command.
		filter := p.filter()
		boundedQueryFilter := bson.E{"filter", filter}
		findCmd = append(findCmd, boundedQueryFilter)

		hint := bson.E{"hint", bson.D{{"_id", 1}}}
		findCmd = append(findCmd, hint)
	}

	return findCmd
}

// filter returns a range filter on _id to be used in a Find query for the
// partition.
func (p *Partition) filter() bson.D {
	// We use $expr to avoid type bracketing and allow comparison of different _id types,
	// and $literal to avoid MQL injection from an _id's value.
	return bson.D{{"$and", bson.A{
		// All _id values >= lower bound.
		bson.D{{"$expr", bson.D{
			{"$gte", bson.A{
				"$_id",
				bson.D{{"$literal", p.Key.Lower}},
			}},
		}}},
		// All _id values <= upper bound.
		bson.D{{"$expr", bson.D{
			{"$lte", bson.A{
				"$_id",
				bson.D{{"$literal", p.Upper}},
			}},
		}}},
	}}}
}
