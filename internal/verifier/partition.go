package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (verifier *Verifier) findLatestPartitionUpperBound(
	ctx context.Context,
	srcNs string,
) (option.Option[any], error) {
	result := verifier.verificationTaskCollection().FindOne(
		ctx,
		bson.D{
			{"generation", 0},
			{"type", verificationTaskVerifyDocuments},
			{"query_filter.namespace", srcNs},
		},
		options.FindOne().
			SetSort(bson.D{
				{"query_filter.partition.upperBound", -1},
			}),
	)

	task := VerificationTask{}
	if err := result.Decode(&task); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return option.None[any](), nil
		}

		return option.None[any](), errors.Wrap(err, "finding latest partition")
	}

	if task.QueryFilter.Partition == nil {
		return option.None[any](), fmt.Errorf("nil partition … shouldn’t happen?!? task=%+v", task)
	}

	return option.FromPointer(&task.QueryFilter.Partition.Upper), nil
}

func (verifier *Verifier) createPartitionTasksWithSampleRate(
	ctx context.Context,
	task *VerificationTask,
) (int, types.DocumentCount, types.ByteCount, error) {
	srcColl := verifier.srcClientCollection(task)
	srcNs := FullName(srcColl)

	shardKeys, err := verifier.getShardKeyFields(
		ctx,
		&uuidutil.NamespaceAndUUID{
			DBName:   srcColl.Database().Name(),
			CollName: srcColl.Name(),
		},
	)
	if err != nil {
		return 0, 0, 0, errors.Wrapf(err, "getting %#q’s shard key", srcNs)
	}

	pipeline := mongo.Pipeline{
		{{"$project", bson.D{{"_id", 1}}}},
	}

	lowerBoundOpt, err := verifier.findLatestPartitionUpperBound(ctx, srcNs)
	if err != nil {
		return 0, 0, 0, err
	}

	if lowerBound, has := lowerBoundOpt.Get(); has {
		verifier.logger.Info().
			Any("resumeFrom", lowerBound).
			Msg("Resuming partitioning from last-created partition’s upper bound.")

		predicates, err := partitions.FilterIdBounds(
			verifier.srcClusterInfo,
			lowerBound,
			primitive.MaxKey{},
		)
		if err != nil {
			return 0, 0, 0, errors.Wrapf(err, "getting lower-bound filter predicate (%v)", lowerBound)
		}

		var filter bson.D
		switch len(predicates) {
		case 2:
			filter = bson.D{{"$and", predicates}}
		case 1:
			filter = predicates[0]
		default:
			panic("no filter predicates??")
		}

		// We want to create partitions starting where we left off. So use
		// the last-created partition’s upper bound as our new lower bound.
		pipeline = append(
			pipeline,
			bson.D{{"$match", filter}},
		)
	}

	idealPartitionBytes := verifier.partitionSizeInBytes

	collBytes, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		verifier.logger,
		srcColl,
	)
	if err != nil {
		return 0, 0, 0, errors.Wrapf(err, "getting %#q’s size", srcNs)
	}

	idealNumPartitions := util.Divide(collBytes, idealPartitionBytes)

	docsPerPartition := util.Divide(docsCount, idealNumPartitions)
	sampleRate := util.Divide(1, docsPerPartition)

	if sampleRate > 0 && sampleRate < 1 {
		pipeline = append(
			pipeline,
			bson.D{
				{"$match", bson.D{
					{"$sampleRate", sampleRate},
				}},
			},
		)
	}

	cursor, err := srcColl.Aggregate(
		ctx,
		pipeline,
		options.Aggregate().
			SetBatchSize(1).
			SetHint(bson.D{{"_id", 1}}),
	)
	if err != nil {
		return 0, 0, 0, errors.Wrapf(err, "opening %#q’s sampling cursor", srcNs)
	}

	defer cursor.Close(ctx)
	cursor.SetBatchSize(1)

	dstNs := FullName(verifier.dstClientCollection(task))

	partitionsCount := 0

	createAndInsertPartition := func(lowerBound, upperBound any) error {
		partition := partitions.Partition{
			Key: partitions.PartitionKey{
				Lower: lowerBound,
			},
			Ns: &partitions.Namespace{
				srcColl.Database().Name(),
				srcColl.Name(),
			},
			Upper:    upperBound,
			IsCapped: isCapped,
		}

		_, err = verifier.InsertPartitionVerificationTask(
			ctx,
			&partition,
			shardKeys,
			dstNs,
		)
		if err != nil {
			return errors.Wrapf(
				err,
				"inserting partition task for namespace %#q",
				srcNs,
			)
		}

		partitionsCount++

		return nil
	}

	lowerBound := lowerBoundOpt.OrElse(primitive.MinKey{})

	for cursor.Next(ctx) {
		upperBound, err := cursor.Current.LookupErr("_id")
		if err != nil {
			return 0, 0, 0, errors.Wrapf(err, "fetching %#q from %#q’s sampling cursor", "_id", srcNs)
		}

		err = createAndInsertPartition(lowerBound, upperBound)
		if err != nil {
			return 0, 0, 0, err
		}

		lowerBound = upperBound
	}

	if cursor.Err() != nil {
		return 0, 0, 0, errors.Wrapf(err, "iterating %#q’s sampling cursor", srcNs)
	}

	err = createAndInsertPartition(lowerBound, primitive.MaxKey{})
	if err != nil {
		return 0, 0, 0, err
	}

	return partitionsCount, types.DocumentCount(docsCount), types.ByteCount(collBytes), nil
}

func (v *Verifier) srcHasSampleRate() bool {
	srcVersion := v.srcClusterInfo.VersionArray

	return srcVersion[0] > 4 || srcVersion[1] >= 4
}
