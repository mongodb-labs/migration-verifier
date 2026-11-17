package verifier

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// These somewhat-arbitrary limits constrain the verifier so that,
	// in a large, naturally-scanned collection, memory usage won’t balloon
	// when many mismatches are found.
	comparatorMaxProblemsLen   = 100_000
	comparatorMaxVariableBytes = 30 << 20 // MiB
)

type comparator struct {
	verifier  *Verifier
	task      *tasks.Task
	namespace string
	workerNum int
	fi        retry.SuccessNotifier

	// Channel states
	srcClosed bool
	dstClosed bool
	readTimer *time.Timer

	// Caches
	srcCache map[string]compare.DocWithTS
	dstCache map[string]compare.DocWithTS

	// Metrics & Results
	problems            []compare.Result
	srcDocCount         types.DocumentCount
	srcByteCount        types.ByteCount
	curHistoryDocCount  types.DocumentCount
	curHistoryByteCount types.ByteCount

	// Used in natural partitioning to synchronize src & dst readers.
	srcFinishedNaturalBatch bool
	dstFinishedNaturalBatch bool

	// We may flush the comparator’s caches when len(problems) or this value
	// exceed predefined limits.
	//
	// Note that this is a lower bound to memory
	// usage, not an attempt to track exact usage. In other words, when this
	// value hits (for example) 10 MiB, the actual memory usage is probably
	// more--even substantially so--but it should not be orders of magnitude
	// more.
	//
	// As long as we prevent an arbitrarily-long task from being able to
	// exhaust available memory, this mechanism is doing its job.
	cachedVariableBytes types.ByteCount

	// Lookups & Reusable Buffers
	firstMismatchTimeLookup firstMismatchTimeLookup
	mapKeyFieldNames        []string
	docKeyValues            []bson.RawValue
	mapKeyBytes             []byte
}

func newComparator(v *Verifier, workerNum int, fi retry.SuccessNotifier, task *tasks.Task) *comparator {
	return &comparator{
		verifier:  v,
		task:      task,
		namespace: task.QueryFilter.Namespace,
		workerNum: workerNum,
		fi:        fi,

		srcCache: map[string]compare.DocWithTS{},
		dstCache: map[string]compare.DocWithTS{},
		problems: []compare.Result{},

		firstMismatchTimeLookup: firstMismatchTimeLookup{
			task:             task,
			docCompareMethod: v.docCompareMethod,
		},
		mapKeyFieldNames: task.QueryFilter.GetDocKeyFields(),
		readTimer:        time.NewTimer(0),
	}
}

func (c *comparator) stillReadingSrc() bool {
	return !c.srcClosed && !c.srcFinishedNaturalBatch
}

func (c *comparator) stillReadingDst() bool {
	return !c.dstClosed && !c.dstFinishedNaturalBatch
}

// readBatches handles the concurrent errgroup I/O and timeout logic.
// It returns: a batch of src docs, a batch of dst docs, a flag to indicate
// reception of cursor-done from the destination, and an error.
//
// NB: The cursor-done flag means that the cursor finished *before* reading
// the dst docs. Thus, the caller MUST process that flag before
// processing the returned dst docs.
func (c *comparator) readBatches(
	ctx context.Context,
	srcChannel, dstChannel <-chan compare.ToComparatorMsg,
) ([]compare.DocWithTS, []compare.DocWithTS, bool, error) {
	simpleTimerReset(c.readTimer, readTimeout)

	var srcBatch, dstBatch []compare.DocWithTS

	// Skip reading from the source if either:
	// 1. The source is already closed.
	// 2. Natural scan: We’re waiting for the destination reader to catch up.
	if !c.srcClosed && !c.srcFinishedNaturalBatch {
		select {
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		case <-c.readTimer.C:
			return nil, nil, false, errors.Errorf("failed to read from source within %s", readTimeout)
		case msg, alive := <-srcChannel:
			if !alive {
				c.srcClosed = true

				break
			}

			c.fi.NoteSuccess("received message from source reader")

			switch payload := msg.(type) {
			case compare.DocsToComparatorMsg:
				lo.Assertf(
					len(payload) <= compare.ToComparatorBatchSize,
					"src reader should send <= %d docs but sent %d",
					compare.ToComparatorBatchSize,
					len(payload),
				)

				c.recordSrcMetrics(payload)

				srcBatch = payload
			case compare.BatchDoneComparatorMsg:
				c.srcFinishedNaturalBatch = true
			default:
				panic(fmt.Sprintf("bad message (%T) from src reader", msg))
			}
		}
	}

	// Skip reading from the destination if either:
	// 1. The destination is already closed.
	// 2. Natural scan: We’re waiting for the source reader to catch up.
	if !c.dstClosed && !c.dstFinishedNaturalBatch {
		select {
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		case <-c.readTimer.C:
			return nil, nil, false, errors.Errorf("failed to read from destination within %s", readTimeout)
		case msg, alive := <-dstChannel:
			if !alive {
				c.dstClosed = true

				break
			}

			c.fi.NoteSuccess("received message from destination reader")

			switch payload := msg.(type) {
			case compare.DocsToComparatorMsg:
				lo.Assertf(
					len(payload) <= compare.ToComparatorBatchSize,
					"dst reader should send <= %d docs but sent %d",
					compare.ToComparatorBatchSize,
					len(payload),
				)

				dstBatch = payload
			case compare.BatchDoneComparatorMsg:
				c.dstFinishedNaturalBatch = true
			default:
				panic(fmt.Sprintf("bad message (%T) from dst reader", msg))
			}
		}
	}

	var naturalBatchDone bool

	if c.srcFinishedNaturalBatch && c.dstFinishedNaturalBatch {
		naturalBatchDone = true
		c.srcFinishedNaturalBatch = false
		c.dstFinishedNaturalBatch = false
	}

	return srcBatch, dstBatch, naturalBatchDone, nil
}

