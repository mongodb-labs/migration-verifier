package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/sync/errgroup"
)

const (
	recheckQueue                  = "recheckQueue"
	maxBSONObjSize                = 16 * 1024 * 1024
	recheckInserterThreadsSoftMax = 100
	maxIdsPerRecheckTask          = 12 * 1024 * 1024
)

// RecheckPrimaryKey stores the implicit type of recheck to perform
// Currently, we only handle document mismatches/change stream updates,
// so DatabaseName, CollectionName, and DocumentID must always be specified.
//
// NB: Order is important here so that, within a given generation,
// sorting by _id will guarantee that all rechecks for a given
// namespace appear consecutively.
type RecheckPrimaryKey struct {
	Generation     int         `bson:"generation"`
	DatabaseName   string      `bson:"db"`
	CollectionName string      `bson:"coll"`
	DocumentID     interface{} `bson:"docID"`
}

// RecheckDoc stores the necessary information to know which documents must be rechecked.
type RecheckDoc struct {
	PrimaryKey RecheckPrimaryKey `bson:"_id"`
	DataSize   int               `bson:"dataSize"`
}

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	ctx context.Context,
	namespace string, documentIDs []interface{}, dataSizes []int) error {
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
		ctx,
		dbNames,
		collNames,
		documentIDs,
		dataSizes,
	)
}

// This will split the given slice into *roughly* the given number of chunks.
// It may end up being more or fewer, but it should be pretty close.
func splitToChunks[T any, Slice ~[]T](elements Slice, numChunks int) []Slice {
	if numChunks < 1 {
		panic(fmt.Sprintf("numChunks (%v) should be >=1", numChunks))
	}

	elsPerChunk := len(elements) / numChunks

	if elsPerChunk == 0 {
		elsPerChunk = 1
	}

	return lo.Chunk(elements, elsPerChunk)
}

