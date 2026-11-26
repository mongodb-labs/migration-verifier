package verifier

import (
	"bytes"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	minOID = bsontools.ToRawValue(bson.ObjectID{})
	maxOID = bsontools.ToRawValue(bson.ObjectID(bytes.Repeat([]byte{0xff}, len(bson.ObjectID{}))))

	// idPartitionRanges are the _id ranges into which a collection’s
	// generation-0 partitioning is split. Each range gets its own
	// partitionIDRange task, which lets multiple workers partition the
	// collection concurrently.
	//
	// The ranges are:
	//   - MinKey to the lowest possible ObjectID
	//   - the lowest possible ObjectID to the highest possible ObjectID
	//   - the highest possible ObjectID to MaxKey
	//
	// Note that, as with any adjacent partitions, adjacent ranges share a
	// boundary value.
	idPartitionRanges = [][2]bson.RawValue{
		{bsontools.ToRawValue(bson.MinKey{}), minOID},
		{minOID, maxOID},
		{maxOID, bsontools.ToRawValue(bson.MaxKey{})},
	}
)

// createPartitionIDRangeTasks creates the partitionIDRange tasks that
// partition the given collection task’s namespace.
func (verifier *Verifier) createPartitionIDRangeTasks(
	ctx context.Context,
	task *tasks.Task,
	shardKeyFields []string,
) (int, error) {
	srcColl := verifier.srcClientCollection(task)
	dstNs := FullName(verifier.dstClientCollection(task))

	for _, idRange := range idPartitionRanges {
		err := verifier.ensurePartitionIDRangeTask(
			ctx,
			&partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: idRange[0],
				},
				Ns: &partitions.Namespace{
					srcColl.Database().Name(),
					srcColl.Name(),
				},
				Upper: idRange[1],
			},
			shardKeyFields,
			dstNs,
		)
		if err != nil {
			return 0, errors.Wrapf(
				err,
				"creating %#q task for %#q (%v to %v)",
				tasks.PartitionIDRange,
				FullName(srcColl),
				idRange[0],
				idRange[1],
			)
		}
	}

	return len(idPartitionRanges), nil
}

// ProcessPartitionIDRangeTask processes a partitionIDRange task.
func (verifier *Verifier) ProcessPartitionIDRangeTask(
	ctx context.Context,
	workerNum int,
	task tasks.Task,
) error {
	lo.Assertf(
		task.Type == tasks.PartitionIDRange,
		"task %v type should be %#q but is %#q",
		task.PrimaryKey,
		tasks.PartitionIDRange,
		task.Type,
	)

	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Stringer("lowerBound", task.QueryFilter.Partition.Key.Lower).
		Stringer("upperBound", task.QueryFilter.Partition.Upper).
		Msg("Partitioning _id range.")

	partitionsCount, err := verifier.createPartitionTasksWithSampleRate(ctx, &task)
	if err != nil {
		return errors.Wrapf(
			err,
			"partitioning %#q’s _id range (%v to %v)",
			task.QueryFilter.Namespace,
			task.QueryFilter.Partition.Key.Lower,
			task.QueryFilter.Partition.Upper,
		)
	}

	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Stringer("lowerBound", task.QueryFilter.Partition.Key.Lower).
		Stringer("upperBound", task.QueryFilter.Partition.Upper).
		Int("partitionsCount", partitionsCount).
		Msg("Done partitioning _id range.")

	task.Status = tasks.Completed

	return errors.Wrapf(
		verifier.UpdateVerificationTask(ctx, &task),
		"updating task %v’s status",
		task.PrimaryKey,
	)
}

// findLatestPartitionUpperBound returns the highest upper bound among
// the verify-documents tasks that the given partitionIDRange task created.
func (verifier *Verifier) findLatestPartitionUpperBound(
	ctx context.Context,
	parentTaskID bson.ObjectID,
) (option.Option[bson.RawValue], error) {
	result := verifier.verificationTaskCollection().FindOne(
		ctx,
		bson.D{
			{"parent_task_id", parentTaskID},
		},
		options.FindOne().
			SetSort(bson.D{
				{"query_filter.partition.upperBound", -1},
			}),
	)

	task := tasks.Task{}
	if err := result.Decode(&task); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return option.None[bson.RawValue](), nil
		}

		return option.None[bson.RawValue](), errors.Wrap(err, "finding latest partition")
	}

	if task.QueryFilter.Partition == nil {
		return option.None[bson.RawValue](), fmt.Errorf("nil partition … shouldn’t happen?!? task=%+v", task)
	}

	return option.Some(task.QueryFilter.Partition.Upper), nil
}

