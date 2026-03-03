package verifier

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"golang.org/x/exp/slices"
)

const (
	readTimeout = 10 * time.Minute

	// Every (this many) docs, add stats to the doc & byte count histories.
	comparisonHistoryThreshold = 500
)

// DocCompareReport represents a batch of document discrepancies.
type DocCompareReport struct {
	// Problems details the discrepancies.
	Problems []compare.Result

	// DocCount is the number of documents checked.
	DocCount types.DocumentCount

	// ByteCount is the total size of the source’s documents.
	ByteCount types.ByteCount
}

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	workerNum int,
	task *tasks.Task,
) <-chan mo.Result[DocCompareReport] {
	var srcChannel, dstChannel <-chan []compare.DocWithTS
	var readSrcCallback, readDstCallback func(context.Context, retry.SuccessNotifier) error

	retryer := retry.New().WithDescription(
		"comparing task %v's documents (namespace: %s)",
		task.PrimaryKey,
		task.QueryFilter.Namespace,
	)

	resultsChan := make(chan mo.Result[DocCompareReport], 100)

	go func() {
		defer close(resultsChan)

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
					reportChan, canceler := chanutil.StartIngestMap(
						ctx,
						resultsChan,
						mo.Ok[DocCompareReport],
					)

					defer func() {
						_ = canceler(ctx)
					}()

					return verifier.compareDocsFromChannels(
						ctx,
						workerNum,
						fi,
						task,
						srcChannel,
						dstChannel,
						reportChan,
					)
				},
				"comparing documents",
			).Run(givenCtx, verifier.logger)

		if err != nil {
			writeErr := chanutil.WriteWithDoneCheck(
				givenCtx,
				resultsChan,
				mo.Err[DocCompareReport](err),
			)

			if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
				verifier.logger.Warn().
					Err(err).
					Msg("Failed to compare documents.")
			}

			return
		}

		if ts, has := task.SrcTimestamp.Get(); has {
			verifier.NoteCompareOfOptime(src, ts)
		}

		if ts, has := task.DstTimestamp.Get(); has {
			verifier.NoteCompareOfOptime(dst, ts)
		}
	}()

	return resultsChan
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

