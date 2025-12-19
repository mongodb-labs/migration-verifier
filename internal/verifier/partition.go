package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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

	var partitionsCount int
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	err := retry.New().WithCallback(
		func(ctx context.Context, fi *retry.FuncInfo) error {
			var err error

			partitionsCount, docCount, byteCount, err = verifier.createPartitionTasksWithSampleRateRetryable(ctx, task, fi)

			return err
		},
		"partitioning %#q",
		srcNs,
	).Run(ctx, verifier.logger)

	return partitionsCount, docCount, byteCount, err
}

func (verifier *Verifier) createPartitionTasksWithSampleRateRetryable(
	ctx context.Context,
	task *VerificationTask,
	fi *retry.FuncInfo,
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
		// NB: $sort MUST precede $project in order to avoid a blocking sort
		// in pre-v6 server versions.
		{{"$sort", bson.D{{"_id", 1}}}},
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
			bson.MaxKey{},
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

	dstNs := FullName(verifier.dstClientCollection(task))

	namespaceAndUUID, err := uuidutil.GetCollectionNamespaceAndUUID(
		ctx,
		verifier.logger,
		verifier.srcClientDatabase(srcColl.Database().Name()),
		srcColl.Name(),
	)
	if err != nil {
		return 0, 0, 0, errors.Wrapf(err, "fetching %#q’s UUID on source", srcNs)
	}

	partitionsCount := 0

	lowerBound := lowerBoundOpt.OrElse(bson.MinKey{})

	createAndInsertPartition := func(lowerBound, upperBound any) error {
		partition := partitions.Partition{
			Key: partitions.PartitionKey{
				SourceUUID: namespaceAndUUID.UUID,
				Lower:      lowerBound,
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

		fi.NoteSuccess("inserted partition #%d", partitionsCount)

		return nil
	}

	idealNumPartitions := util.DivideToF64(collBytes, idealPartitionBytes)

	// We only want to go in here when the collection has enough data
	// to justify partitioning.
	if idealNumPartitions > 1 {
		docsPerPartition := util.DivideToF64(docsCount, idealNumPartitions)

		sampleRate := util.DivideToF64(1, docsPerPartition)

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

		cursor, err := partitions.ForPartitionAggregation(srcColl).Aggregate(
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
	}

	err = createAndInsertPartition(lowerBound, bson.MaxKey{})
	if err != nil {
		return 0, 0, 0, err
	}

	return partitionsCount, types.DocumentCount(docsCount), types.ByteCount(collBytes), nil
}

func (v *Verifier) srcHasSampleRate() bool {
	srcVersion := v.srcClusterInfo.VersionArray

	return srcVersion[0] > 4 || srcVersion[1] >= 4
}
