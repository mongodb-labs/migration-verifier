package compare

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mmongo"
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
	toCompare chan<- DocWithTS,
	toDst chan<- []DocID,
) error {
	defer close(toCompare)
	defer close(toDst)

	lo.Assertf(
		task.QueryFilter.Partition.Natural,
		"natural partition required",
	)

	lowerBoundRV := task.QueryFilter.Partition.Key.Lower

	upperRecordID := task.QueryFilter.Partition.Upper

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
		// The server allows this, thankfully.
		cmd := bson.D{
			{"find", coll.Name()},
			{"hint", bson.D{{"$natural", 1}}},
			{"showRecordId", true},
			{"$_requestResumeToken", true},
			{"filter", docFilter.OrElse(bson.D{})},
			{"projection", bson.D{
				{"_id", 0},
				{"doc", docProjection},
			}},
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
			Stringer("resumeToken", resumeToken).
			Err(err).
			Msg("Resume token is no longer valid. Will attempt use of earlier tokens.")

		// NB: These are in descending order.
		priorResumeTokens, err := tasks.FetchPriorResumeTokens(
			ctx,
			task.QueryFilter.Namespace,
			startRecordID.MustGet(),
			tasksColl,
		)
		if err != nil {
			return errors.Wrapf(err, "fetching resume tokens after a key-not-found error")
		}

		failedTokens := 1

		for _, priorResumeToken := range priorResumeTokens {
			cmd := createCmd(option.Some(bsontools.ToRawValue(priorResumeToken)))

			cursor, err = coll.Database().RunCommandCursor(sctx, cmd)
			if err == nil {
				logger.Info().
					Any("task", task.PrimaryKey).
					Str("srcNamespace", task.QueryFilter.Namespace).
					Int("skippedPartitions", failedTokens).
					Msg("Due to a document deletion on the source cluster, this task has to read other tasks’ documents. This task may take longer to complete than others.")

				break
			}

			if !mmongo.ErrorHasCode(err, util.KeyNotFoundErrCode) {
				return errors.Wrapf(err, "opening source cursor after replacing resume token")
			}

			failedTokens++
		}

		if failedTokens > len(priorResumeTokens) {
			logger.Info().
				Any("task", task.PrimaryKey).
				Str("srcNamespace", task.QueryFilter.Namespace).
				Int("skippedPartitions", failedTokens).
				Msg("Due to a document deletion on the source cluster, this task has to read the entire collection. This task may take longer to complete than others.")

			cmd := createCmd(option.None[bson.RawValue]())

			cursor, err = coll.Database().RunCommandCursor(sctx, cmd)
			if err != nil {
				return errors.Wrapf(err, "opening source cursor from beginning")
			}
		}
	}

	defer cursor.Close(ctx)

	retryState.NoteSuccess("opened cursor")

	var batch []DocWithTS
	var batchDocIDs []DocID

	flush := func(ctx context.Context) error {
		logger.Trace().
			Any("task", task.PrimaryKey).
			Int("count", len(batchDocIDs)).
			Msg("Flushing to dst.")

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
			Int("count", len(batch)).
			Msg("Flushing to compare.")

		// Now send documents (one by one) to the comparison thread.
		for d, docAndTS := range batch {
			err := chanutil.WriteWithDoneCheck(
				ctx,
				toCompare,
				docAndTS,
			)
			if err != nil {
				// NB: This leaks memory, but that shouldn’t matter because
				// this error should crash the verifier.
				return errors.Wrapf(err, "sending src doc %d of %d to compare", 1+d, len(batch))
			}

			retryState.NoteSuccess("sent doc #%d of %d to compare thread", 1+d, len(batch))
		}

		logger.Trace().
			Any("task", task.PrimaryKey).
			Int("count", len(batch)).
			Msg("Done flushing to compare.")

		retryState.NoteSuccess("sent %d docs to compare", len(batch))

		batch = batch[:0]

		return nil
	}

cursorLoop:
	for {
		if !cursor.Next(ctx) {
			if cursor.Err() != nil {
				return errors.Wrapf(err, "reading documents")
			}

			break
		}

		opTime := sess.OperationTime()
		if opTime == nil {
			panic("session should have optime after reading a document")
		}

		recIDRV, err := cursor.Current.LookupErr(partitions.RecordID)
		if err != nil {
			return errors.Wrapf(err, "getting record ID")
		}

		if startID, has := startRecordID.Get(); has {
			val, err := compareRawValues(recIDRV, startID)
			if err != nil {
				return errors.Wrap(err, "comparing current record ID with start")
			}

			if val < 0 {
				continue cursorLoop
			}
		}

		if rvIsNonEmpty(upperRecordID) {
			val, err := compareRawValues(recIDRV, upperRecordID)
			if err != nil {
				return errors.Wrap(err, "comparing record ID with partition’s upper bound")
			}

			if val > 0 {
				break cursorLoop
			}
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

		batch = append(batch, NewDocWithTS(userDoc, *opTime))

		docID, err := compareMethod.RawDocIDForComparison(
			userDoc,
		)
		if err != nil {
			return errors.Wrapf(err, "parsing doc ID for comparison")
		}

		batchDocIDs = append(batchDocIDs, NewDocID(docID))

		if cursor.RemainingBatchLength() == 0 {
			if err := flush(ctx); err != nil {
				return errors.Wrapf(err, "flushing docs")
			}
		}
	}

	if len(batch) > 0 {
		if err := flush(ctx); err != nil {
			return errors.Wrapf(err, "flushing final docs")
		}
	}

	return nil
}

func compareRawValues(a, b bson.RawValue) (int, error) {
	if a.Type != b.Type {
		return 0, fmt.Errorf("can’t compare BSON %s against %s", a.Type, b.Type)
	}

	switch a.Type {
	case bson.TypeInt64:
		aI64, err := bsontools.RawValueTo[int64](a)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s)", a.Type, a)
		}

		bI64, err := bsontools.RawValueTo[int64](b)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s)", b.Type, b)
		}

		return cmp.Compare(aI64, bI64), nil
	case bson.TypeBinary:
		aBin, err := bsontools.RawValueTo[bson.Binary](a)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s)", a.Type, a)
		}

		bBin, err := bsontools.RawValueTo[bson.Binary](b)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s)", b.Type, b)
		}

		if aBin.Subtype != bBin.Subtype {
			return 0, errors.Wrapf(
				err,
				"cannot compare BSON binary subtype %d against %d",
				aBin.Subtype,
				bBin.Subtype,
			)
		}

		return bytes.Compare(aBin.Data, bBin.Data), nil
	default:
		return 0, fmt.Errorf("can’t compare BSON %s (other type: %s)", a.Type, b.Type)
	}
}