// NB: This closes the given reportsChan.
func (verifier *Verifier) compareDocsFromChannels(
	ctx context.Context,
	workerNum int,
	fi retry.SuccessNotifier,
	task *tasks.Task,
	srcChannel, dstChannel <-chan []compare.DocWithTS,
	reportsChan chan<- DocCompareReport,
) error {

	// 1. Initialize State
	c := newComparator(verifier, workerNum, fi, task)
	defer func() {
		if !c.readTimer.Stop() {
			<-c.readTimer.C
		}
	}()

	verifier.docsComparedHistory.Add(0)
	verifier.bytesComparedHistory.Add(0)

	// 2. Stream Processing Loop
	for c.stillReading() {
		srcBatch, dstBatch, err := c.readBatches(ctx, srcChannel, dstChannel)
		if err != nil {
			return err
		}

		// Interleave processing the batches
		for len(srcBatch)+len(dstBatch) > 0 {
			if len(srcBatch) > 0 {
				doc := srcBatch[0]
				srcBatch = srcBatch[1:]

				if err := c.processSingleDoc(doc, true); err != nil {
					return errors.Wrapf(
						err, "comparer thread failed to handle %#q's source doc (task: %s) with ID %v",
						c.namespace, task.PrimaryKey, doc.Doc.Lookup("_id"),
					)
				}
			}

			if len(dstBatch) > 0 {
				doc := dstBatch[0]
				dstBatch = dstBatch[1:]

				if err := c.processSingleDoc(doc, false); err != nil {
					return errors.Wrapf(
						err, "comparer thread failed to handle %#q's destination doc (task: %s) with ID %v",
						c.namespace, task.PrimaryKey, doc.Doc.Lookup("_id"),
					)
				}
			}
		}

		probs, err := c.flushIfNeeded(ctx, reportsChan)

		if err != nil {
			return errors.Wrapf(err, "flushing problems")
		}

		if probs > 0 {
			verifier.logger.Debug().
				Any("task", task.PrimaryKey).
				Int("workerNum", workerNum).
				Str("namespace", task.QueryFilter.Namespace).
				Int("count", probs).
				Msg("Recorded document-disparity problems.")
		}
	}

	_, err := c.flush(ctx, reportsChan)

	return err
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
	hostnameAndPort := task.QueryFilter.Partition.HostnameAndPort.MustGetf(
		"hostname/port missing; this is required for natural partitions",
	)

	client, err := mmongo.GetDirectClient(verifier.srcURI, hostnameAndPort)
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

			theCursor, err := verifier.getDocumentsCursor(
				sctx,
				coll,
				verifier.dstClusterInfo,
				verifier.dstChangeReader.getStartTimestamp(),
				&dupeTask,
				option.None[*readpref.ReadPref](),
			)

			for _, id := range docIDs {
				id.PutInPool()
			}

			if err != nil {
				return errors.Wrapf(err, "finding %d documents", len(docIDs))
			}

			state.NoteSuccess("opened dst find cursor")

			verifier.logger.Trace().
				Any("task", task.PrimaryKey).
				Int("idsInQuery", len(docIDs)).
				Msg("Iterating dst cursor.")

			dstDocsFound, err := iterateCursorToChannel(
				sctx,
				state,
				theCursor,
				dstToCompareChannel,
			)
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

		theCursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.srcClientCollection(task),
			verifier.srcClusterInfo,
			verifier.srcChangeReader.getLastSeenClusterTime().OrElse(
				verifier.srcChangeReader.getStartTimestamp(),
			),
			task,
			option.IfNotZero(verifier.readPreference),
		)

		if err == nil {
			state.NoteSuccess("opened src find cursor")

			_, err = iterateCursorToChannel(
				sctx,
				state,
				theCursor,
				srcChannel,
			)

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

		theCursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.dstClientCollection(task),
			verifier.dstClusterInfo,
			verifier.dstChangeReader.getLastSeenClusterTime().OrElse(
				verifier.dstChangeReader.getStartTimestamp(),
			),
			task,
			option.None[*readpref.ReadPref](),
		)

		if err == nil {
			state.NoteSuccess("opened dst find cursor")

			_, err = iterateCursorToChannel(
				sctx,
				state,
				theCursor,
				dstChannel,
			)
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

// iterateCursorToChannel returns the # of documents sent to the channel.
//
// NB: This DOES NOT close the given channel.
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

	var docsCount int
	var bytesEnqueued types.ByteCount

	var docsWithTSCache []compare.DocWithTS

	flush := func() error {
		err := chanutil.WriteWithDoneCheck(
			sctx,
			writer,
			slices.Clone(docsWithTSCache),
		)

		if err != nil {
			return errors.Wrapf(
				err,
				"sending %d documents (%s) to compare thread",
				len(docsWithTSCache),
				reportutils.FmtBytes(bytesEnqueued),
			)
		}

		state.NoteSuccess(
			"sent %d documents (%s) to compare thread",
			len(docsWithTSCache),
			reportutils.FmtBytes(bytesEnqueued),
		)

		docsWithTSCache = docsWithTSCache[:0]
		bytesEnqueued = 0

		return nil
	}

	for cursor.Next(sctx) {
		state.NoteSuccess("received a document")

		docsCount++

		clusterTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			return 0, errors.Wrap(err, "reading cluster time from session")
		}

		needFlush := cmp.Or(
			len(docsWithTSCache) == compare.ToComparatorBatchSize,
			int(bytesEnqueued)+len(cursor.Current) > compare.ToComparatorByteLimit,
		)

		if needFlush {
			if err := flush(); err != nil {
				return 0, err
			}
		}

		docsWithTSCache = append(
			docsWithTSCache,
			compare.NewDocWithTSFromPool(cursor.Current, clusterTime),
		)

		bytesEnqueued += types.ByteCount(len(cursor.Current))
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
	readPref option.Option[*readpref.ReadPref],
) (*mongo.Cursor, error) {
	var findOptions bson.D
	var andPredicates bson.A

	if task.Ids != nil {
		andPredicates = append(andPredicates, bson.D{{"_id", bson.M{"$in": task.Ids}}})
		andPredicates = verifier.maybeAppendGlobalFilterToPredicates(andPredicates)
		filter := bson.D{{"$and", andPredicates}}

		findOptions = bson.D{
			{"filter", filter},

			// The server limits the initial response to 101 documents by
			// default. There are decent odds, though, that a single response
			// can fit all of the needed documents, so we override that default.
			//
			// We could derive the batch size from len(task.Ids), but just in
			// case there are duplicate `_id`s across shards we might as well
			// just set a “really high” batch size.
			{"batchSize", math.MaxInt32},
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

	runCommandOptions := options.RunCmd()

	if rp, has := readPref.Get(); has {
		runCommandOptions = runCommandOptions.SetReadPreference(rp)
	}

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

func compareOneDocument(
	docCompareMethod compare.Method,
	srcClientDoc, dstClientDoc bson.Raw,
	namespace string,
) ([]compare.Result, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		// Happy path! The documents binary-match.
		return nil, nil
	}

	docID, err := docCompareMethod.ClonedDocIDForComparison(srcClientDoc)
	if err != nil {
		return nil, errors.Wrapf(err, "extracting doc ID for comparison")
	}

	if docCompareMethod == compare.ToHashedIndexKey {
		// With hash comparison, mismatches are opaque.
		return []compare.Result{{
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
		if docCompareMethod.ShouldIgnoreFieldOrder() {
			return nil, nil
		}
		dataSize := max(len(srcClientDoc), len(dstClientDoc))

		// If we're respecting field order we have just done a binary compare so we have fields in different order.
		return []compare.Result{{
			ID:        docID,
			Details:   Mismatch + " : only field order differs",
			Cluster:   ClusterTarget,
			NameSpace: namespace,
			DataSize:  int32(dataSize),
		}}, nil
	}

	results := mismatchResultsToVerificationResults(mismatch, srcClientDoc, dstClientDoc, namespace, docID, "" /* fieldPrefix */)

	return results, nil
}
