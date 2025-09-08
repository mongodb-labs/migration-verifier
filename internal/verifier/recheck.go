package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
)

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	namespace string,
	documentIDs []any,
	dataSizes []int,
) error {
	dbName, collName := SplitNamespace(namespace)

	dbNames := make([]string, len(documentIDs))
	collNames := make([]string, len(documentIDs))
	for i := range documentIDs {
		dbNames[i] = dbName
		collNames[i] = collName
	}

	verifier.logger.Debug().
		Int("count", len(documentIDs)).
		Msg("Persisting rechecks for mismatched or missing documents.")

	return verifier.insertRecheckDocs(
		dbNames,
		collNames,
		documentIDs,
		dataSizes,
	)
}

func (verifier *Verifier) insertRecheckDocs(
	dbNames []string,
	collNames []string,
	documentIDs []any,
	dataSizes []int,
) error {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation, _ := verifier.getGenerationWhileLocked()

	err := verifier.localDB.InsertRechecks(
		generation,
		dbNames,
		collNames,
		documentIDs,
		dataSizes,
	)

	if err != nil {
		return errors.Wrapf(
			err,
			"persisting %d recheck(s) for generation %d",
			len(documentIDs),
			generation,
		)
	}

	verifier.logger.Debug().
		Int("generation", generation).
		Int("count", len(documentIDs)).
		Msg("Persisted rechecks.")

	return nil
}

// DropOldRecheckQueueWhileLocked deletes the previous generation’s recheck
// documents from the verifier’s metadata.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) DropOldRecheckQueueWhileLocked(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	verifier.logger.Debug().
		Int("previousGeneration", prevGeneration).
		Msg("Deleting previous generation's enqueued rechecks.")

	/*
		genCollection := verifier.getRecheckQueueCollection(prevGeneration)

		return retry.New().WithCallback(
			func(ctx context.Context, i *retry.FuncInfo) error {
				return genCollection.Drop(ctx)
			},
			"deleting generation %d's enqueued rechecks",
			prevGeneration,
		).Run(ctx, verifier.logger)
	*/

	return verifier.localDB.ClearAllRechecksForGeneration(prevGeneration)
}

func (verifier *Verifier) getPreviousGenerationWhileLocked() int {
	generation, _ := verifier.getGenerationWhileLocked()
	if generation < 1 {
		panic("This function is forbidden before generation 1!")
	}

	return generation - 1
}

// GenerateRecheckTasksWhileLocked fetches the previous generation’s recheck
// documents from the verifier’s metadata and creates current-generation
// document-verification tasks from them.
//
// Note that this function DOES NOT retry on failure, so callers should wrap
// calls to this function in a retryer.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) GenerateRecheckTasksWhileLocked(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	rechecksCount, err := verifier.localDB.GetRechecksCount(prevGeneration)
	if err != nil {
		return errors.Wrapf(err,
			"counting generation %d’s rechecks",
			prevGeneration,
		)
	}

	verifier.logger.Debug().
		Int("priorGeneration", prevGeneration).
		Int("rechecksCount", int(rechecksCount)).
		Msgf("Creating recheck tasks from prior generation’s enqueued rechecks.")

	// We generate one recheck task per collection, unless
	// 1) The size of the list of IDs would exceed 12MB (a very conservative way of avoiding
	//    the 16MB BSON limit)
	// 2) The size of the data would exceed our desired partition size.  This limits memory use
	//    during the recheck phase.
	// 3) The number of documents exceeds $rechecksCount/$numWorkers. We do
	//    this to prevent one thread from doing all of the rechecks.

	var prevDBName, prevCollName string
	var idAccum []bson.RawValue
	var idsSizer util.BSONArraySizer
	var totalDocs types.DocumentCount
	var dataSizeAccum, totalRecheckData int64

	maxDocsPerTask := max(
		int(rechecksCount)/verifier.numWorkers,
		verifier.numWorkers,
	)

	/*
		// The sort here is important because the recheck _id is an embedded
		// document that includes the namespace. Thus, all rechecks for a given
		// namespace will be consecutive in this query’s result.
		cursor, err := recheckColl.Find(
			ctx,
			bson.D{},
			options.Find().SetSort(bson.D{{"_id", 1}}),
		)
		if err != nil {
			return err
		}
		defer cursor.Close(ctx)
	*/

	persistBufferedRechecks := func() error {
		if len(idAccum) == 0 {
			return nil
		}

		namespace := prevDBName + "." + prevCollName

		task, err := verifier.InsertDocumentRecheckTask(
			ctx,
			lo.ToAnySlice(idAccum),
			types.ByteCount(dataSizeAccum),
			namespace,
		)
		if err != nil {
			return errors.Wrapf(
				err,
				"failed to create a %d-document recheck task for collection %#q",
				len(idAccum),
				namespace,
			)
		}

		verifier.logger.Debug().
			Any("task", task.PrimaryKey).
			Str("namespace", namespace).
			Int("numDocuments", len(idAccum)).
			Str("dataSize", reportutils.FmtBytes(dataSizeAccum)).
			Msg("Created document recheck task.")

		return nil
	}

	recheckCtx, recheckCancel := contextplus.WithCancelCause(ctx)
	recheckReader := verifier.localDB.GetRecheckReader(recheckCtx, prevGeneration)
	defer recheckCancel(fmt.Errorf("finished recheck"))

	// We group these here using a sort rather than using aggregate because aggregate is
	// subject to a 16MB limit on group size.
	for recheckResult := range recheckReader {
		recheck, err := recheckResult.Get()

		if err != nil {
			return errors.Wrap(err, "reading rechecks from local DB")
		}

		// We persist rechecks if any of these happen:
		// - the namespace has changed
		// - we’ve reached the per-task recheck maximum
		// - the buffered document IDs’ size exceeds the per-task maximum
		// - the buffered documents exceed the partition size
		//
		if recheck.DB != prevDBName ||
			recheck.Coll != prevCollName ||
			len(idAccum) > maxDocsPerTask ||
			types.ByteCount(idsSizer.Len()) >= verifier.recheckMaxSizeInBytes ||
			dataSizeAccum >= verifier.partitionSizeInBytes {

			err := persistBufferedRechecks()
			if err != nil {
				return err
			}

			prevDBName = recheck.DB
			prevCollName = recheck.Coll
			idsSizer = util.BSONArraySizer{}
			dataSizeAccum = 0
			idAccum = idAccum[:0]
		}

		idsSizer.Add(recheck.DocID)
		dataSizeAccum += int64(recheck.Size)
		idAccum = append(idAccum, recheck.DocID)

		totalRecheckData += int64(recheck.Size)
		totalDocs++
	}

	err = persistBufferedRechecks()

	if err == nil && totalDocs > 0 {
		verifier.logger.Info().
			Int("generation", 1+prevGeneration).
			Int64("totalDocs", int64(totalDocs)).
			Str("totalData", reportutils.FmtBytes(totalRecheckData)).
			Msg("Scheduled documents for recheck in the new generation.")
	}

	return err
}
