package verifier

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	recheckBatchByteLimit  = 1024 * 1024
	recheckBatchCountLimit = 1000

	recheckQueueCollectionNameBase = "recheckQueue"
)

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	ctx context.Context,
	namespace string, documentIDs []bson.RawValue, dataSizes []int32) error {
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

	return verifier.insertRecheckDocs(ctx, dbNames, collNames, documentIDs, dataSizes)
}

func (verifier *Verifier) insertRecheckDocs(
	ctx context.Context,
	dbNames []string,
	collNames []string,
	documentIDs []bson.RawValue,
	dataSizes []int32,
) error {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation, _ := verifier.getGenerationWhileLocked()

	eg, groupCtx := contextplus.ErrGroup(ctx)

	// MongoDB’s Go driver starts failing requests if we try to exceed
	// its connection pool’s size. To avoid that, we limit our concurrency.
	eg.SetLimit(100)

	genCollection := verifier.getRecheckQueueCollection(generation)

	start := time.Now()
	insertThreads := 0

	sendRechecks := func(rechecks []bson.Raw) {
		insertThreads++

		eg.Go(func() error {

			retryer := retry.New()
			err := retryer.WithCallback(
				func(retryCtx context.Context, _ *retry.FuncInfo) error {
					requestBSON := buildRequestBSON(
						genCollection.Name(),
						rechecks,
					)

					// The driver’s InsertMany method inspects each document
					// to ensure that it has an _id. We can avoid that extra
					// overhead by calling RunCommand instead.
					err := genCollection.Database().RunCommand(
						retryCtx,
						requestBSON,
					).Err()

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
						len(rechecks),
						err,
					)

					return err
				},
				"persisting %d recheck(s)",
				len(rechecks),
			).Run(groupCtx, verifier.logger)

			return errors.Wrapf(err, "batch of %d rechecks", len(rechecks))
		})
	}

	curRechecks := make([]bson.Raw, 0, recheckBatchCountLimit)
	curBatchBytes := 0
	for i, dbName := range dbNames {
		recheckDoc := recheck.Doc{
			PrimaryKey: recheck.PrimaryKey{
				SrcDatabaseName:   dbName,
				SrcCollectionName: collNames[i],
				DocumentID:        documentIDs[i],
				Rand:              rand.Int32(),
			},
			DataSize: dataSizes[i],
		}

		recheckRaw, err := recheckDoc.MarshalToBSON()
		if err != nil {
			return errors.Wrapf(err, "marshaling recheck for %#q", dbName+"."+collNames[i])
		}

		curRechecks = append(
			curRechecks,
			bson.Raw(recheckRaw),
		)

		curBatchBytes += len(recheckRaw)
		if curBatchBytes > recheckBatchByteLimit || len(curRechecks) >= recheckBatchCountLimit {
			sendRechecks(curRechecks)
			curRechecks = make([]bson.Raw, 0, recheckBatchCountLimit)
			curBatchBytes = 0
		}
	}

	if len(curRechecks) > 0 {
		sendRechecks(curRechecks)
	}

	if err := eg.Wait(); err != nil {
		return errors.Wrapf(
			err,
			"persisting %d recheck(s) for generation %d",
			len(documentIDs),
			generation,
		)
	}

	if time.Since(start) > time.Second {
		verifier.logger.Warn().
			Int("count", len(documentIDs)).
			Int("insertThreads", insertThreads).
			Stringer("totalTime", time.Since(start)).
			Msg("Slow recheck persistence.")
	}

	verifier.logger.Trace().
		Int("generation", generation).
		Int("count", len(documentIDs)).
		Msg("Persisted rechecks.")

	return nil
}

func buildRequestBSON(collName string, rechecks []bson.Raw) bson.Raw {
	rechecksBSONSize := mbson.GetBSONArraySize(rechecks)

	rechecksBSON := make(bson.RawArray, 4, rechecksBSONSize)
	binary.LittleEndian.PutUint32(rechecksBSON, uint32(rechecksBSONSize))
	for i, recheck := range rechecks {
		rechecksBSON = bsoncore.AppendDocumentElement(
			rechecksBSON,
			strconv.Itoa(i),
			recheck,
		)
	}

	// final NUL
	rechecksBSON = append(rechecksBSON, 0)

	if len(rechecksBSON) != rechecksBSONSize {
		panic(fmt.Sprintf("rechecks BSON doc size (%d) != expected (%d)", len(rechecksBSON), rechecksBSONSize))
	}

	// This BSON doc takes 39 bytes besides the collection name and requests.
	expectedBSONSize := 39 + len(collName) + rechecksBSONSize
	requestBSON := make(bson.Raw, 4, expectedBSONSize)

	requestBSON = bsoncore.AppendStringElement(
		requestBSON,
		"insert",
		collName,
	)
	requestBSON = bsoncore.AppendBooleanElement(
		requestBSON,
		"ordered",
		false,
	)
	requestBSON = bsoncore.AppendArrayElement(
		requestBSON,
		"documents",
		rechecksBSON,
	)
	requestBSON = append(requestBSON, 0)

	if len(requestBSON) != expectedBSONSize {
		panic(fmt.Sprintf("request BSON size (%d) mismatches expected (%d)", len(requestBSON), expectedBSONSize))
	}

	binary.LittleEndian.PutUint32(requestBSON, uint32(len(requestBSON)))

	return requestBSON
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

	startTime := time.Now()

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

	// The sort here is important because the recheck _id is an embedded
	// document that includes the namespace. Thus, all rechecks for a given
	// namespace will be consecutive in this query’s result.
	cursor, err := recheckColl.Find(
		ctx,
		bson.D{},
		options.Find().
			SetSort(bson.D{{"_id", 1}}).
			SetProjection(bson.D{
				{"_id.rand", 0},
			}),
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

	var lastIDRaw bson.RawValue

	// We group these here using a sort rather than using aggregate because aggregate is
	// subject to a 16MB limit on group size.
	for cursor.Next(ctx) {
		var doc recheck.Doc
		err = (&doc).UnmarshalFromBSON(cursor.Current)
		if err != nil {
			return err
		}

		idRaw := doc.PrimaryKey.DocumentID

		// We persist rechecks if any of these happen:
		// - the namespace has changed
		// - we’ve reached the per-task recheck maximum
		// - the buffered document IDs’ size exceeds the per-task maximum
		// - the buffered documents exceed the partition size
		//
		if doc.PrimaryKey.SrcDatabaseName != prevDBName ||
			doc.PrimaryKey.SrcCollectionName != prevCollName ||
			len(idAccum) > maxRecheckIDs ||
			types.ByteCount(idsSizer.Len()) >= maxRecheckIDsBytes ||
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
			lastIDRaw = bson.RawValue{}
		}

		// We’re iterating the rechecks in order such that, if the same doc
		// gets enqueued from multiple sources, we’ll see those records
		// consecutively. We can deduplicate here, then, by checking to see if
		// the doc ID has changed. (NB: At this point we know the namespace
		// has *not* changed because we just checked for that.)
		if idRaw.Equal(lastIDRaw) {
			continue
		}

		lastIDRaw = idRaw

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
			Stringer("timeElapsed", time.Since(startTime)).
			Msg("Scheduled documents for recheck in the new generation.")
	}

	return err
}

func (v *Verifier) getRecheckQueueCollection(generation int) *mongo.Collection {
	return v.verificationDatabase().
		Collection(fmt.Sprintf("%s_gen%d", recheckQueueCollectionNameBase, generation))
}
