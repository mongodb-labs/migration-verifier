package partitions

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
)

// Partitions is a slice of partitions.
type Partitions struct {
	logger     *logger.Logger
	partitions []*Partition
}

// NewPartitions returns an empty partition slice.
func NewPartitions(logger *logger.Logger) *Partitions {
	return &Partitions{logger: logger, partitions: nil}
}

// AppendPartitions appends the input slice to the in-memory partitions.
func (p *Partitions) AppendPartitions(partitions []*Partition) {
	p.partitions = append(p.partitions, partitions...)
}

// GetSlice returns the slice of partitions.
func (p *Partitions) GetSlice() []*Partition {
	return p.partitions
}

func PartitionCollectionWithSize(
	ctx context.Context,
	srcColl *mongo.Collection,
	subLogger *logger.Logger,
	partitionSizeInBytes int64,
) ([]*Partition, types.DocumentCount, types.ByteCount, error) {
	collSizeInBytes, collDocCount, err := GetSizeAndDocumentCount(ctx, subLogger, srcColl)
	if err != nil {
		return nil, 0, 0, errors.Wrapf(
			err,
			"failed to fetch %#q’s size and document count",
			srcColl.Database().Name()+"."+srcColl.Name(),
		)
	}

	sampleRate := 1 - float64(partitionSizeInBytes)/float64(collSizeInBytes)

	namespace := srcColl.Database().Name() + "." + srcColl.Name()

	subLogger.Debug().
		Str("namespace", namespace).
		Str("partitionSize", reportutils.FmtBytes(partitionSizeInBytes)).
		Str("collectionSize", reportutils.FmtBytes(collSizeInBytes)).
		Float64("sampleRate", sampleRate).
		Msg("Partitioning collection.")

	rcColl := srcColl.Database().Client().
		Database(
			srcColl.Database().Name(),
			options.Database().SetReadConcern(readconcern.Available()),
		).
		Collection(srcColl.Name())

	boundsCursor, err := rcColl.Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$match", bson.D{{"$sampleRate", sampleRate}}}},
			{{"$project", bson.D{{"_id", 1}}}},
		},
		options.Aggregate().
			SetHint(
				bson.M{
					"_id": 1,
				},
			),
	)

	if err != nil {
		return nil, 0, 0, errors.Wrapf(
			err,
			"failed to query for %#q’s partition boundaries",
			namespace,
		)
	}

	boundsDocs := []bson.M{}
	err = boundsCursor.All(ctx, &boundsDocs)
	if err != nil {
		return nil, 0, 0, errors.Wrapf(
			err,
			"failed to fetch %#q’s partition boundaries",
			namespace,
		)
	}

	subLogger.Debug().
		Str("namespace", namespace).
		Int("boundariesCount", len(boundsDocs)).
		Msg("Received partition boundaries.")

	ranges := lo.Reduce(
		boundsDocs,
		func(agg [][2]any, idDoc bson.M, i int) [][2]any {
			if i == 0 {
				return mslices.Of([2]any{primitive.MinKey{}, idDoc["_id"]})
			}

			priorRange := lo.LastOrEmpty(agg)
			// TODO: panic if empty
			return append(
				agg,
				[2]any{priorRange[1], idDoc["_id"]},
			)
		},
		nil,
	)

	ranges = append(
		ranges,
		[2]any{
			lo.LastOrEmpty(boundsDocs)["_id"],
			primitive.MaxKey{},
		},
	)

	partitions := lo.Map(
		ranges,
		func(curRange [2]any, _ int) *Partition {
			return &Partition{
				Key: PartitionKey{
					Lower: curRange[0],
				},
				Ns:    &Namespace{srcColl.Database().Name(), srcColl.Name()},
				Upper: curRange[1],
			}
		},
	)

	return partitions, types.DocumentCount(collDocCount), types.ByteCount(collSizeInBytes), nil
}

// GetSizeAndDocumentCount uses collStats to return a collection's byte size, document count, and
// capped status, in that order.
//
// Exported for usage in integration tests.
func GetSizeAndDocumentCount(ctx context.Context, logger *logger.Logger, srcColl *mongo.Collection) (int64, int64, error) {
	srcDB := srcColl.Database()
	collName := srcColl.Name()

	value := struct {
		Size  int64 `bson:"size"`
		Count int64 `bson:"count"`
	}{}

	err := retry.New().WithCallback(
		func(ctx context.Context, ri *retry.FuncInfo) error {
			ri.Log(logger.Logger, "collStats", "source", srcDB.Name(), collName, "Retrieving collection size and document count.")
			request := bson.D{
				{"aggregate", collName},
				{"pipeline", mongo.Pipeline{
					bson.D{{"$collStats", bson.D{
						{"storageStats", bson.E{"scale", 1}},
					}}},
					// The "$group" here behaves as a project and rename when there's only one
					// document (non-sharded case).  When there are multiple documents (one for
					// each shard) it correctly sums the counts and sizes from each shard.
					bson.D{{"$group", bson.D{
						{"_id", "ns"},
						{"count", bson.D{{"$sum", "$storageStats.count"}}},
						{"size", bson.D{{"$sum", "$storageStats.size"}}},
						{"capped", bson.D{{"$first", "$capped"}}}}}},
				}},
				{"cursor", bson.D{}},
			}

			cursor, driverErr := srcDB.RunCommandCursor(ctx, request)
			if driverErr != nil {
				return driverErr
			}

			defer cursor.Close(ctx)
			if cursor.Next(ctx) {
				if err := cursor.Decode(&value); err != nil {
					return errors.Wrapf(err, "failed to decode $collStats response for source namespace %s.%s", srcDB.Name(), collName)
				}
			}
			return nil
		},
		"retrieving %#q's statistics",
		srcDB.Name()+"."+collName,
	).Run(ctx, logger)

	// TODO (REP-960): remove this check.
	// If we get NamespaceNotFoundError then return 0,0 since we won't do any partitioning with those returns
	// and the aggregation did not fail so we do not want to return an error. A
	// NamespaceNotFoundError can happen if the database does not exist.
	if util.IsNamespaceNotFoundError(err) {
		return 0, 0, nil
	}

	if err != nil {
		return 0, 0, errors.Wrapf(err, "failed to run aggregation for $collStats for source namespace %s.%s", srcDB.Name(), collName)
	}

	logger.Debug().Msgf("Collection %s.%s size: %d, document count: %d",
		srcDB.Name(), collName, value.Size, value.Count)

	return value.Size, value.Count, nil
}
