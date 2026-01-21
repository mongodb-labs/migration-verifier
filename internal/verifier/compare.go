package verifier

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/exp/slices"
)

const (
	readTimeout = 10 * time.Minute

	// Every (this many) docs, add stats to the doc & byte count histories.
	comparisonHistoryThreshold = 500
)

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	workerNum int,
	task *tasks.Task,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	var srcChannel, dstChannel <-chan compare.DocWithTS
	var readSrcCallback, readDstCallback func(context.Context, retry.SuccessNotifier) error

	results := []VerificationResult{}
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	retryer := retry.New().WithDescription(
		"comparing task %v's documents (namespace: %s)",
		task.PrimaryKey,
		task.QueryFilter.Namespace,
	)

	err := retryer.
		WithBefore(func() error {
			srcChannel, dstChannel, readSrcCallback, readDstCallback = verifier.getFetcherChannelsAndCallbacks(task)

			return nil
		}).
		WithErrorCodes(util.CursorKilledErrCode).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				return readSrcCallback(ctx, fi)
			},
			"reading from source",
		).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				return readDstCallback(ctx, fi)
			},
			"reading from destination",
		).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				var err error
				results, docCount, byteCount, err = verifier.compareDocsFromChannels(
					ctx,
					workerNum,
					fi,
					task,
					srcChannel,
					dstChannel,
				)

				return err
			},
			"comparing documents",
		).Run(givenCtx, verifier.logger)

	if err == nil {
		if ts, has := task.SrcTimestamp.Get(); has {
			verifier.NoteCompareOfOptime(src, ts)
		}

		if ts, has := task.DstTimestamp.Get(); has {
			verifier.NoteCompareOfOptime(dst, ts)
		}
	}

	return results, docCount, byteCount, err
}

func (verifier *Verifier) NoteCompareOfOptime(
	cluster whichCluster,
	optime bson.Timestamp,
) {
	var dg *msync.DataGuard[bson.Timestamp]

	switch cluster {
	case src:
		dg = verifier.lastProcessedSrcOptime
	case dst:
		dg = verifier.lastProcessedDstOptime
	default:
		panic("bad cluster: " + cluster)
	}

	dg.Store(func(t bson.Timestamp) bson.Timestamp {
		if optime.After(t) {
			return optime
		}
		return t
	})
}

