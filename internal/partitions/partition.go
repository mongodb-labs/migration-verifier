package partitions

import (
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// PartitionKey represents the _id of a partition document stored in the destination.
type PartitionKey struct {
	SourceUUID  util.UUID `bson:"srcUUID"`
	MongosyncID string    `bson:"id"`
	Lower       any       `bson:"lowerBound"`
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

	// The upper index key bound for the partition.
	Upper any `bson:"upperBound"`

	// Set to true if the partition is for a capped collection. If so, this partition's
	// upper/lower bounds should be set to the minKey and maxKey of the collection.
	IsCapped bool `bson:"isCapped"`
}

// String returns a string representation of the partition.
func (p *Partition) String() string {
	return fmt.Sprintf(
		"{db: %s, coll: %s, collUUID: %s, mongosyncID: %s, lower: %s, upper: %s}",
		p.Ns.DB, p.Ns.Coll, p.Key.SourceUUID, p.Key.MongosyncID, p.GetLowerBoundString(), p.GetUpperBoundString())
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
func (p *Partition) getIndexKeyBoundString(bound any) string {
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
func (p *Partition) lowerBoundFromCurrent(current bson.Raw) (any, error) {
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

type PartitionQueryParameters struct {
	filter    option.Option[bson.D]
	sortField option.Option[string]
	hint      option.Option[bson.D]
}

func (pqp PartitionQueryParameters) ToFindOptions() bson.D {
	doc := bson.D{}

	if theFilter, has := pqp.filter.Get(); has {
		doc = append(doc, bson.E{"filter", theFilter})
	}

	pqp.addHintIfNeeded(&doc)

	return doc
}

func (pqp PartitionQueryParameters) ToAggOptions() bson.D {
	pl := mongo.Pipeline{}

	if theFilter, has := pqp.filter.Get(); has {
		pl = append(pl, bson.D{{"$match", theFilter}})
	}

	if theSort, has := pqp.sortField.Get(); has {
		pl = append(pl, bson.D{{"$sort", bson.D{{theSort, 1}}}})
	}

	doc := bson.D{
		{"pipeline", pl},
	}

	pqp.addHintIfNeeded(&doc)

	return doc
}

func (pqp PartitionQueryParameters) addHintIfNeeded(docRef *bson.D) {
	if theHint, has := pqp.hint.Get(); has {
		*docRef = append(*docRef, bson.E{"hint", theHint})
	}
}

// GetQueryParameters returns a PartitionQueryParameters that describes the
// parameters needed to fetch docs for the partition. It is intended to allow
// the same partitioning to be used on different collections (e.g. use the
// partitions on the source to read the destination for verification)
// If the passed-in buildinfo indicates a mongodb version < 5.0, type bracketing is not used.
// filterAndPredicates is a slice of filter criteria that's used to construct the "filter" field in the find option.
func (p *Partition) GetQueryParameters(clusterInfo *util.ClusterInfo, filterAndPredicates bson.A) PartitionQueryParameters {
	params := PartitionQueryParameters{}

	if p == nil {
		if len(filterAndPredicates) > 0 {
			params.filter = option.Some(bson.D{{"$and", filterAndPredicates}})
		}

		return params
	}

	if p.IsCapped {
		// For capped collections, sort the documents by their natural order. We deliberately
		// exclude the ID filter to ensure that documents are inserted in the correct order.
		params.sortField = option.Some("$natural")
	} else {
		// For non-capped collections, sort by _id to minimize the amount of time
		// that a given document spends cached in memory.
		params.sortField = option.Some("_id")

		// For non-capped collections, the cursor should use the ID filter and the _id index.
		// Get the bounded query filter from the partition to be used in the Find command.
		useExprFind := true

		if clusterInfo != nil {
			useExprFind = false

			if clusterInfo.VersionArray != nil {
				useExprFind = clusterInfo.VersionArray[0] >= 5
			}
		}

		filterAndPredicates = slices.Clone(filterAndPredicates)

		if useExprFind {
			filterAndPredicates = append(filterAndPredicates, p.filterWithExpr())
		} else {
			filterAndPredicates = append(filterAndPredicates, p.filterWithExplicitTypeChecks())
		}

		params.hint = option.Some(bson.D{{"_id", 1}})
	}

	if len(filterAndPredicates) > 0 {
		params.filter = option.Some(bson.D{{"$and", filterAndPredicates}})
	}

	return params
}

// filterWithExpr returns a range filter on _id to be used in a Find query for the
// partition.  This filter will properly handle mixed-type _ids, but on server versions
// < 5.0, will not use indexes and thus will be very slow
func (p *Partition) filterWithExpr() bson.D {
	// We use $expr to avoid type bracketing and allow comparison of different _id types,
	// and $literal to avoid MQL injection from an _id's value.
	return bson.D{{"$and", []bson.D{
		// All _id values >= lower bound.
		{{"$expr", bson.D{
			{"$gte", bson.A{
				"$_id",
				bson.D{{"$literal", p.Key.Lower}},
			}},
		}}},
		// All _id values <= upper bound.
		{{"$expr", bson.D{
			{"$lte", bson.A{
				"$_id",
				bson.D{{"$literal", p.Upper}},
			}},
		}}},
	}}}
}

// filterWithExplicitTypeChecks compensates for the server’s type bracketing
// by matching _id types between the partition’s min & max boundaries.
// It should yield the same result as filterWithExpr.
func (p *Partition) filterWithExplicitTypeChecks() bson.D {
	return bson.D{{"$and", []bson.D{
		getGTEQueryPredicate(p.Key.Lower),
		getLTEQueryPredicate(p.Upper),
	}}}
}

func getGTEQueryPredicate(boundary any) bson.D {
	_, greaterTypeStrs, err := getTypeBracketExcludedBSONTypes(boundary)
	if err != nil {
		panic(errors.Wrapf(err, "creating query predicate: gte (%T: %v)", boundary, boundary))
	}

	return bson.D{
		{"$or", []bson.D{
			// All _id values >= lower bound.
			{{"_id", bson.D{{"$gte", boundary}}}},

			// All BSON types above the _id’s type *and* not checked
			// in the _id comparison.
			{{"_id", bson.D{{"$type", greaterTypeStrs}}}},
		}},
	}
}

func getLTEQueryPredicate(boundary any) bson.D {
	lesserTypeStrs, _, err := getTypeBracketExcludedBSONTypes(boundary)
	if err != nil {
		panic(errors.Wrapf(err, "creating query predicate: lte (%T: %v)", boundary, boundary))
	}

	return bson.D{
		{"$or", []bson.D{
			// All _id values <= upper bound.
			{{"_id", bson.D{{"$lte", boundary}}}},

			// All BSON types below the _id’s type *and* not checked
			// in the _id comparison.
			{{"_id", bson.D{{"$type", lesserTypeStrs}}}},
		}},
	}
}
