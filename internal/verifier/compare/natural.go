package compare

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/logger"
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
	client *mongo.Client,
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
		"partition must indicate natural scan but doesn’t (task: %+v)",
		task,
	)

	upperRecordID := task.QueryFilter.Partition.Upper

	sess, err := client.StartSession()
	if err != nil {
		return errors.Wrapf(err, "starting session")
	}
	defer sess.EndSession(ctx)

	sctx := mongo.NewSessionContext(ctx, sess)

	db, collName := mmongo.SplitNamespace(task.QueryFilter.Namespace)
	coll := client.Database(db).Collection(collName)

	var cursor *mongo.Cursor

	var resumeTokenOpt option.Option[bson.RawValue]

	lowerBound := task.QueryFilter.Partition.Key.Lower

	// To simplify testing we interpret empty RawValue here as equivalent
	// to BSON null.
	if lowerBound.Type != bson.TypeNull && !lowerBound.IsZero() {
		resumeTokenOpt = option.Some(lowerBound)
	}

	var canUseStartAt bool

	if resumeTokenOpt.IsSome() {
		version, err := mmongo.GetVersionArray(ctx, client)
		if err != nil {
			return errors.Wrapf(err, "fetching server version")
		}

		canUseStartAt = mmongo.FindCanUseStartAt(version)
	}

	for {
		cmd := bson.D{
			{"find", coll.Name()},
			{"hint", bson.D{{"$natural", 1}}},
			{"showRecordId", true},
			{"$_requestResumeToken", true},
		}

		if filter, has := docFilter.Get(); has {
			cmd = append(cmd, bson.E{"filter", filter})
		}

		var docProjection any

		switch compareMethod {
		case ToHashedIndexKey:
			docProjection = GetHashedIndexKeyProjection(task.QueryFilter)
		case Binary, IgnoreOrder:
			docProjection = "$$ROOT"
		}

		cmd = append(cmd, bson.E{"projection", bson.D{
			{"_id", 0},
			{"_", docProjection},
		}})

		if startAt, has := resumeTokenOpt.Get(); has {
			cmd = append(cmd, bson.E{
				lo.Ternary(canUseStartAt, "$_startAt", "$_resumeAfter"),
				startAt,
			})
		}

		cursor, err = coll.Database().RunCommandCursor(sctx, cmd)
		if err != nil {
			if !mmongo.ErrorHasCode(err, util.KeyNotFoundErrCode) {
				return errors.Wrapf(err, "reading from source")
			}

			startAt := resumeTokenOpt.MustGet()
			raw := lo.Must(bsontools.RawValueTo[bson.Raw](startAt))

			raw = slices.Clone(raw)
			recIDRV := lo.Must(raw.LookupErr("$recordId"))
			switch recIDRV.Type {
			case bson.TypeInt64:
				recID := recIDRV.Int64()

				if recID == 1 {
					resumeTokenOpt = option.None[bson.RawValue]()
				} else {
					replaced, err := bsontools.ReplaceInRaw(
						&raw,
						bsontools.ToRawValue(recID-1),
						"$recordId",
					)
					lo.Assertf(
						replaced,
						"should update record ID in token (%+v)",
						raw,
					)
					if err != nil {
						return errors.Wrapf(err, "incrementing record ID in resume token (%v)", raw)
					}

					resumeTokenOpt = option.Some(bsontools.ToRawValue(raw))
				}

				continue
			default:
				return fmt.Errorf("fatal error: received key-not-found error (%v) but cannot increment record ID (%s) because of its BSON type (%s)", err, recIDRV.String(), recIDRV.Type)
			}
		}

		break
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

		recIDRV, err := cursor.Current.LookupErr("$recordId")
		if err != nil {
			return errors.Wrapf(err, "getting record ID")
		}

		switch upperRecordID.Type {

		// To simplify testing we interpret empty RawValue here as equivalent
		// to BSON null.
		case 0, bson.TypeNull:
			// No upper bound.
		case bson.TypeInt64:
			// A non-clustered collection:
			recID, err := bsontools.RawValueTo[int64](recIDRV)
			if err != nil {
				return errors.Wrapf(err, "parsing record ID")
			}

			bound, err := bsontools.RawValueTo[int64](upperRecordID)
			if err != nil {
				return errors.Wrapf(err, "parsing upper bound")
			}

			if recID > bound {
				break cursorLoop
			}
		case bson.TypeBinary:
			// A clustered collection:
			recID, err := bsontools.RawValueTo[bson.Binary](recIDRV)
			if err != nil {
				return errors.Wrapf(err, "parsing record ID")
			}

			bound, err := bsontools.RawValueTo[bson.Binary](upperRecordID)
			if err != nil {
				return errors.Wrapf(err, "parsing upper bound")
			}

			if recID.Subtype != bound.Subtype {
				return errors.Wrapf(
					err,
					"upper bound subtype is %d but record ID subtype is %d",
					bound.Subtype,
					recID.Subtype,
				)
			}

			if bytes.Compare(recID.Data, bound.Data) > 0 {
				break cursorLoop
			}
		default:
			panic(fmt.Sprintf("bad upper bound (%v) BSON type: %s", upperRecordID, upperRecordID.Type))
		}

		userDocRV, err := cursor.Current.LookupErr("_")
		if err != nil {
			return errors.Wrapf(err, "getting user document")
		}

		userDoc, err := bsontools.RawValueTo[bson.Raw](userDocRV)
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