func (verifier *Verifier) compareDocsFromChannels(
	ctx context.Context,
	workerNum int,
	fi retry.SuccessNotifier,
	task *tasks.Task,
	srcChannel, dstChannel <-chan compare.DocWithTS,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	results := []VerificationResult{}

	// Document & byte counts for both the task and a batch of docs to tally
	// for the Verifier’s relevant History structs to track those figures.
	// (We don’t want to report each individual doc in the history because that
	// could be very big.)
	var taskSrcDocCount, curHistoryDocCount types.DocumentCount
	var taskSrcByteCount, curHistoryByteCount types.ByteCount

	// Add these so that the very first logs will be meaningful.
	verifier.docsComparedHistory.Add(0)
	verifier.bytesComparedHistory.Add(0)

	mapKeyFieldNames := task.QueryFilter.GetDocKeyFields()

	namespace := task.QueryFilter.Namespace

	srcCache := map[string]compare.DocWithTS{}
	dstCache := map[string]compare.DocWithTS{}

	firstMismatchTimeLookup := firstMismatchTimeLookup{
		task:             task,
		docCompareMethod: verifier.docCompareMethod,
	}

	// A reusable slice of doc key values.
	var docKeyValues []bson.RawValue
	var mapKeyBytes []byte

	// This is the core document-handling logic. It either:
	//
	// a) caches the new document if its mapKey is unseen, or
	// b) compares the new doc against its previously-received, cached
	//    counterpart and records any mismatch.
	handleNewDoc := func(curDocWithTS compare.DocWithTS, isSrc bool) error {
		docKeyValues = docKeyValues[:0]
		docKeyValues, err := verifier.docCompareMethod.GetDocKeyValues(
			docKeyValues,
			curDocWithTS.Doc,
			mapKeyFieldNames,
		)
		if err != nil {
			return errors.Wrapf(err, "extracting doc key (fields: %v) values from doc %+v", mapKeyFieldNames, curDocWithTS.Doc)
		}

		mapKeyBytes = mapKeyBytes[:0]
		mapKey := getMapKey(mapKeyBytes, docKeyValues)

		var ourMap, theirMap map[string]compare.DocWithTS

		if isSrc {
			ourMap = srcCache
			theirMap = dstCache
		} else {
			ourMap = dstCache
			theirMap = srcCache
		}
		// See if we've already cached a document with this
		// mapKey from the other channel.
		theirDocWithTS, exists := theirMap[mapKey]

		// If there is no such cached document, then cache the newly-received
		// document in our map then proceed to the next document.
		//
		// (We'll remove the cache entry when/if the other channel yields a
		// document with the same mapKey.)
		if !exists {
			ourMap[mapKey] = curDocWithTS
			return nil
		}

		// We have two documents! First we remove the cache entry. This saves
		// memory, but more importantly, it lets us know, once we exhaust the
		// channels, which documents were missing on one side or the other.
		delete(theirMap, mapKey)

		// We can also schedule the release of the documents’ buffers.
		defer curDocWithTS.Release()
		defer theirDocWithTS.Release()

		// Now we determine which document came from whom.
		var srcDoc, dstDoc compare.DocWithTS
		if isSrc {
			srcDoc = curDocWithTS
			dstDoc = theirDocWithTS
		} else {
			srcDoc = theirDocWithTS
			dstDoc = curDocWithTS
		}

		// Finally we compare the documents and save any mismatch report(s).
		mismatches, err := verifier.compareOneDocument(srcDoc.Doc, dstDoc.Doc, namespace)

		if err != nil {
			return errors.Wrap(err, "failed to compare documents")
		}

		if len(mismatches) == 0 {
			return nil
		}

		firstMismatchTime := firstMismatchTimeLookup.get(srcDoc.Doc)

		for i := range mismatches {
			mismatches[i].MismatchHistory = createMismatchTimes(firstMismatchTime)
			mismatches[i].SrcTimestamp = option.Some(srcDoc.TS)
			mismatches[i].DstTimestamp = option.Some(dstDoc.TS)
		}

		results = append(results, mismatches...)

		return nil
	}

	var srcClosed, dstClosed bool

	readTimer := time.NewTimer(0)
	defer func() {
		if !readTimer.Stop() {
			<-readTimer.C
		}
	}()

	// We always read src & dst together. This ensures that, if one side
	// lags the other significantly, we won’t keep caching the faster side’s
	// documents and thus consume more & more memory.
	for !srcClosed || !dstClosed {
		simpleTimerReset(readTimer, readTimeout)

		var srcDocWithTS, dstDocWithTS compare.DocWithTS

		eg, egCtx := contextplus.ErrGroup(ctx)

		if !srcClosed {
			eg.Go(func() error {
				var alive bool
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case <-readTimer.C:
					return errors.Errorf(
						"failed to read from source after %s",
						readTimeout,
					)
				case srcDocWithTS, alive = <-srcChannel:
					if !alive {
						srcClosed = true
						break
					}

					fi.NoteSuccess("received document from source")

					taskSrcDocCount++
					taskSrcByteCount += types.ByteCount(len(srcDocWithTS.Doc))

					verifier.workerTracker.SetSrcCounts(
						workerNum,
						taskSrcDocCount,
						taskSrcByteCount,
					)

					curHistoryDocCount++
					curHistoryByteCount += types.ByteCount(len(srcDocWithTS.Doc))
					if curHistoryDocCount >= comparisonHistoryThreshold {
						verifier.docsComparedHistory.Add(curHistoryDocCount)
						verifier.bytesComparedHistory.Add(curHistoryByteCount)

						curHistoryDocCount = 0
						curHistoryByteCount = 0
					}
				}

				return nil
			})
		}

		if !dstClosed {
			eg.Go(func() error {
				var alive bool
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case <-readTimer.C:
					return errors.Errorf(
						"failed to read from destination after %s",
						readTimeout,
					)
				case dstDocWithTS, alive = <-dstChannel:
					if !alive {
						dstClosed = true
						break
					}

					fi.NoteSuccess("received document from destination")
				}

				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			return nil, 0, 0, errors.Wrap(
				err,
				"failed to read documents",
			)
		}

		if srcDocWithTS.Doc != nil {
			err := handleNewDoc(srcDocWithTS, true)

			if err != nil {

				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's source doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					srcDocWithTS.Doc.Lookup("_id"),
				)
			}
		}

		if dstDocWithTS.Doc != nil {
			err := handleNewDoc(dstDocWithTS, false)

			if err != nil {
				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's destination doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					dstDocWithTS.Doc.Lookup("_id"),
				)
			}
		}
	}

	// We got here because both srcChannel and dstChannel are closed,
	// which means we have processed all documents with the same mapKey
	// between source & destination.
	//
	// At this point, any documents left in the cache maps are simply
	// missing on the other side. We add results for those.

	// We might as well pre-grow the slice:
	results = slices.Grow(results, len(srcCache)+len(dstCache))

	for _, docWithTS := range srcCache {
		firstMismatchTime := firstMismatchTimeLookup.get(docWithTS.Doc)

		results = append(
			results,
			VerificationResult{
				ID: lo.Must(verifier.docCompareMethod.ClonedDocIDForComparison(
					docWithTS.Doc,
				)),
				Details:         Missing,
				Cluster:         ClusterTarget,
				NameSpace:       namespace,
				dataSize:        int32(len(docWithTS.Doc)),
				SrcTimestamp:    option.Some(docWithTS.TS),
				MismatchHistory: createMismatchTimes(firstMismatchTime),
			},
		)

		docWithTS.Release()
	}

	for _, docWithTS := range dstCache {
		firstMismatchTime := firstMismatchTimeLookup.get(docWithTS.Doc)

		results = append(
			results,
			VerificationResult{
				ID: lo.Must(verifier.docCompareMethod.ClonedDocIDForComparison(
					docWithTS.Doc,
				)),
				Details:      Missing,
				Cluster:      ClusterSource,
				NameSpace:    namespace,
				DstTimestamp: option.Some(docWithTS.TS),

				dataSize:        int32(len(docWithTS.Doc)),
				MismatchHistory: createMismatchTimes(firstMismatchTime),
			},
		)

		docWithTS.Release()
	}

	verifier.docsComparedHistory.Add(curHistoryDocCount)
	verifier.bytesComparedHistory.Add(curHistoryByteCount)

	return results, taskSrcDocCount, taskSrcByteCount, nil
}

