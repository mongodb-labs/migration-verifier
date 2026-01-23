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
	"github.com/10gen/migration-verifier/mslices"
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
	var srcChannel, dstChannel <-chan []compare.DocWithTS
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
			var err error

			srcChannel, dstChannel, readSrcCallback, readDstCallback, err = verifier.getFetcherChannelsAndCallbacks(task)

			return err
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
	srcChannel, dstChannel <-chan []compare.DocWithTS,
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
		defer curDocWithTS.Free()
		defer theirDocWithTS.Free()

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

		var srcDocsWithTS, dstDocsWithTS []compare.DocWithTS

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
				case srcDocsWithTS, alive = <-srcChannel:
					if !alive {
						srcClosed = true
						break
					}

					fi.NoteSuccess("received document from source")

					taskSrcDocCount += types.DocumentCount(len(srcDocsWithTS))
					curHistoryDocCount += types.DocumentCount(len(srcDocsWithTS))

					for _, docWithTS := range srcDocsWithTS {
						taskSrcByteCount += types.ByteCount(len(docWithTS.Doc))

						curHistoryByteCount += types.ByteCount(len(docWithTS.Doc))
						if curHistoryDocCount >= comparisonHistoryThreshold {
							verifier.docsComparedHistory.Add(curHistoryDocCount)
							verifier.bytesComparedHistory.Add(curHistoryByteCount)

							curHistoryDocCount = 0
							curHistoryByteCount = 0
						}
					}

					verifier.workerTracker.SetSrcCounts(
						workerNum,
						taskSrcDocCount,
						taskSrcByteCount,
					)
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
				case dstDocsWithTS, alive = <-dstChannel:
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

		for len(srcDocsWithTS)+len(dstDocsWithTS) > 0 {
			if len(srcDocsWithTS) > 0 {
				srcDoc := srcDocsWithTS[0]
				srcDocsWithTS = srcDocsWithTS[1:]

				err := handleNewDoc(srcDoc, true)

				if err != nil {

					return nil, 0, 0, errors.Wrapf(
						err,
						"comparer thread failed to handle %#q's source doc (task: %s) with ID %v",
						namespace,
						task.PrimaryKey,
						srcDoc.Doc.Lookup("_id"),
					)
				}
			}

			if len(dstDocsWithTS) > 0 {
				dstDoc := dstDocsWithTS[0]
				dstDocsWithTS = dstDocsWithTS[1:]

				err := handleNewDoc(dstDoc, false)

				if err != nil {

					return nil, 0, 0, errors.Wrapf(
						err,
						"comparer thread failed to handle %#q's destination doc (task: %s) with ID %v",
						namespace,
						task.PrimaryKey,
						dstDoc.Doc.Lookup("_id"),
					)
				}
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

		docWithTS.Free()
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

		docWithTS.Free()
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
	<-chan []compare.DocWithTS,
	<-chan []compare.DocWithTS,
	func(context.Context, retry.SuccessNotifier) error,
	func(context.Context, retry.SuccessNotifier) error,
	error,
) {
	if task.QueryFilter.Partition != nil && task.QueryFilter.Partition.Natural {
		return verifier.getFetcherChannelsAndCallbacksForNaturalPartition(task)
	}

	srcChan, dstChan, srcCB, dstCB := verifier.getFetcherChannelsAndCallbacksForIDPartition(task)

	return srcChan, dstChan, srcCB, dstCB, nil
}

func (verifier *Verifier) getFetcherChannelsAndCallbacksForNaturalPartition(
	task *tasks.Task,
) (
	<-chan []compare.DocWithTS,
	<-chan []compare.DocWithTS,
	func(context.Context, retry.SuccessNotifier) error,
	func(context.Context, retry.SuccessNotifier) error,
	error,
) {
	hostname := task.QueryFilter.Partition.HostnameAndPort.MustGetf(
		"hostname/port missing; this is required for natural partitions",
	)

	connstr, err := compare.SetDirectHostInConnectionString(
		verifier.srcURI,
		hostname,
	)
	if err != nil {
		return nil, nil, nil, nil, errors.Wrapf(err, "setting source connstr to connect directly to %#q", hostname)
	}

	client, err := mongo.Connect(options.Client().
		ApplyURI(connstr).
		SetAppName(clientAppName),
	)
	if err != nil {
		return nil, nil, nil, nil, errors.Wrapf(err, "connecting to client for natural read")
	}

	srcToCompareChannel := make(chan []compare.DocWithTS)
	dstToCompareChannel := make(chan []compare.DocWithTS)

	srcToDstChannel := make(chan []compare.DocID, 1_000)

	// Read documents between the lower & upper bounds. Since we can’t actually
	// query the server that way, though, we subtract the record IDs & set that
	// difference as the query’s limit. Any “missing” record IDs will cause
	// “excess” documents to be read past the upper bound, but all that’ll cause
	// is redundant comparisons, which is OK. (Hopefully there won’t be too-too
	// many of those.)
	readSrcCallback := func(ctx context.Context, state retry.SuccessNotifier) error {
		return compare.ReadNaturalPartitionFromSource(
			ctx,
			verifier.logger,
			state,
			client,
			verifier.verificationTaskCollection(),
			task,
			option.IfNotZero(verifier.globalFilter),
			verifier.docCompareMethod,
			srcToCompareChannel,
			srcToDstChannel,
		)
	}

	// We do NOT query the destination by record ID because record IDs probably
	// differ from those on the source for the same document. Instead the source
	// sends document IDs to this thread, then we read those documents
	// individually from the destination. It means the destination lags the
	// source, but as long as the channels are buffered nothing should block
	// unnecessarily.
	//
	// Documents on the destination should be in roughly the same natural order
	// as on the source. (Otherwise we’ll incur read amplification on the
	// destination.)
	readDstCallback := func(ctx context.Context, state retry.SuccessNotifier) error {
		defer func() {
			close(dstToCompareChannel)
		}()

		sess, err := verifier.dstClient.StartSession()
		if err != nil {
			return errors.Wrapf(err, "starting session")
		}
		defer sess.EndSession(ctx)

		sctx := mongo.NewSessionContext(ctx, sess)

		coll := verifier.dstClientCollection(task)

		for {
			docIDsOpt, err := chanutil.ReadWithDoneCheck(sctx, srcToDstChannel)

			if err != nil {
				return err
			}
			docIDs, isOpen := docIDsOpt.Get()
			if !isOpen {
				state.NoteSuccess("saw channel from source closed")
				break
			}

			state.NoteSuccess("received %d doc IDs from source to fetch", len(docIDs))

			dupeTask := *task
			dupeTask.Ids = mslices.Map1(
				docIDs,
				func(id compare.DocID) bson.RawValue {
					return id.ID
				},
			)

			verifier.logger.Trace().
				Any("task", task.PrimaryKey).
				Int("count", len(docIDs)).
				Msg("Querying dst for documents.")

			cursorStartTime := time.Now()

			cursor, err := verifier.getDocumentsCursor(
				sctx,
				coll,
				verifier.dstClusterInfo,
				verifier.dstChangeReader.getStartTimestamp(),
				&dupeTask,
			)

			for _, id := range docIDs {
				id.Free()
			}

			if err != nil {
				return errors.Wrapf(err, "finding %d documents", len(docIDs))
			}

			state.NoteSuccess("opened dst find cursor")

			verifier.logger.Trace().
				Any("task", task.PrimaryKey).
				Int("idsInQuery", len(docIDs)).
				Msg("Iterating dst cursor.")

			dstDocsFound, err := iterateCursorToChannel(sctx, state, cursor, dstToCompareChannel)
			if err != nil {
				return errors.Wrap(
					err,
					"failed to send documents from destination to compare",
				)
			}

			verifier.logger.Trace().
				Any("task", task.PrimaryKey).
				Int("docsFound", dstDocsFound).
				Stringer("elapsed", time.Since(cursorStartTime)).
				Msg("Done iterating dst cursor.")

			// The compare thread, to prevent OOMs, always reads documents
			// from the src & dst together. It only stops listening on one
			// side or the other when the channel closes. This is fine for
			// ID-partitioned verification because there is exactly 1 query
			// per partition & cluster.
			//
			// With natural partitioning, though, the destination runs
			// a separate query for each document batch from the source.
			// So if there are missing documents on the destination, we’ll
			// block the compare thread unless the destination “compensates”
			// by sending dummy values. We do that here.
			missingDocsCount := len(docIDs) - dstDocsFound

			lo.Assertf(
				missingDocsCount >= 0,
				"dest docs (%d) must be <= source docs (%d)",
				dstDocsFound,
				len(docIDs),
			)

			if missingDocsCount > 0 {
				verifier.logger.Trace().
					Any("task", task.PrimaryKey).
					Int("count", missingDocsCount).
					Msg("Sending dummy dst docs to compare thread.")
			}

			// Reader threads send documents to the comparator in slices rather
			// than individually. We could send 1 slice per document, but if
			// there are 1,000 documents missing then we’re sending 1,000
			// messages, which is a bit unideal. We thus optimize by sending
			// only as many dummy messages as we need to unblock the comparator.
			srcBatchesSent := compare.ToComparatorBatchCount(len(docIDs))
			dstBatchesSent := compare.ToComparatorBatchCount(dstDocsFound)

			for i := range srcBatchesSent - dstBatchesSent {
				err := chanutil.WriteWithDoneCheck(
					ctx,
					dstToCompareChannel,
					nil,
				)
				if err != nil {
					return errors.Wrapf(err, "sending dummy doc %d of %d dst->compare", 1+i, missingDocsCount)
				}

				state.NoteSuccess(
					"sent dummy doc #%d of %d to compare thread",
					1+i,
					missingDocsCount,
				)
			}

		}

		return nil
	}

	return srcToCompareChannel, dstToCompareChannel, readSrcCallback, readDstCallback, nil
}

func (verifier *Verifier) getFetcherChannelsAndCallbacksForIDPartition(
	task *tasks.Task,
) (
	<-chan []compare.DocWithTS,
	<-chan []compare.DocWithTS,
	func(context.Context, retry.SuccessNotifier) error,
	func(context.Context, retry.SuccessNotifier) error,
) {
	srcChannel := make(chan []compare.DocWithTS)
	dstChannel := make(chan []compare.DocWithTS)

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

			_, err = iterateCursorToChannel(sctx, state, cursor, srcChannel)

			err = errors.Wrap(
				err,
				"failed to read source documents",
			)

			close(srcChannel)
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

			_, err = iterateCursorToChannel(sctx, state, cursor, dstChannel)
			err = errors.Wrap(
				err,
				"failed to read destination documents",
			)

			close(dstChannel)
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

// This returns the # of documents sent to the comparator and # of batches
// used to send those documents.
func iterateCursorToChannel(
	sctx context.Context,
	state retry.SuccessNotifier,
	cursor *mongo.Cursor,
	writer chan<- []compare.DocWithTS,
) (int, error) {
	sess := mongo.SessionFromContext(sctx)
	if sess == nil {
		panic("need a session")
	}

	docsCount := 0

	var docsWithTSCache []compare.DocWithTS

	flush := func() error {
		err := chanutil.WriteWithDoneCheck(
			sctx,
			writer,
			slices.Clone(docsWithTSCache),
		)

		if err != nil {
			return errors.Wrapf(err, "sending %d documents to compare thread", len(docsWithTSCache))
		}

		state.NoteSuccess("sent %d documents to compare thread", len(docsWithTSCache))

		docsWithTSCache = docsWithTSCache[:0]

		return nil
	}

	for cursor.Next(sctx) {
		state.NoteSuccess("received a document")

		docsCount++

		clusterTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			return 0, errors.Wrap(err, "reading cluster time from session")
		}

		docsWithTSCache = append(
			docsWithTSCache,
			compare.NewDocWithTS(cursor.Current, clusterTime),
		)

		if len(docsWithTSCache) == compare.ToComparatorBatchSize {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}

	if cursor.Err() != nil {
		return 0, errors.Wrap(cursor.Err(), "failed to iterate cursor")
	}

	state.NoteSuccess("exhausted cursor")

	if len(docsWithTSCache) > 0 {
		if err := flush(); err != nil {
			return 0, err
		}
	}

	return docsCount, nil
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
		bson.D{
			{"find", collection.Name()},
			{"comment", fmt.Sprintf("task %v", task.PrimaryKey)},
		},
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
