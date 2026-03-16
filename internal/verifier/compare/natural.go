package compare

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/mongotools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func rvIsNonEmpty(rv bson.RawValue) bool {
	return !rv.IsZero() && rv.Type != bson.TypeNull
}

// ReadNaturalPartitionFromSource queries a source collection according
// to the given task and sends the relevant data to the destination
// reader and compare channels. This function only returns when there are
// no more documents to read (or a failure happens).
//
// IMPORTANT: The given client MUST be a direct connection to the same node
// that was read to create the partitions.
//
// This closes the passed-in channels when it exits.
//
// NOTE: Each DocWithTS and DocID sent over the channels must be released,
// or the memory will leak.
func ReadNaturalPartitionFromSource(
	ctx context.Context,
	logger *logger.Logger,
	retryState retry.SuccessNotifier,
	srcClient *mongo.Client,
	tasksColl *mongo.Collection,
	task *tasks.Task,
	docFilter option.Option[bson.D],
	compareMethod Method,
	toCompare chan<- []DocWithTS,
	toDst chan<- []DocID,
) error {
	defer close(toCompare)
	defer close(toDst)

	if err := validateNaturalTask(task); err != nil {
		return err
	}

	sess, err := srcClient.StartSession()
	if err != nil {
		return errors.Wrapf(err, "starting session")
	}
	defer sess.EndSession(ctx)

	sctx := mongo.NewSessionContext(ctx, sess)

	db, collName := mmongo.SplitNamespace(task.QueryFilter.Namespace)
	coll := srcClient.Database(db).Collection(collName)

	var resumeTokenOpt option.Option[bson.RawValue]
	var startRecordID option.Option[bson.RawValue]
	var resumeTokenParameter string

	srcVersion, err := mmongo.GetVersionArray(ctx, srcClient)
	if err != nil {
		return errors.Wrapf(err, "fetching source version")
	}

	// Handle Resume Token
	lowerBoundRV := task.QueryFilter.Partition.Key.Lower
	if rvIsNonEmpty(lowerBoundRV) {
		resumeTokenOpt = option.Some(lowerBoundRV)

		startRecordID, resumeTokenParameter, err = setupResumeToken(srcVersion, lowerBoundRV)
		if err != nil {
			return err
		}
	}

	var docProjection any
	if mmongo.WhyFindCannotResume([2]int(srcVersion[:])) == nil {
		docProjection = getDocProjection(task, compareMethod)
	}

	createCmd := func(resumeTokenOpt option.Option[bson.RawValue]) bson.D {
		return buildNaturalFindCmd(
			task, coll.Name(), docFilter, docProjection, resumeTokenOpt, resumeTokenParameter,
		)
	}

	cursor, err := openSourceCursor(sctx, logger, coll, tasksColl, task, createCmd, resumeTokenOpt, startRecordID)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	retryState.NoteSuccess("opened cursor")

	return processCursor(
		ctx, sess, logger, retryState, cursor, task, compareMethod,
		docProjection != nil,
		startRecordID, task.QueryFilter.Partition.Upper, toCompare, toDst,
	)
}

func validateNaturalTask(task *tasks.Task) error {
	lo.Assertf(
		task.QueryFilter.Partition.Natural,
		"natural partition required; task: %+v",
		task,
	)

	if !rvIsNonEmpty(task.QueryFilter.Partition.Upper) {
		return fmt.Errorf("empty/null upper bound is invalid")
	}
	return nil
}

func getDocProjection(task *tasks.Task, compareMethod Method) any {
	switch compareMethod {
	case ToHashedIndexKey:
		return GetHashedIndexKeyProjection(task.QueryFilter)
	case Binary, IgnoreOrder:
		return "$$ROOT"
	default:
		return "$$ROOT"
	}
}

func setupResumeToken(
	srcVersion [3]int,
	lowerBoundRV bson.RawValue,
) (option.Option[bson.RawValue], string, error) {
	canUseStartAt := mmongo.FindCanUseStartAt(srcVersion)

	rawToken, err := bsontools.RawValueTo[bson.Raw](lowerBoundRV)
	if err != nil {
		return option.None[bson.RawValue](), "", errors.Wrapf(err, "resume token to %T", rawToken)
	}

	recIDRV, err := rawToken.LookupErr(partitions.RecordID)
	if err != nil {
		return option.None[bson.RawValue](), "", errors.Wrapf(err, "extracting record ID (resume token: %+v)", rawToken)
	}

	resumeTokenParameter := lo.Ternary(canUseStartAt, "$_startAt", "$_resumeAfter")

	return option.Some(recIDRV), resumeTokenParameter, nil
}