func createMismatchTimes(firstDateTime option.Option[bson.DateTime]) recheck.MismatchHistory {
	if fdt, has := firstDateTime.Get(); has {
		return recheck.MismatchHistory{
			First:      fdt,
			DurationMS: time.Since(fdt.Time()).Milliseconds(),
		}
	}

	return recheck.MismatchHistory{
		First: bson.NewDateTimeFromTime(time.Now()),
	}
}

func simpleTimerReset(t *time.Timer, dur time.Duration) {
	if !t.Stop() {
		<-t.C
	}

	t.Reset(dur)
}

func (verifier *Verifier) getFetcherChannelsAndCallbacks(
	task *tasks.Task,
) (
	<-chan compare.DocWithTS,
	<-chan compare.DocWithTS,
	func(context.Context, retry.SuccessNotifier) error,
	func(context.Context, retry.SuccessNotifier) error,
) {
	srcChannel := make(chan compare.DocWithTS)
	dstChannel := make(chan compare.DocWithTS)

	readSrcCallback := func(ctx context.Context, state retry.SuccessNotifier) error {
		// We open a session here so that we can read the session’s cluster
		// time, which we store along with any document mismatches we may see.
		//
		// Ideally the driver would just expose the individual server responses’
		// cluster times, but alas.
		sess, err := verifier.srcClient.StartSession()
		if err != nil {
			return errors.Wrapf(err, "starting session")
		}

		sctx := mongo.NewSessionContext(ctx, sess)

		cursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.srcClientCollection(task),
			verifier.srcClusterInfo,
			verifier.srcChangeReader.getLastSeenClusterTime().OrElse(
				verifier.srcChangeReader.getStartTimestamp(),
			),
			task,
		)

		if err == nil {
			state.NoteSuccess("opened src find cursor")

			err = errors.Wrap(
				iterateCursorToChannel(sctx, state, cursor, srcChannel),
				"failed to read source documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"opening source documents cursor",
			)
		}

		return err
	}

	readDstCallback := func(ctx context.Context, state retry.SuccessNotifier) error {
		sess, err := verifier.dstClient.StartSession()
		if err != nil {
			return errors.Wrapf(err, "starting session")
		}

		sctx := mongo.NewSessionContext(ctx, sess)

		cursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.dstClientCollection(task),
			verifier.dstClusterInfo,
			verifier.dstChangeReader.getLastSeenClusterTime().OrElse(
				verifier.dstChangeReader.getStartTimestamp(),
			),
			task,
		)

		if err == nil {
			state.NoteSuccess("opened dst find cursor")

			err = errors.Wrap(
				iterateCursorToChannel(sctx, state, cursor, dstChannel),
				"failed to read destination documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"failed to find destination documents",
			)
		}

		return err
	}

	return srcChannel, dstChannel, readSrcCallback, readDstCallback
}