func simpleTimerReset(t *time.Timer, dur time.Duration) {
	if !t.Stop() {
		<-t.C
	}

	t.Reset(dur)
}

// recordSrcMetrics isolates the noise of tracking business metrics.
func (c *comparator) recordSrcMetrics(docs []compare.DocWithTS) {
	c.srcDocCount += types.DocumentCount(len(docs))
	c.curHistoryDocCount += types.DocumentCount(len(docs))

	for _, docWithTS := range docs {
		docSize := c.verifier.docCompareMethod.DocSize(docWithTS.Doc)
		c.srcByteCount += docSize
		c.curHistoryByteCount += docSize

		if c.curHistoryDocCount >= comparisonHistoryThreshold {
			c.publishHistoryCounts()
		}
	}

	c.verifier.workerTracker.SetSrcCounts(c.workerNum, c.srcDocCount, c.srcByteCount)
}

func (c *comparator) publishHistoryCounts() {
	c.verifier.docsComparedHistory.Add(c.curHistoryDocCount)
	c.verifier.bytesComparedHistory.Add(c.curHistoryByteCount)

	c.curHistoryDocCount = 0
	c.curHistoryByteCount = 0
}

// processSingleDoc is the refactored `handleNewDoc` closure.
func (c *comparator) processSingleDoc(curDocWithTS compare.DocWithTS, isSrc bool) error {
	var err error
	c.docKeyValues = c.docKeyValues[:0]
	c.docKeyValues, err = c.verifier.docCompareMethod.GetDocKeyValues(
		c.docKeyValues, curDocWithTS.Doc, c.mapKeyFieldNames,
	)
	if err != nil {
		return errors.Wrapf(err, "extracting doc key (fields: %v) values from doc %+v", c.mapKeyFieldNames, curDocWithTS.Doc)
	}

	c.mapKeyBytes = c.mapKeyBytes[:0]
	mapKey := getMapKey(c.mapKeyBytes, c.docKeyValues)

	var ourMap, theirMap map[string]compare.DocWithTS
	if isSrc {
		ourMap, theirMap = c.srcCache, c.dstCache
	} else {
		ourMap, theirMap = c.dstCache, c.srcCache
	}

	theirDocWithTS, exists := theirMap[mapKey]
	if !exists {
		// The other map lacks this document. Cache it, and we’re done for now.
		ourMap[mapKey] = curDocWithTS
		c.cachedVariableBytes += types.ByteCount(len(mapKey) + len(curDocWithTS.Doc))

		return nil
	}

	// The other map also has the document. Remove it from the cache, then
	// we’ll compare their doc with ours.
	delete(theirMap, mapKey)
	c.cachedVariableBytes -= types.ByteCount(len(mapKey) + len(theirDocWithTS.Doc))

	defer curDocWithTS.PutInPool()
	defer theirDocWithTS.PutInPool()

	var srcDoc, dstDoc compare.DocWithTS
	if isSrc {
		srcDoc, dstDoc = curDocWithTS, theirDocWithTS
	} else {
		srcDoc, dstDoc = theirDocWithTS, curDocWithTS
	}

	mismatches, err := compareOneDocument(c.verifier.docCompareMethod, srcDoc.Doc, dstDoc.Doc, c.namespace)
	if err != nil {
		return errors.Wrap(err, "failed to compare documents")
	}

	if len(mismatches) == 0 {
		return nil
	}

	firstMismatchTime := c.firstMismatchTimeLookup.get(srcDoc.Doc)
	for i := range mismatches {
		mismatches[i].MismatchHistory = createMismatchTimes(firstMismatchTime)
		mismatches[i].SrcTimestamp = option.Some(srcDoc.TS)
		mismatches[i].DstTimestamp = option.Some(dstDoc.TS)

		c.cachedVariableBytes += types.ByteCount(len(mismatches[i].ID.Value))
	}

	c.problems = append(c.problems, mismatches...)
	return nil
}

