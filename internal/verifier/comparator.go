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
	src       comparatorInput
	dst       comparatorInput
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

		src: comparatorInput{cluster: src},
		dst: comparatorInput{cluster: dst},

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

// comparatorInput tracks one reader’s channel to the comparator.
type comparatorInput struct {
	// cluster names the reader in messages.
	cluster whichCluster

	closed bool

	// The reader’s last-sent batch-end message. When this is set, the
	// comparator reads only from the other reader until it also receives a
	// batch-end message. The comparator asserts that the two messages match.
	finishedNaturalBatch option.Option[compare.BatchDoneComparatorMsg]
}

// stillReading indicates whether the comparator should read from this input:
// it is neither closed nor waiting for the other reader to finish a natural
// batch.
func (in *comparatorInput) stillReading() bool {
	return !in.closed && in.finishedNaturalBatch.IsNone()
}

// readBatches reads up to 1 batch from both src & dst channels. It returns
// those batches plus a flag to indicate whether a natural batch boundary is
// reached.
func (c *comparator) readBatches(
	ctx context.Context,
	srcChannel, dstChannel <-chan compare.ToComparatorMsg,
) ([]compare.DocWithTS, []compare.DocWithTS, bool, error) {
	srcBatch, err := c.readOne(ctx, &c.src, srcChannel)
	if err != nil {
		return nil, nil, false, err
	}

	if srcBatch != nil {
		c.recordSrcMetrics(srcBatch)
	}

	dstBatch, err := c.readOne(ctx, &c.dst, dstChannel)
	if err != nil {
		return nil, nil, false, err
	}

	var naturalBatchDone bool

	srcBatchNum, srcDone := c.src.finishedNaturalBatch.Get()
	dstBatchNum, dstDone := c.dst.finishedNaturalBatch.Get()

	if srcDone && dstDone {
		lo.Assertf(
			srcBatchNum == dstBatchNum,
			"src finished natural batch %d, but dst finished batch %d",
			srcBatchNum,
			dstBatchNum,
		)

		naturalBatchDone = true
		c.src.finishedNaturalBatch = option.None[compare.BatchDoneComparatorMsg]()
		c.dst.finishedNaturalBatch = option.None[compare.BatchDoneComparatorMsg]()
	}

	return srcBatch, dstBatch, naturalBatchDone, nil
}

// readOne reads one message from ch, unless the input is closed or waiting
// for the other reader to finish a natural batch. It returns the message’s
// documents, if any.
//
// Each read gets the full read timeout, so a slow read from one reader
// doesn’t shorten the wait for the other.
func (c *comparator) readOne(
	ctx context.Context,
	in *comparatorInput,
	ch <-chan compare.ToComparatorMsg,
) ([]compare.DocWithTS, error) {
	if !in.stillReading() {
		return nil, nil
	}

	c.readTimer.Reset(readTimeout)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.readTimer.C:
		return nil, errors.Errorf("failed to read from %s within %s", in.cluster, readTimeout)
	case msg, alive := <-ch:
		if !alive {
			in.closed = true

			return nil, nil
		}

		c.fi.NoteSuccess("received message from %s reader", in.cluster)

		switch payload := msg.(type) {
		case compare.DocsToComparatorMsg:
			lo.Assertf(
				len(payload) <= compare.ToComparatorBatchSize,
				"%s reader should send <= %d docs but sent %d",
				in.cluster,
				compare.ToComparatorBatchSize,
				len(payload),
			)

			return payload, nil
		case compare.BatchDoneComparatorMsg:
			in.finishedNaturalBatch = option.Some(payload)

			return nil, nil
		default:
			panic(fmt.Sprintf("bad message (%T) from %s reader", msg, in.cluster))
		}
	}
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

// flush sends a report of the comparator’s current state. It always flushes.
func (c *comparator) flush(
	ctx context.Context,
	reportChan chan<- DocCompareReport,
) error {
	report := c.takeReport()

	err := chanutil.WriteWithDoneCheck(ctx, reportChan, report)
	if err != nil {
		return errors.Wrapf(err, "flushing %d problems", len(report.Problems))
	}

	return nil
}

// takeReport sweeps unpaired documents into the problems list, then returns
// a report of the comparator’s current state & resets that state.
func (c *comparator) takeReport() DocCompareReport {
	c.sweepMissingDocs()

	report := DocCompareReport{
		Problems:  c.problems,
		DocCount:  c.srcDocCount,
		ByteCount: c.srcByteCount,
	}

	c.problems = nil
	clear(c.srcCache)
	clear(c.dstCache)

	c.srcDocCount = 0
	c.srcByteCount = 0
	c.cachedVariableBytes = 0

	c.publishHistoryCounts()

	return report
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
