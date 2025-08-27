package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	recheckQueueCollectionNameBase = "recheckQueue"
)

// RecheckPrimaryKey stores the implicit type of recheck to perform
// Currently, we only handle document mismatches/change stream updates,
// so SrcDatabaseName, SrcCollectionName, and DocumentID must always be specified.
//
// NB: Order is important here so that, within a given generation,
// sorting by _id will guarantee that all rechecks for a given
// namespace appear consecutively.
type RecheckPrimaryKey struct {
	SrcDatabaseName   string `bson:"db"`
	SrcCollectionName string `bson:"coll"`
	DocumentID        any    `bson:"docID"`
}

// RecheckDoc stores the necessary information to know which documents must be rechecked.
type RecheckDoc struct {
	PrimaryKey RecheckPrimaryKey `bson:"_id"`

	// NB: Because we don’t update the recheck queue’s documents, this field
	// and any others that may be added will remain unchanged even if a recheck
	// is enqueued multiple times for the same document in the same generation.
	DataSize int `bson:"dataSize"`
}

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	ctx context.Context,
	namespace string, documentIDs []any, dataSizes []int) error {
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
	documentIDs []any,
	dataSizes []int,
) error {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation, _ := verifier.getGenerationWhileLocked()

	docIDIndexes := lo.Range(len(documentIDs))
	indexesPerThread := splitToChunks(docIDIndexes, verifier.numWorkers)

	eg, groupCtx := contextplus.ErrGroup(ctx)

	genCollection := verifier.getRecheckQueueCollection(generation)

	for _, curThreadIndexes := range indexesPerThread {
		eg.Go(func() error {
			models := make([]mongo.WriteModel, len(curThreadIndexes))
			for m, i := range curThreadIndexes {
				recheckDoc := RecheckDoc{
					PrimaryKey: RecheckPrimaryKey{
						SrcDatabaseName:   dbNames[i],
						SrcCollectionName: collNames[i],
						DocumentID:        documentIDs[i],
					},
					DataSize: dataSizes[i],
				}

				models[m] = mongo.NewInsertOneModel().
					SetDocument(recheckDoc)
			}

			retryer := retry.New()
			err := retryer.WithCallback(
				func(retryCtx context.Context, _ *retry.FuncInfo) error {
					_, err := genCollection.BulkWrite(
						retryCtx,
						models,
						options.BulkWrite().SetOrdered(false),
					)

					// We expect duplicate-key errors from the above because:
					//
					// a) The same document can change multiple times per generation.
					// b) The source’s changes also happen on the destination, and both
					//    change streams populate this recheck queue.
					//
					// Because of that, we ignore duplicate-key errors. We do *not*, though,
					// ignore other errors that may accompany duplicate-key errors in the
					// same server response.
					//
					// This does mean that the persisted DataSize _does not change_ after
					// a document is first inserted. That should matter little, though,
					// the persisted document size is just an optimization for parallelizing,
					// and document sizes probably remain stable(-ish) across updates.
					err = util.TolerateSimpleDuplicateKeyInBulk(
						verifier.logger,
						len(models),
						err,
					)

					return err
				},
				"persisting %d recheck(s)",
				len(models),
			).Run(groupCtx, verifier.logger)

			return errors.Wrapf(err, "failed to persist %d recheck(s) for generation %d", len(models), generation)
		})
	}

	if err := eg.Wait(); err != nil {
		return errors.Wrapf(
			err,
			"failed to persist %d recheck(s) for generation %d",
			len(documentIDs),
			generation,
		)
	}

	verifier.logger.Trace().
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

	genCollection := verifier.getRecheckQueueCollection(prevGeneration)

	return retry.New().WithCallback(
		func(ctx context.Context, i *retry.FuncInfo) error {
			return genCollection.Drop(ctx)
		},
		"deleting generation %d's enqueued rechecks",
		prevGeneration,
	).Run(ctx, verifier.logger)
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

	verifier.logger.Debug().
		Int("priorGeneration", prevGeneration).
		Msgf("Counting prior generation’s enqueued rechecks.")

	recheckColl := verifier.getRecheckQueueCollection(prevGeneration)

	rechecksCount, err := recheckColl.CountDocuments(ctx, bson.D{})
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
	var idAccum []any
	var idsSizer util.BSONArraySizer
	var totalDocs types.DocumentCount
	var dataSizeAccum, totalRecheckData int64

	maxDocsPerTask := max(
		int(rechecksCount)/verifier.numWorkers,
		verifier.numWorkers,
	)

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

	persistBufferedRechecks := func() error {
		if len(idAccum) == 0 {
			return nil
		}

		namespace := prevDBName + "." + prevCollName

		task, err := verifier.InsertDocumentRecheckTask(
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
			Any("task", task.PrimaryKey).
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

		idRaw, err := cursor.Current.LookupErr("_id", "docID")
		if err != nil {
			return errors.Wrapf(err, "failed to find docID in enqueued recheck %v", cursor.Current)
		}

		// We persist rechecks if any of these happen:
		// - the namespace has changed
		// - we’ve reached the per-task recheck maximum
		// - the buffered document IDs’ size exceeds the per-task maximum
		// - the buffered documents exceed the partition size
		//
		if doc.PrimaryKey.SrcDatabaseName != prevDBName ||
			doc.PrimaryKey.SrcCollectionName != prevCollName ||
			len(idAccum) > maxDocsPerTask ||
			types.ByteCount(idsSizer.Len()) >= verifier.recheckMaxSizeInBytes ||
			dataSizeAccum >= verifier.partitionSizeInBytes {

			err := persistBufferedRechecks()
			if err != nil {
				return err
			}

			prevDBName = doc.PrimaryKey.SrcDatabaseName
			prevCollName = doc.PrimaryKey.SrcCollectionName
			idsSizer = util.BSONArraySizer{}
			dataSizeAccum = 0
			idAccum = idAccum[:0]
		}

		idsSizer.Add(idRaw)
		dataSizeAccum += int64(doc.DataSize)
		idAccum = append(idAccum, doc.PrimaryKey.DocumentID)

		totalRecheckData += int64(doc.DataSize)
		totalDocs++
	}

	err = cursor.Err()
	if err != nil {
		return err
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

func (v *Verifier) getRecheckQueueCollection(generation int) *mongo.Collection {
	return v.verificationDatabase().
		Collection(fmt.Sprintf("%s_gen%d", recheckQueueCollectionNameBase, generation))
}
