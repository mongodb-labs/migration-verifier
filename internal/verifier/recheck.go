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
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
	"golang.org/x/exp/slices"
)

const (
	recheckBatchByteLimit  = 1024 * 1024
	recheckBatchCountLimit = 1000

	recheckQueueCollectionNameBase = "recheckQueue"

	maxTasksPerRequest = 500
	maxInsertSize      = 256 << 10

	// TODO: Try something beneath 256 KiB instead.
	maxRecheckIDsBytes = 1024 * 1024 // 1 MiB

	// The max # of docs that we want each recheck task’s cursor to return.
	maxRecheckIDs = 10_000
)

// InsertFailedCompareRecheckDocs is for inserting RecheckDocs based on failures during Check.
func (verifier *Verifier) InsertFailedCompareRecheckDocs(
	ctx context.Context,
	namespace string,
	documentIDs []bson.RawValue,
	dataSizes []int32,
	firstMismatchTimes []bson.DateTime,
) error {
	if firstMismatchTimes == nil {
		panic("mismatch recheck must have first-mismatch times!")
	}

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

	return verifier.insertRecheckDocs(ctx, dbNames, collNames, documentIDs, dataSizes, firstMismatchTimes)
}

func (verifier *Verifier) insertRecheckDocs(
	ctx context.Context,
	dbNames []string,
	collNames []string,
	documentIDs []bson.RawValue,
	dataSizes []int32,
	firstMismatchTimes []bson.DateTime,
) error {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation, _ := verifier.getGenerationWhileLocked()

	// We enqueue for the generation after the current one.
	generation++

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

	var firstMismatchTime option.Option[bson.DateTime]
	curRechecks := make([]bson.Raw, 0, recheckBatchCountLimit)
	curBatchBytes := 0
	for i, dbName := range dbNames {
		if firstMismatchTimes != nil {
			firstMismatchTime = option.Some(firstMismatchTimes[i])
		}

		recheckDoc := recheck.Doc{
			PrimaryKey: recheck.PrimaryKey{
				SrcDatabaseName:   dbName,
				SrcCollectionName: collNames[i],
				DocumentID:        documentIDs[i],
				Rand:              rand.Int32(),
			},
			DataSize:          dataSizes[i],
			FirstMismatchTime: firstMismatchTime,
		}

		recheckRaw := recheckDoc.MarshalToBSON()

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

// DropCurrentGenRecheckQueue deletes the current generation’s recheck
// documents from the verifier’s metadata. This should only be called
// after new recheck tasks have been created from those rechecks.
func (verifier *Verifier) DropCurrentGenRecheckQueue(ctx context.Context) error {
	generation := verifier.generation

	verifier.logger.Debug().
		Int("generation", generation).
		Msg("Deleting current generation's enqueued rechecks.")

	genCollection := verifier.getRecheckQueueCollection(generation)

	return retry.New().WithCallback(
		func(ctx context.Context, i *retry.FuncInfo) error {
			return genCollection.Drop(ctx)
		},
		"deleting generation %d's enqueued rechecks",
		generation,
	).Run(ctx, verifier.logger)
}

// GenerateRecheckTasks fetches the rechecks enqueued for the current generation
// from the verifier’s metadata and creates document-verification tasks from
// them.
//
// Note that this function DOES NOT retry on failure, so callers should wrap
// calls to this function in a retryer.
func (verifier *Verifier) GenerateRecheckTasks(ctx context.Context) error {
	generation, _ := verifier.getGeneration()

	recheckColl := verifier.getRecheckQueueCollection(generation)

	startTime := time.Now()

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

	firstMismatchTime := map[int32]bson.DateTime{}

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

	var curTasks []bson.Raw
	var curTasksBytes int

	eg, egCtx := contextplus.ErrGroup(ctx)

	var totalTasks, totalInserts int
	persistBufferedRechecks := func() error {
		if len(idAccum) == 0 {
			return nil
		}

		namespace := prevDBName + "." + prevCollName

		task, err := verifier.createDocumentRecheckTask(
			idAccum,
			firstMismatchTime,
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
		totalTasks++

		taskRaw, err := bson.Marshal(task)
		if err != nil {
			return errors.Wrapf(
				err,
				"failed to marshal a %d-document recheck task for collection %#q",
				len(idAccum),
				namespace,
			)
		}

		curTasks = append(curTasks, taskRaw)
		curTasksBytes += len(taskRaw)
		if len(curTasks) == maxTasksPerRequest || curTasksBytes >= maxInsertSize {
			tasksClone := slices.Clone(curTasks)
			curTasks = curTasks[:0]

			eg.Go(
				func() error {
					return verifier.insertDocumentRecheckTasks(egCtx, tasksClone)
				},
			)

			totalInserts++
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
			clear(firstMismatchTime)
		}

		// A document can be enqueued for recheck for multiple reasons:
		// - changed on source
		// - changed on destination
		// - mismatch seen
		//
		// We’re iterating the rechecks in order such that, if the same doc
		// gets enqueued from multiple sources, we’ll see those records
		// consecutively. We can deduplicate here, then, by checking to see if
		// the doc ID has changed. (NB: At this point we know the namespace
		// has *not* changed because we just checked for that.)
		if idRaw.Equal(lastIDRaw) {

			if doc.FirstMismatchTime.IsNone() {
				// A non-mismatch recheck means the document changed. In that
				// case we want to clear the mismatch count. This way a document
				// that changes over & over won’t seem persistently mismatched
				// merely because the replicator hasn’t kept up with the rate
				// of change.
				lastIDIndex := len(idAccum) - 1

				delete(firstMismatchTime, int32(lastIDIndex))
			}

			continue
		}

		lastIDRaw = idRaw

		idsSizer.Add(idRaw)
		dataSizeAccum += int64(doc.DataSize)
		if fmt, has := doc.FirstMismatchTime.Get(); has {
			firstMismatchTime[int32(len(idAccum))] = fmt
		}
		idAccum = append(idAccum, doc.PrimaryKey.DocumentID)

		totalRecheckData += int64(doc.DataSize)
		totalDocs++
	}

	err = cursor.Err()
	if err != nil {
		return errors.Wrapf(err, "reading persisted rechecks")
	}

	err = persistBufferedRechecks()
	if err != nil {
		return err
	}

	if len(curTasks) > 0 {
		eg.Go(
			func() error {
				return verifier.insertDocumentRecheckTasks(egCtx, curTasks)
			},
		)
	}

	err = eg.Wait()
	if err != nil {
		return errors.Wrapf(err, "persisting document recheck tasks")
	}

	if totalDocs > 0 {
		verifier.logger.Info().
			Int("generation", generation).
			Int64("totalDocs", int64(totalDocs)).
			Int("tasks", totalTasks).
			Int("insertRequests", totalInserts).
			Str("totalData", reportutils.FmtBytes(totalRecheckData)).
			Stringer("timeElapsed", time.Since(startTime)).
			Msg("Scheduled documents for recheck in the new generation.")
	}

	return err
}

func (v *Verifier) getRecheckQueueCollection(generation int) *mongo.Collection {
	if generation == 0 {
		panic("Recheck for generation 0 is nonsense!")
	}

	return v.verificationDatabase().
		Collection(fmt.Sprintf("%s_gen%d", recheckQueueCollectionNameBase, generation))
}