// Will only flush when appropriate.
// Returns a string description of why it flushed.
func (c *comparator) flushIfNeeded(
	ctx context.Context,
	reportChan chan<- DocCompareReport,
) (option.Option[string], error) {
	totalProblems := c.countUnpairedDocs() + len(c.problems)

	var whyFlush option.Option[string]

	whyFlush = lo.Ternary(
		totalProblems >= comparatorMaxProblemsLen,
		option.Some("problems count"),
		lo.Ternary(
			c.cachedVariableBytes >= comparatorMaxVariableBytes,
			option.Some("memory usage"),
			whyFlush,
		),
	)

	if whyFlush.IsNone() {
		return option.None[string](), nil
	}

	return whyFlush, c.flush(ctx, reportChan)
}

// Returns the # of problems flushed. Always flushes.
func (c *comparator) flush(
	ctx context.Context,
	reportChan chan<- DocCompareReport,
) error {
	c.sweepMissingDocs()

	probsToFlush := c.problems

	err := chanutil.WriteWithDoneCheck(
		ctx,
		reportChan,
		DocCompareReport{
			Problems:  probsToFlush,
			DocCount:  c.srcDocCount,
			ByteCount: c.srcByteCount,
		},
	)
	if err != nil {
		return errors.Wrapf(err, "flushing %d problems", len(probsToFlush))
	}

	c.problems = nil
	clear(c.srcCache)
	clear(c.dstCache)

	c.srcDocCount = 0
	c.srcByteCount = 0
	c.cachedVariableBytes = 0

	c.publishHistoryCounts()

	return nil
}

func (c *comparator) countUnpairedDocs() int {
	return len(c.srcCache) + len(c.dstCache)
}

// sweepMissingDocs processes the remaining unpaired documents in the caches.
func (c *comparator) sweepMissingDocs() {
	c.problems = slices.Grow(c.problems, c.countUnpairedDocs())

	for _, docWithTS := range c.srcCache {
		firstMismatchTime := c.firstMismatchTimeLookup.get(docWithTS.Doc)
		c.problems = append(c.problems, compare.Result{
			ID:              lo.Must(c.verifier.docCompareMethod.ClonedDocIDForComparison(docWithTS.Doc)),
			Details:         compare.Missing,
			Cluster:         constants.ClusterTarget,
			NameSpace:       c.namespace,
			DataSize:        int32(len(docWithTS.Doc)),
			SrcTimestamp:    option.Some(docWithTS.TS),
			MismatchHistory: createMismatchTimes(firstMismatchTime),
		})
		docWithTS.PutInPool()
	}

	for _, docWithTS := range c.dstCache {
		firstMismatchTime := c.firstMismatchTimeLookup.get(docWithTS.Doc)
		c.problems = append(c.problems, compare.Result{
			ID:              lo.Must(c.verifier.docCompareMethod.ClonedDocIDForComparison(docWithTS.Doc)),
			Details:         compare.Missing,
			Cluster:         constants.ClusterSource,
			NameSpace:       c.namespace,
			DataSize:        int32(len(docWithTS.Doc)),
			DstTimestamp:    option.Some(docWithTS.TS),
			MismatchHistory: createMismatchTimes(firstMismatchTime),
		})
		docWithTS.PutInPool()
	}
}
