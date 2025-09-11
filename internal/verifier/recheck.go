package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/localdb"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
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
	documentIDsAny []any,
	dataSizes []int,
) error {

	if len(dbNames) != len(collNames) || len(collNames) != len(documentIDsAny) || len(documentIDsAny) != len(dataSizes) {
		panic(fmt.Sprintf(
			"Mismatched recheck slices!! dbNames=%d collNames=%d docIDs=%d sizes=%d",
			len(dbNames),
			len(collNames),
			len(documentIDsAny),
			len(dataSizes),
		))
	}

	var rawDocIDs []bson.RawValue
	dbNames, collNames, rawDocIDs, dataSizes = deduplicateRechecks(dbNames, collNames, documentIDsAny, dataSizes)

	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation, _ := verifier.getGenerationWhileLocked()

	rechecks := make([]localdb.Recheck, 0, len(dbNames))

	for i, dbName := range dbNames {
		rechecks = append(
			rechecks,
			localdb.Recheck{
				DB:    dbName,
				Coll:  collNames[i],
				DocID: rawDocIDs[i],
				Size:  types.ByteCount(dataSizes[i]),
			},
		)
	}

	start := time.Now()
	err := verifier.localDB.InsertRechecks(generation, rechecks)

	if err != nil {
		return errors.Wrapf(
			err,
			"persisting %d recheck(s) for generation %d",
			len(dbNames),
			generation,
		)
	}

	verifier.logger.Debug().
		Int("generation", generation).
		Int("count", len(rawDocIDs)).
		Stringer("timeElapsed", time.Since(start)).
		Msg("Persisted rechecks.")

	return nil
}

func deduplicateRechecks(
	dbNames, collNames []string,
	documentIDs []any,
	dataSizes []int,
) ([]string, []string, []bson.RawValue, []int) {
	dedupeMap := map[string]map[string]map[string]int{}

	uniqueElems := 0

	for i, dbName := range dbNames {
		collName := collNames[i]
		docID := documentIDs[i]
		dataSize := dataSizes[i]

		docIDRaw := mbson.MustConvertToRawValue(docID)

		docIDStr := string(append(
			[]byte{byte(docIDRaw.Type)},
			docIDRaw.Value...,
		))

		if _, ok := dedupeMap[dbName]; !ok {
			dedupeMap[dbName] = map[string]map[string]int{
				collName: {
					docIDStr: dataSize,
				},
			}

			uniqueElems++

			continue
		}

		if _, ok := dedupeMap[dbName][collName]; !ok {
			dedupeMap[dbName][collName] = map[string]int{
				docIDStr: dataSize,
			}

			uniqueElems++

			continue
		}

		if _, ok := dedupeMap[dbName][collName][docIDStr]; !ok {
			dedupeMap[dbName][collName][docIDStr] = dataSize
			uniqueElems++
		}
	}

	dbNames = make([]string, 0, uniqueElems)
	collNames = make([]string, 0, uniqueElems)
	rawDocIDs := make([]bson.RawValue, 0, uniqueElems)
	dataSizes = make([]int, 0, uniqueElems)

	for dbName, collMap := range dedupeMap {
		for collName, docMap := range collMap {
			for docIDStr, dataSize := range docMap {
				dbNames = append(dbNames, dbName)
				collNames = append(collNames, collName)
				rawDocIDs = append(
					rawDocIDs,
					bson.RawValue{
						Type:  []bsontype.Type(docIDStr)[0],
						Value: []byte(docIDStr)[1:],
					},
				)
				dataSizes = append(dataSizes, dataSize)
			}
		}
	}

	return dbNames, collNames, rawDocIDs, dataSizes
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
