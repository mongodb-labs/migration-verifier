package compare

import (
	"context"
	"fmt"
	"net/url"
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
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func SetDirectHostInConnectionString(connstr, hostname string) (string, error) {
	parsedURI, err := url.ParseRequestURI(connstr)
	if err != nil {
		return "", errors.Wrapf(err, "parsing connection string")
	}

	parsedURI.Host = hostname

	_, connstr, err = mmongo.MaybeAddDirectConnection(parsedURI.String())
	if err != nil {
		return "", errors.Wrapf(err, "tweaking connection string to %#q to ensure direct connection", parsedURI.Host)
	}

	return connstr, nil
}

func rvIsNonEmpty(rv bson.RawValue) bool {
	return !rv.IsZero() && rv.Type != bson.TypeNull
}

// ReadNaturalPartitionFromSource queries a source collection according
// to the given task and sends the relevant data to the destination
// reader and compare channels. This function only returns when there are
// no more documents to read (or a failure happens).
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

	lo.Assertf(
		task.QueryFilter.Partition.Natural,
		"natural partition required; task: %+v",
		task,
	)

	lowerBoundRV := task.QueryFilter.Partition.Key.Lower

	upperRecordID := task.QueryFilter.Partition.Upper
	if !rvIsNonEmpty(upperRecordID) {
		return fmt.Errorf("empty/null upper bound is invalid")
	}

	sess, err := srcClient.StartSession()
	if err != nil {
		return errors.Wrapf(err, "starting session")
	}
	defer sess.EndSession(ctx)

	sctx := mongo.NewSessionContext(ctx, sess)

	db, collName := mmongo.SplitNamespace(task.QueryFilter.Namespace)
	coll := srcClient.Database(db).Collection(collName)

	var cursor *mongo.Cursor

	var resumeTokenOpt option.Option[bson.RawValue]

	if rvIsNonEmpty(lowerBoundRV) {
		resumeTokenOpt = option.Some(lowerBoundRV)
	}

	var canUseStartAt bool

	resumeToken, hasToken := resumeTokenOpt.Get()

	var startRecordID option.Option[bson.RawValue]

	if hasToken {
		version, err := mmongo.GetVersionArray(ctx, srcClient)
		if err != nil {
			return errors.Wrapf(err, "fetching server version")
		}

		canUseStartAt = mmongo.FindCanUseStartAt(version)

		rawToken, err := bsontools.RawValueTo[bson.Raw](resumeToken)
		if err != nil {
			return errors.Wrapf(err, "resume token to %T", rawToken)
		}

		recIDRV, err := rawToken.LookupErr(partitions.RecordID)
		if err != nil {
			return errors.Wrapf(err, "extracting record ID (resume token: %+v)", rawToken)
		}

		startRecordID = option.Some(recIDRV)
	}

	resumeTokenParameter := lo.Ternary(
		canUseStartAt,
		"$_startAt",
		"$_resumeAfter",
	)

	// Because we send `showRecordId` we need to disambiguate the
	// server-added $recordId field from a user field of the same name
	// (however unlikely!). We do that by projecting the original
	// document down a level.
	var docProjection any

	switch compareMethod {
	case ToHashedIndexKey:
		docProjection = GetHashedIndexKeyProjection(task.QueryFilter)
	case Binary, IgnoreOrder:
		docProjection = "$$ROOT"
	}

	createCmd := func(resumeTokenOpt option.Option[bson.RawValue]) bson.D {
		// This will yield documents of this form:
		// {
		//      $recordId: <...>,
		//      doc:       { ... },
		// }
		//
		// NB: If the user document is 16 MiB, then the above will exceed that.
		// Thankfully, the server allows an extra 16 KiB of headroom for such.
		cmd := bson.D{
			{"find", coll.Name()},
			{"hint", bson.D{{"$natural", 1}}},
			{"showRecordId", true},
			{"$_requestResumeToken", true},
			{"filter", docFilter.OrElse(bson.D{})},
			{"readConcern", bson.D{
				{"level", "majority"},
			}},
			{"projection", bson.D{
				{"_id", 0},
				{"doc", docProjection},
			}},
			{"comment", fmt.Sprintf("task %v", task.PrimaryKey)},
		}

		if token, has := resumeTokenOpt.Get(); has {
			cmd = append(cmd, bson.E{resumeTokenParameter, token})
		}

		return cmd
	}

	cursor, err = coll.Database().RunCommandCursor(sctx, createCmd(resumeTokenOpt))

	if err != nil {
		if !mmongo.ErrorHasCode(err, util.KeyNotFoundErrCode) {
			return errors.Wrapf(err, "opening source cursor")
		}

		logger.Debug().
			Any("task", task.PrimaryKey).
			Str("namespace", task.QueryFilter.Namespace).
			Stringer("resumeToken", resumeToken).
			Err(err).
			Msg("Resume token is no longer valid. Will attempt use of earlier tokens.")

		cursor, err = openBackupNaturalCursor(
			ctx,
			logger,
			coll,
			task,
			tasksColl,
			startRecordID,
			createCmd,
		)
		if err != nil {
			return errors.Wrapf(err, "opening backup natural cursor")
		}
	}

	defer cursor.Close(ctx)

	retryState.NoteSuccess("opened cursor")

	var batch []DocWithTS
	var batchDocIDs []DocID

	flush := func(ctx context.Context) error {
		logger.Trace().
			Any("task", task.PrimaryKey).
			Str("namespace", task.QueryFilter.Namespace).
			Int("count", len(batchDocIDs)).
			Msg("Flushing source document IDs to dst.")

		err := chanutil.WriteWithDoneCheck(
			ctx,
			toDst,
			slices.Clone(batchDocIDs),
		)
		if err != nil {
			// NB: This leaks memory, but that shouldn’t matter because
			// this error should crash the verifier.
			return errors.Wrapf(err, "sending %d doc IDs to dst", len(batchDocIDs))
		}

		retryState.NoteSuccess("sent %d-doc batch to dst", len(batch))

		batchDocIDs = batchDocIDs[:0]

		logger.Trace().
			Any("task", task.PrimaryKey).
			Str("namespace", task.QueryFilter.Namespace).
			Int("count", len(batch)).
			Msg("Flushing source documents to compare.")

		toCompareStart := time.Now()

		// Now send documents  to the comparison thread.
		for chunk := range mslices.Chunk(slices.Clone(batch), ToComparatorBatchSize) {
			err = chanutil.WriteWithDoneCheck(
				ctx,
				toCompare,
				chunk,
			)
			if err != nil {
				// NB: This leaks memory, but that shouldn’t matter because
				// this error should crash the verifier.
				return errors.Wrapf(err, "sending %d of %d src docs to compare", len(chunk), len(batch))
			}
		}

		retryState.NoteSuccess("sent %d src docs to compare thread", len(batch))

		logger.Trace().
			Any("task", task.PrimaryKey).
			Int("count", len(batch)).
			Stringer("elapsed", time.Since(toCompareStart)).
			Msg("Done flushing source documents to compare.")

		retryState.NoteSuccess("sent %d docs to compare", len(batch))

		batch = batch[:0]

		return nil
	}

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
			val, err := bsontools.CompareRawValues(recIDRV, startID)
			if err != nil {
				return errors.Wrap(err, "comparing current record ID with start")
			}

			if val < 0 {
				continue cursorLoop
			}
		}

		// NB: There is always an upper bound.
		val, err := bsontools.CompareRawValues(recIDRV, upperRecordID)
		if err != nil {
			return errors.Wrap(err, "comparing record ID with partition’s upper bound")
		}

		if val > 0 {
			break cursorLoop
		}

		var userDoc bson.Raw

		userDocRV, err := cursor.Current.LookupErr("doc")
		if err != nil {
			return errors.Wrapf(err, "getting user document")
		}

		userDoc, err = bsontools.RawValueTo[bson.Raw](userDocRV)
		if err != nil {
			return errors.Wrapf(err, "parsing user document")
		}

		batch = append(batch, NewDocWithTSFromPool(userDoc, *opTime))

		docID, err := compareMethod.RawDocIDForComparison(
			userDoc,
		)
		if err != nil {
			return errors.Wrapf(err, "parsing doc ID for comparison")
		}

		batchDocIDs = append(batchDocIDs, NewDocIDFromPool(docID))

		if cursor.RemainingBatchLength() == 0 {
			if err := flush(ctx); err != nil {
				return errors.Wrapf(err, "flushing docs")
			}
		}
	}

	retryState.NoteSuccess("reached end of cursor")

	if len(batch) > 0 {
		if err := flush(ctx); err != nil {
			return errors.Wrapf(err, "flushing final docs")
		}
	}

	return nil
}

func openBackupNaturalCursor(
	ctx context.Context,
	logger *logger.Logger,
	coll *mongo.Collection,
	task *tasks.Task,
	tasksColl *mongo.Collection,
	startRecordID option.Option[bson.RawValue],
	createCmd func(resumeTokenOpt option.Option[bson.RawValue]) bson.D,
) (*mongo.Cursor, error) {

	// NB: These are in descending order.
	priorResumeTokens, err := tasks.FetchPriorResumeTokens(
		ctx,
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

		cursor, err := coll.Database().RunCommandCursor(ctx, cmd)
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

	cursor, err := coll.Database().RunCommandCursor(ctx, cmd)
	if err != nil {
		return nil, errors.Wrapf(err, "opening source cursor from beginning")
	}

	return cursor, err
}