func buildNaturalFindCmd(
	task *tasks.Task,
	collName string,
	docFilter option.Option[bson.D],
	docProjection any,
	resumeTokenOpt option.Option[bson.RawValue],
	resumeTokenParameter string,
) bson.D {
	cmd := bson.D{
		{"find", collName},
		{"comment", fmt.Sprintf("task %v", task.PrimaryKey)},
		{"hint", bson.D{{"$natural", 1}}},
		{"showRecordId", true},
		{"filter", docFilter.OrElse(bson.D{})},
		{"readConcern", bson.D{{"level", "majority"}}},
	}

	if docProjection != nil {
		cmd = append(cmd, bson.D{
			{"projection", bson.D{{"_id", 0}, {"doc", docProjection}}},
			{"$_requestResumeToken", true},
		}...)
	}

	if token, has := resumeTokenOpt.Get(); has {
		cmd = append(cmd, bson.E{Key: resumeTokenParameter, Value: token})
	}

	return cmd
}

func openSourceCursor(
	sctx context.Context, // for the source read
	logger *logger.Logger,
	coll *mongo.Collection,
	tasksColl *mongo.Collection,
	task *tasks.Task,
	createCmd func(option.Option[bson.RawValue]) bson.D,
	resumeTokenOpt option.Option[bson.RawValue],
	startRecordID option.Option[bson.RawValue],
) (*mongo.Cursor, error) {
	cursor, err := coll.Database().RunCommandCursor(sctx, createCmd(resumeTokenOpt))
	if err == nil {
		return cursor, nil
	}

	if !mmongo.ErrorHasCode(err, util.KeyNotFoundErrCode) {
		return nil, errors.Wrapf(err, "opening source cursor")
	}

	logger.Debug().
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Err(err).
		Msg("Resume token is no longer valid. Will attempt use of earlier tokens.")

	cursor, err = openBackupNaturalCursor(sctx, logger, coll, task, tasksColl, startRecordID, createCmd)
	if err != nil {
		return nil, errors.Wrapf(err, "opening backup natural cursor")
	}

	return cursor, nil
}

func processCursor(
	ctx context.Context,
	sess *mongo.Session,
	logger *logger.Logger,
	retryState retry.SuccessNotifier,
	cursor *mongo.Cursor,
	task *tasks.Task,
	compareMethod Method,
	hasResumeToken bool,
	startRecordID option.Option[bson.RawValue],
	upperRecordID bson.RawValue,
	toCompare chan<- []DocWithTS,
	toDst chan<- []DocID,
) error {
	var batch []DocWithTS
	var batchDocIDs []DocID

cursorLoop:
	for {
		if !cursor.Next(ctx) {
			if cursor.Err() != nil {
				return errors.Wrapf(cursor.Err(), "reading documents")
			}
			break
		}

		retryState.NoteSuccess("received a document")

		opTime := sess.OperationTime()
		if opTime == nil {
			panic("session should have optime after reading a document")
		}

		recIDRV, err := cursor.Current.LookupErr(partitions.RecordID)
		if err != nil {
			return errors.Wrapf(err, "getting record ID")
		}

		if startID, has := startRecordID.Get(); has {
			val, err := mongotools.CompareRecordIDs(recIDRV, startID)
			if err != nil {
				return errors.Wrap(err, "comparing current record ID with start")
			}
			if val < 0 {
				continue cursorLoop
			}
		}

		val, err := mongotools.CompareRecordIDs(recIDRV, upperRecordID)
		if err != nil {
			return errors.Wrap(err, "comparing record ID with partition’s upper bound")
		}
		if val > 0 {
			break cursorLoop
		}

		var userDoc bson.Raw

		if hasResumeToken {
			userDoc, err = bsontools.RawLookup[bson.Raw](cursor.Current, "doc")
			if err != nil {
				return errors.Wrapf(err, "getting user document")
			}
		} else {
			var found bool
			userDoc, found, err = bsontools.RemoveFromRaw(cursor.Current, partitions.RecordID)
			if err != nil {
				return errors.Wrapf(err, "removing %#q from user document", partitions.RecordID)
			}

			lo.Assertf(
				found,
				"should always find resume token (doc: %+v)",
				userDoc,
			)
		}

		batch = append(batch, NewDocWithTSFromPool(userDoc, *opTime))

		docID, err := compareMethod.RawDocIDForComparison(userDoc)
		if err != nil {
			return errors.Wrapf(err, "parsing doc ID for comparison")
		}

		batchDocIDs = append(batchDocIDs, NewDocIDFromPool(docID))

		if cursor.RemainingBatchLength() == 0 {
			if err := flushSourceBatch(ctx, logger, retryState, task, &batch, &batchDocIDs, toCompare, toDst); err != nil {
				return errors.Wrapf(err, "flushing docs")
			}
		}
	}

	retryState.NoteSuccess("reached end of cursor")

	if len(batch) > 0 {
		if err := flushSourceBatch(ctx, logger, retryState, task, &batch, &batchDocIDs, toCompare, toDst); err != nil {
			return errors.Wrapf(err, "flushing final docs")
		}
	}

	return nil
}