func (verifier *Verifier) createPartitionTasksWithSampleRate(
	ctx context.Context,
	task *tasks.Task,
) (int, error) {
	lo.Assertf(
		verifier.srcHasSampleRate(),
		"$sampleRate support required",
	)

	var partitionsCount int

	err := retry.New().WithCallback(
		func(ctx context.Context, fi *retry.FuncInfo) error {
			var err error

			partitionsCount, err = verifier.createPartitionTasksWithSampleRateRetryable(
				ctx,
				fi,
				task,
			)

			return err
		},
		"partitioning %#q (%v to %v)",
		task.QueryFilter.Namespace,
		task.QueryFilter.Partition.Key.Lower,
		task.QueryFilter.Partition.Upper,
	).Run(ctx, verifier.logger)

	return partitionsCount, err
}

func (verifier *Verifier) createPartitionTasksWithSampleRateRetryable(
	ctx context.Context,
	fi *retry.FuncInfo,
	task *tasks.Task,
) (int, error) {
	srcColl := verifier.srcClientCollection(task)
	srcNs := FullName(srcColl)
	shardFields := task.QueryFilter.ShardKeys

	rangeLower := task.QueryFilter.Partition.Key.Lower
	rangeUpper := task.QueryFilter.Partition.Upper

	pipeline := mongo.Pipeline{
		// NB: $sort MUST precede $project in order to avoid a blocking sort
		// in pre-v6 server versions.
		{{"$sort", bson.D{{"_id", 1}}}},
		{{"$project", bson.D{{"_id", 1}}}},
	}

	resumeFromOpt, err := verifier.findLatestPartitionUpperBound(ctx, task.PrimaryKey)
	if err != nil {
		return 0, err
	}

	if resumeFrom, has := resumeFromOpt.Get(); has {
		// If the range’s final partition already exists, then there’s
		// nothing more to do.
		if resumeFrom.Equal(rangeUpper) {
			return 0, nil
		}

		verifier.logger.Info().
			Any("task", task.PrimaryKey).
			Str("namespace", srcNs).
			Any("resumeFrom", resumeFrom).
			Msg("Resuming partitioning from last-created partition’s upper bound.")
	}

	// We want to create partitions starting where we left off. So use
	// the last-created partition’s upper bound, if there is one, as our
	// new lower bound.
	lowerBound := resumeFromOpt.OrElse(rangeLower)

	predicates, err := partitions.FilterIdBounds(
		verifier.srcClusterInfo,
		lowerBound,
		rangeUpper,
	)
	if err != nil {
		return 0, errors.Wrapf(err, "getting _id filter predicates (%v to %v)", lowerBound, rangeUpper)
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

	pipeline = append(
		pipeline,
		bson.D{{"$match", filter}},
	)

	idealPartitionBytes := verifier.partitionSizeInBytes

	collBytes, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		verifier.logger,
		srcColl,
	)
	if err != nil {
		return 0, errors.Wrapf(err, "getting %#q’s size", srcNs)
	}

	dstNs := FullName(verifier.dstClientCollection(task))

	partitionsCount := 0

	createAndInsertPartition := func(lowerBound, upperBound bson.RawValue) error {
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

		_, err := verifier.insertPartitionVerificationTask(
			ctx,
			&partition,
			shardFields,
			dstNs,
			option.Some(task.PrimaryKey),
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
		return 0, errors.Wrapf(err, "opening %#q’s sampling cursor", srcNs)
	}

	defer cursor.Close(ctx)
	cursor.SetBatchSize(1)

	for cursor.Next(ctx) {
		upperBound, err := cursor.Current.LookupErr("_id")
		if err != nil {
			return 0, errors.Wrapf(err, "fetching %#q from %#q’s sampling cursor", "_id", srcNs)
		}

		err = createAndInsertPartition(lowerBound, upperBound)
		if err != nil {
			return 0, err
		}

		lowerBound = upperBound
	}

	if cursor.Err() != nil {
		return 0, errors.Wrapf(cursor.Err(), "iterating %#q’s sampling cursor", srcNs)
	}

	err = createAndInsertPartition(lowerBound, rangeUpper)
	if err != nil {
		return 0, err
	}

	return partitionsCount, nil
}

func (v *Verifier) srcHasSampleRate() bool {
	return mmongo.VersionAtLeast(v.srcClusterInfo.VersionArray, 4, 4)
}