func iterateCursorToChannel(
	sctx context.Context,
	state retry.SuccessNotifier,
	cursor *mongo.Cursor,
	writer chan<- compare.DocWithTS,
) error {
	defer close(writer)

	sess := mongo.SessionFromContext(sctx)
	if sess == nil {
		panic("need a session")
	}

	for cursor.Next(sctx) {
		state.NoteSuccess("received a document")

		clusterTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			return errors.Wrap(err, "reading cluster time from session")
		}

		err = chanutil.WriteWithDoneCheck(
			sctx,
			writer,
			compare.NewDocWithTS(cursor.Current, clusterTime),
		)

		if err != nil {
			return errors.Wrapf(err, "sending document to compare thread")
		}
	}

	return errors.Wrap(cursor.Err(), "failed to iterate cursor")
}

func getMapKey(buf []byte, docKeyValues []bson.RawValue) string {
	for _, value := range docKeyValues {
		buf = rvToMapKey(buf, value)
	}

	return string(buf)
}

func (verifier *Verifier) getDocumentsCursor(
	sctx context.Context,
	collection *mongo.Collection,
	clusterInfo *util.ClusterInfo,
	readConcernTS bson.Timestamp,
	task *tasks.Task,
) (*mongo.Cursor, error) {
	var findOptions bson.D
	var andPredicates bson.A

	if task.Ids != nil {
		andPredicates = append(andPredicates, bson.D{{"_id", bson.M{"$in": task.Ids}}})
		andPredicates = verifier.maybeAppendGlobalFilterToPredicates(andPredicates)
		filter := bson.D{{"$and", andPredicates}}

		findOptions = bson.D{
			bson.E{"filter", filter},
		}
	} else {
		pqp, err := task.QueryFilter.Partition.GetQueryParameters(
			clusterInfo,
			verifier.maybeAppendGlobalFilterToPredicates(andPredicates),
		)
		if err != nil {
			return nil, errors.Wrapf(err, "getting query parameters for task: %+v", task)
		}

		findOptions = pqp.ToFindOptions()
	}

	cmd := append(
		bson.D{{"find", collection.Name()}},
		findOptions...,
	)

	if verifier.docCompareMethod == compare.ToHashedIndexKey {
		cmd = append(
			cmd,
			bson.E{"projection", compare.GetHashedIndexKeyProjection(task.QueryFilter)},
		)
	}

	sess := mongo.SessionFromContext(sctx)

	if sess == nil {
		panic("No session?!?")
	}

	runCommandOptions := options.RunCmd().SetReadPreference(verifier.readPreference)

	// We never want to read before the change stream start time,
	// or for the last generation, the change stream end time.
	cmd = append(
		cmd,
		bson.E{"readConcern", bson.D{
			{"level", "majority"},
			{"afterClusterTime", readConcernTS},
		}},
	)

	// Suppress this log for recheck tasks because the list of IDs can be
	// quite long.
	if task.Ids == nil {
		if verifier.logger.Trace().Enabled() {

			evt := verifier.logger.Trace().
				Any("task", task.PrimaryKey)

			cmdStr, err := bson.MarshalExtJSON(cmd, true, false)
			if err != nil {
				cmdStr = fmt.Appendf(nil, "%s", cmd)
			}

			evt.
				Str("cmd", string(cmdStr)).
				Str("options", fmt.Sprintf("%v", *runCommandOptions)).
				Msg("getDocuments command.")

		}
	}

	return collection.Database().RunCommandCursor(
		sctx,
		cmd,
		runCommandOptions,
	)
}

func (verifier *Verifier) compareOneDocument(srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		// Happy path! The documents binary-match.
		return nil, nil
	}

	docID, err := verifier.docCompareMethod.ClonedDocIDForComparison(srcClientDoc)
	if err != nil {
		return nil, errors.Wrapf(err, "extracting doc ID for comparison")
	}

	if verifier.docCompareMethod == compare.ToHashedIndexKey {
		// With hash comparison, mismatches are opaque.
		return []VerificationResult{{
			ID:        docID,
			Details:   Mismatch,
			Cluster:   ClusterTarget,
			NameSpace: namespace,
		}}, nil
	}

	mismatch, err := BsonUnorderedCompareRawDocumentWithDetails(srcClientDoc, dstClientDoc)
	if err != nil {
		return nil, err
	}
	if mismatch == nil {
		if verifier.docCompareMethod.ShouldIgnoreFieldOrder() {
			return nil, nil
		}
		dataSize := max(len(srcClientDoc), len(dstClientDoc))

		// If we're respecting field order we have just done a binary compare so we have fields in different order.
		return []VerificationResult{{
			ID:        docID,
			Details:   Mismatch + " : only field order differs",
			Cluster:   ClusterTarget,
			NameSpace: namespace,
			dataSize:  int32(dataSize),
		}}, nil
	}

	results := mismatchResultsToVerificationResults(mismatch, srcClientDoc, dstClientDoc, namespace, docID, "" /* fieldPrefix */)

	return results, nil
}