func flushSourceBatch(
	ctx context.Context,
	logger *logger.Logger,
	retryState retry.SuccessNotifier,
	task *tasks.Task,
	batch *[]DocWithTS,
	batchDocIDs *[]DocID,
	toCompare chan<- []DocWithTS,
	toDst chan<- []DocID,
) error {
	logger.Trace().
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Int("count", len(*batchDocIDs)).
		Msg("Flushing source document IDs to dst.")

	for chunk := range mslices.Chunk(*batchDocIDs, 10_000) {
		err := chanutil.WriteWithDoneCheck(ctx, toDst, slices.Clone(chunk))
		if err != nil {
			return errors.Wrapf(err, "sending %d doc IDs to dst", len(*batchDocIDs))
		}
	}

	retryState.NoteSuccess("sent %d-doc batch to dst", len(*batch))
	*batchDocIDs = (*batchDocIDs)[:0]

	logger.Trace().
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Int("count", len(*batch)).
		Msg("Flushing source documents to compare.")

	toCompareStart := time.Now()

	for chunk := range mslices.Chunk(*batch, ToComparatorBatchSize) {
		err := chanutil.WriteWithDoneCheck(ctx, toCompare, slices.Clone(chunk))
		if err != nil {
			return errors.Wrapf(err, "sending %d of %d src docs to compare", len(chunk), len(*batch))
		}
	}

	retryState.NoteSuccess("sent %d src docs to compare thread", len(*batch))

	logger.Trace().
		Any("task", task.PrimaryKey).
		Int("count", len(*batch)).
		Stringer("elapsed", time.Since(toCompareStart)).
		Msg("Done flushing source documents to compare.")

	retryState.NoteSuccess("sent %d docs to compare", len(*batch))
	*batch = (*batch)[:0]

	return nil
}

func openBackupNaturalCursor(
	sctx context.Context, // for source reads
	logger *logger.Logger,
	srcColl *mongo.Collection,
	task *tasks.Task,
	tasksColl *mongo.Collection,
	startRecordID option.Option[bson.RawValue],
	createCmd func(resumeTokenOpt option.Option[bson.RawValue]) bson.D,
) (*mongo.Cursor, error) {
	lo.Assert(
		mongo.SessionFromContext(sctx) != nil,
		"context must have a session!",
	)

	// NB: These are in descending order.
	priorResumeTokens, err := tasks.FetchPriorResumeTokens(
		mongo.NewSessionContext(sctx, nil), // need a session-less context
		task.QueryFilter.Namespace,
		startRecordID.MustGet(),
		tasksColl,
	)
	if err != nil {
		return nil, errors.Wrapf(err, "fetching resume tokens after a key-not-found error")
	}

	failedTokens := 1

	for _, priorResumeToken := range priorResumeTokens {
		cmd := createCmd(option.Some(bsontools.ToRawValue(priorResumeToken)))

		// No readpref is necessary because this should be a direct connection.
		cursor, err := srcColl.Database().RunCommandCursor(sctx, cmd)
		if err == nil {
			logger.Info().
				Any("task", task.PrimaryKey).
				Str("srcNamespace", task.QueryFilter.Namespace).
				Int("skippedPartitions", failedTokens).
				Msg("Due to a document deletion on the source cluster, this task has to read other tasks’ documents. This task may take longer to complete than others.")

			return cursor, nil
		}

		if !mmongo.ErrorHasCode(err, util.KeyNotFoundErrCode) {
			return nil, errors.Wrapf(err, "opening source cursor after replacing resume token")
		}

		failedTokens++
	}

	lo.Assertf(
		failedTokens > len(priorResumeTokens),
		"failed tokens (%d) should outnumber prior tokens (%d)",
		failedTokens,
		len(priorResumeTokens),
	)

	logger.Info().
		Any("task", task.PrimaryKey).
		Str("srcNamespace", task.QueryFilter.Namespace).
		Int("skippedPartitions", failedTokens).
		Msg("Due to a document deletion on the source cluster, this task has to read the entire collection. This task may take longer to complete than others.")

	cmd := createCmd(option.None[bson.RawValue]())

	// No readpref is necessary because this should be a direct connection.
	cursor, err := srcColl.Database().RunCommandCursor(sctx, cmd)
	if err != nil {
		return nil, errors.Wrapf(err, "opening source cursor from beginning")
	}

	return cursor, err
}