func (verifier *Verifier) insertRecheckDocs(
	ctx context.Context,
	dbNames []string,
	collNames []string,
	documentIDs []interface{},
	dataSizes []int,
) error {
	verifier.mux.Lock()
	defer verifier.mux.Unlock()

	generation, _ := verifier.getGenerationWhileLocked()

	docIDIndexes := lo.Range(len(documentIDs))
	indexesPerThread := splitToChunks(docIDIndexes, recheckInserterThreadsSoftMax)

	eg, groupCtx := errgroup.WithContext(ctx)

	for _, curThreadIndexes := range indexesPerThread {
		curThreadIndexes := curThreadIndexes

		eg.Go(func() error {
			models := make([]mongo.WriteModel, len(curThreadIndexes))
			for m, i := range curThreadIndexes {
				pk := RecheckPrimaryKey{
					Generation:     generation,
					DatabaseName:   dbNames[i],
					CollectionName: collNames[i],
					DocumentID:     documentIDs[i],
				}

				// The filter must exclude DataSize; otherwise, if a failed comparison
				// and a change event happen on the same document for the same
				// generation, the 2nd insert will fail because a) its filter won’t
				// match anything, and b) it’ll try to insert a new document with the
				// same _id as the one that the 1st insert already created.
				filterDoc := bson.D{{"_id", pk}}

				recheckDoc := RecheckDoc{
					PrimaryKey: pk,
					DataSize:   dataSizes[i],
				}

				models[m] = mongo.NewReplaceOneModel().
					SetFilter(filterDoc).
					SetReplacement(recheckDoc).
					SetUpsert(true)
			}

			retryer := retry.New(retry.DefaultDurationLimit)
			err := retryer.Run(
				groupCtx,
				verifier.logger,
				func(retryCtx context.Context, _ *retry.FuncInfo) error {
					_, err := verifier.verificationDatabase().Collection(recheckQueue).BulkWrite(
						retryCtx,
						models,
						options.BulkWrite().SetOrdered(false),
					)

					return err
				},
			)

			return errors.Wrapf(err, "failed to persist %d recheck(s) for generation %d", len(models), generation)
		})
	}

	err := eg.Wait()

	if err != nil {
		return errors.Wrapf(
			err,
			"failed to persist %d recheck(s) for generation %d",
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

// ClearRecheckDocs deletes the previous generation’s recheck
// documents from the verifier’s metadata.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) ClearRecheckDocs(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	verifier.logger.Debug().Msgf("Deleting generation %d’s %s documents", prevGeneration, recheckQueue)

	_, err := verifier.verificationDatabase().Collection(recheckQueue).DeleteMany(
		ctx, bson.D{{"_id.generation", prevGeneration}})
	return err
}

func (verifier *Verifier) getPreviousGenerationWhileLocked() int {
	generation, _ := verifier.getGenerationWhileLocked()
	if generation < 1 {
		panic("This function is forbidden before generation 1!")
	}

	return generation - 1
}

// GenerateRecheckTasks fetches the previous generation’s recheck
// documents from the verifier’s metadata and creates current-generation
// document-verification tasks from them.
//
// The verifier **MUST** be locked when this function is called (or panic).
func (verifier *Verifier) GenerateRecheckTasks(ctx context.Context) error {
	prevGeneration := verifier.getPreviousGenerationWhileLocked()

	findFilter := bson.D{{"_id.generation", prevGeneration}}

	verifier.logger.Debug().
		Int("priorGeneration", prevGeneration).
		Msgf("Counting prior generation’s enqueued rechecks.")

	recheckColl := verifier.verificationDatabase().Collection(recheckQueue)
	rechecksCount, err := recheckColl.CountDocuments(ctx, findFilter)
	if err != nil {
		return errors.Wrapf(err,
			"failed to count generation %d’s rechecks",
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
	var idAccum []interface{}
	var idLenAccum int
	var dataSizeAccum int64

	maxDocsPerTask := rechecksCount / int64(verifier.numWorkers)

	if maxDocsPerTask < int64(verifier.numWorkers) {
		maxDocsPerTask = int64(verifier.numWorkers)
	}

	// The sort here is important because the recheck _id is an embedded
	// document that includes the namespace. Thus, all rechecks for a given
	// namespace will be consecutive in this query’s result.
	cursor, err := recheckColl.Find(
		ctx,
		findFilter,
		options.Find().SetSort(bson.D{{"_id", 1}}),
	)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	persistBufferedRechecks := func() error {
		if len(idAccum) == 0 {
			return nil
		}

		namespace := prevDBName + "." + prevCollName

		err := verifier.InsertDocumentRecheckTask(
			ctx,
			idAccum,
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
			Str("namespace", namespace).
			Int("numDocuments", len(idAccum)).
			Str("dataSize", reportutils.FmtBytes(dataSizeAccum)).
			Msg("Created document recheck task.")

		return nil
	}

	// We group these here using a sort rather than using aggregate because aggregate is
	// subject to a 16MB limit on group size.
	for cursor.Next(ctx) {
		var doc RecheckDoc
		err = cursor.Decode(&doc)
		if err != nil {
			return err
		}

		idRaw := cursor.Current.Lookup("_id", "docID")
		idLen := len(idRaw.Value)

		// We persist rechecks if any of these happen:
		// - the namespace has changed
		// - we’ve reached the per-task recheck maximum
		// - the buffered document IDs’ size exceeds the per-task maximum
		// - the buffered documents exceed the partition size
		//
		if doc.PrimaryKey.DatabaseName != prevDBName ||
			doc.PrimaryKey.CollectionName != prevCollName ||
			int64(len(idAccum)) > maxDocsPerTask ||
			idLenAccum >= maxIdsPerRecheckTask ||
			dataSizeAccum >= verifier.partitionSizeInBytes {

			err := persistBufferedRechecks()
			if err != nil {
				return err
			}

			prevDBName = doc.PrimaryKey.DatabaseName
			prevCollName = doc.PrimaryKey.CollectionName
			idLenAccum = 0
			dataSizeAccum = 0
			idAccum = idAccum[:0]
		}

		idLenAccum += idLen
		dataSizeAccum += int64(doc.DataSize)
		idAccum = append(idAccum, doc.PrimaryKey.DocumentID)
	}

	err = cursor.Err()
	if err != nil {
		return err
	}

	return persistBufferedRechecks()
}
