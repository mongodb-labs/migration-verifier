package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// meterName names the verifier’s meter.
const meterName = "github.com/10gen/migration-verifier/internal/verifier"

// Metric label keys. Every label except metricLabelNamespace has a small,
// fixed set of values.
const (
	metricLabelPhase   = attribute.Key("phase")
	metricLabelStage   = attribute.Key("stage")
	metricLabelCluster = attribute.Key("cluster")
	metricLabelOpType  = attribute.Key("op_type")
	metricLabelBatch   = attribute.Key("batch")

	// metricLabelNamespace is a collection’s namespace (database.collection).
	// Unlike the other labels, its values are customer data, and there can be
	// many of them.
	metricLabelNamespace = attribute.Key("namespace")
)

// Values of the stage label.
const (
	metricStageInitial = "initial"
	metricStageRecheck = "recheck"
)

// Values of the batch label.
const (
	metricBatchOpen    = "open"
	metricBatchGetMore = "getMore"
)

var metricPhases = []string{Check, Recheck, Idle}

// Bucket boundaries. Durations are in seconds.
var (
	// A generation can take from seconds to days.
	generationDurationBuckets = []float64{
		1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400, 28800, 86400,
	}

	// Database commands normally take milliseconds. The verifier warns once
	// finding a task takes more than findTaskTimeWarnThreshold. Reading a batch
	// can take seconds, whether to fill a large batch or to await change events.
	commandDurationBuckets = []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
	}

	// Batch sizes, in bytes, up to a full batch (16 MiB).
	batchSizeBuckets = []float64{
		0,
		512,

		// KiB:
		1 << 10,
		2 << 10,
		4 << 10,
		8 << 10,
		16 << 10,
		32 << 10,
		64 << 10,
		128 << 10,
		256 << 10,

		// MiB:
		1 << 20,
		2 << 20,
		4 << 20,
		8 << 20,
		16 << 20,
	}

	// Documents or events per batch. An idle change reader’s batches are
	// empty; a full batch of small documents can hold over 100,000.
	batchCountBuckets = []float64{
		0, 1, 10, 100, 1000, 10_000, 100_000,
	}
)

// verifierMetrics holds the verifier’s OTel instruments.
type verifierMetrics struct {
	meter otelmetric.Meter

	phase      otelmetric.Int64Gauge
	generation otelmetric.Int64Gauge

	comparedDocs       otelmetric.Int64Counter
	comparedBytes      otelmetric.Int64Counter
	generationDuration otelmetric.Float64Histogram

	changeEvents          otelmetric.Int64Counter
	changeReaderLag       otelmetric.Float64ObservableGauge
	changeReaderSaturated otelmetric.Float64ObservableGauge

	// changeReaderCallback observes the current change readers. It is
	// replaced whenever the change readers are.
	changeReaderCallback option.Option[otelmetric.Registration]

	findTaskDuration       otelmetric.Float64Histogram
	recheckPersistDuration otelmetric.Float64Histogram

	changeReadDuration otelmetric.Float64Histogram
	changeReadCount    otelmetric.Int64Histogram
	changeReadSize     otelmetric.Int64Histogram

	collectionReadDuration otelmetric.Float64Histogram
	collectionReadCount    otelmetric.Int64Histogram
	collectionReadSize     otelmetric.Int64Histogram
}

// newVerifierMetrics creates the verifier’s instruments. Creating an
// instrument fails only if its definition is invalid (for example, a bad
// name), which is a bug that tests catch, so failures panic.
func newVerifierMetrics(provider otelmetric.MeterProvider) *verifierMetrics {
	meter := provider.Meter(meterName)

	return &verifierMetrics{
		meter: meter,

		phase: lo.Must(meter.Int64Gauge(
			"migration_verifier_phase",
			otelmetric.WithDescription("1 for the verifier’s current phase (check, recheck, or idle), 0 for the others."),
		)),
		generation: lo.Must(meter.Int64Gauge(
			"migration_verifier_generation",
			otelmetric.WithDescription("The current generation. Generation 0 is the initial check; later generations are rechecks."),
		)),

		comparedDocs: lo.Must(meter.Int64Counter(
			"migration_verifier_compared_docs",
			otelmetric.WithUnit("{document}"),
			otelmetric.WithDescription("Source documents compared, by stage (initial or recheck)."),
		)),
		comparedBytes: lo.Must(meter.Int64Counter(
			"migration_verifier_compared_bytes",
			otelmetric.WithUnit("By"),
			otelmetric.WithDescription("Source document bytes compared, by stage (initial or recheck)."),
		)),
		generationDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_generation_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long each generation took, by stage (initial or recheck). Rechecks should get shorter as the verification converges."),
			otelmetric.WithExplicitBucketBoundaries(generationDurationBuckets...),
		)),

		changeEvents: lo.Must(meter.Int64Counter(
			"migration_verifier_change_events",
			otelmetric.WithUnit("{event}"),
			otelmetric.WithDescription("Change events read, by cluster and operation type."),
		)),
		changeReaderLag: lo.Must(meter.Float64ObservableGauge(
			"migration_verifier_change_reader_lag",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How far each cluster’s change reader is behind the cluster’s latest operation."),
		)),
		changeReaderSaturated: lo.Must(meter.Float64ObservableGauge(
			"migration_verifier_change_reader_buffer_saturation",
			otelmetric.WithUnit("1"),
			otelmetric.WithDescription("How full each cluster’s change reader’s buffer is, from 0 to 1. If it stays high, change events arrive faster than they can be persisted."),
		)),

		findTaskDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_find_task_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long a worker’s query for its next task took. Slow queries suggest that the metadata database is overloaded."),
			otelmetric.WithExplicitBucketBoundaries(commandDurationBuckets...),
		)),
		recheckPersistDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_recheck_persist_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long persisting a set of rechecks took."),
			otelmetric.WithExplicitBucketBoundaries(commandDurationBuckets...),
		)),

		changeReadDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_change_read_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long each change reader’s read of a batch of change events took, by cluster. This includes the time spent waiting for events."),
			otelmetric.WithExplicitBucketBoundaries(commandDurationBuckets...),
		)),
		changeReadCount: lo.Must(meter.Int64Histogram(
			"migration_verifier_change_read_count",
			otelmetric.WithUnit("{event}"),
			otelmetric.WithDescription("The number of change events in each batch that a change reader read, by cluster."),
			otelmetric.WithExplicitBucketBoundaries(batchCountBuckets...),
		)),
		changeReadSize: lo.Must(meter.Int64Histogram(
			"migration_verifier_change_read_size",
			otelmetric.WithUnit("By"),
			otelmetric.WithDescription("The total size of each batch of change events that a change reader read, by cluster."),
			otelmetric.WithExplicitBucketBoundaries(batchSizeBuckets...),
		)),

		collectionReadDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_collection_read_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long each read of a batch of documents to compare took, by cluster, namespace, and batch (open or getMore). An open batch’s duration is that of the command that opened the cursor."),
			otelmetric.WithExplicitBucketBoundaries(commandDurationBuckets...),
		)),
		collectionReadCount: lo.Must(meter.Int64Histogram(
			"migration_verifier_collection_read_count",
			otelmetric.WithUnit("{document}"),
			otelmetric.WithDescription("The number of documents to compare in each batch read, by cluster, namespace, and batch (first or getMore)."),
			otelmetric.WithExplicitBucketBoundaries(batchCountBuckets...),
		)),
		collectionReadSize: lo.Must(meter.Int64Histogram(
			"migration_verifier_collection_read_size",
			otelmetric.WithUnit("By"),
			otelmetric.WithDescription("The total size of each batch of documents to compare, by cluster, namespace, and batch (first or getMore)."),
			otelmetric.WithExplicitBucketBoundaries(batchSizeBuckets...),
		)),
	}
}

// recordPhase records current as the verifier’s phase.
func (m *verifierMetrics) recordPhase(ctx context.Context, current string) {
	for _, phase := range metricPhases {
		m.phase.Record(
			ctx,
			lo.Ternary[int64](phase == current, 1, 0),
			otelmetric.WithAttributes(metricLabelPhase.String(phase)),
		)
	}
}

func (m *verifierMetrics) recordGeneration(ctx context.Context, generation int) {
	m.generation.Record(ctx, int64(generation))
}

// recordCompared adds a batch of compared source documents.
func (m *verifierMetrics) recordCompared(
	ctx context.Context,
	generation int,
	docs, bytes int64,
) {
	opt := otelmetric.WithAttributes(metricLabelStage.String(metricStage(generation)))

	m.comparedDocs.Add(ctx, docs, opt)
	m.comparedBytes.Add(ctx, bytes, opt)
}

func (m *verifierMetrics) recordGenerationDuration(
	ctx context.Context,
	generation int,
	elapsed time.Duration,
) {
	m.generationDuration.Record(
		ctx,
		elapsed.Seconds(),
		otelmetric.WithAttributes(metricLabelStage.String(metricStage(generation))),
	)
}

// recordChangeEvents adds a batch’s change events, given as counts by
// operation type.
func (m *verifierMetrics) recordChangeEvents(
	ctx context.Context,
	cluster whichCluster,
	countByOpType map[string]int64,
) {
	for opType, count := range countByOpType {
		m.changeEvents.Add(
			ctx,
			count,
			otelmetric.WithAttributes(
				metricLabelCluster.String(string(cluster)),
				metricLabelOpType.String(opType),
			),
		)
	}
}

func (m *verifierMetrics) recordFindTaskDuration(ctx context.Context, elapsed time.Duration) {
	m.findTaskDuration.Record(ctx, elapsed.Seconds())
}

func (m *verifierMetrics) recordRecheckPersistDuration(ctx context.Context, elapsed time.Duration) {
	m.recheckPersistDuration.Record(ctx, elapsed.Seconds())
}

// recordChangeRead records a change reader’s read of a batch of events.
func (m *verifierMetrics) recordChangeRead(
	ctx context.Context,
	cluster whichCluster,
	read compare.BatchReadStats,
) {
	opt := otelmetric.WithAttributes(metricLabelCluster.String(string(cluster)))

	m.changeReadDuration.Record(ctx, read.Duration.Seconds(), opt)
	m.changeReadCount.Record(ctx, int64(read.Docs), opt)
	m.changeReadSize.Record(ctx, int64(read.Bytes), opt)
}

// recordCollectionRead records a read of a batch of documents to compare.
func (m *verifierMetrics) recordCollectionRead(
	ctx context.Context,
	cluster whichCluster,
	namespace string,
	read compare.BatchReadStats,
) {
	opt := otelmetric.WithAttributes(
		metricLabelCluster.String(string(cluster)),
		metricLabelNamespace.String(namespace),
		metricLabelBatch.String(lo.Ternary(read.First, metricBatchOpen, metricBatchGetMore)),
	)

	m.collectionReadDuration.Record(ctx, read.Duration.Seconds(), opt)
	m.collectionReadCount.Record(ctx, int64(read.Docs), opt)
	m.collectionReadSize.Record(ctx, int64(read.Bytes), opt)
}

// changeReaderGaugeSource is what the change reader gauges read. Its methods
// read only in-memory state, so they’re cheap enough to call on every gather.
type changeReaderGaugeSource interface {
	getWhichCluster() whichCluster
	getCurrentTimestamps() option.Option[readerCurrentTimestamps]
	getBufferSaturation() float64
}

// observeChangeReaders makes the change reader gauges observe readers,
// replacing any readers that they observed before.
func (m *verifierMetrics) observeChangeReaders(readers ...changeReaderGaugeSource) error {
	if registration, has := m.changeReaderCallback.Get(); has {
		if err := registration.Unregister(); err != nil {
			return err
		}

		m.changeReaderCallback = option.None[otelmetric.Registration]()
	}

	registration, err := m.meter.RegisterCallback(
		func(_ context.Context, observer otelmetric.Observer) error {
			for _, reader := range readers {
				opt := otelmetric.WithAttributes(
					metricLabelCluster.String(string(reader.getWhichCluster())),
				)

				if timestamps, has := reader.getCurrentTimestamps().Get(); has {
					observer.ObserveFloat64(m.changeReaderLag, timestamps.Lag().Seconds(), opt)
				}

				observer.ObserveFloat64(m.changeReaderSaturated, reader.getBufferSaturation(), opt)
			}

			return nil
		},
		m.changeReaderLag,
		m.changeReaderSaturated,
	)
	if err != nil {
		return err
	}

	m.changeReaderCallback = option.Some(registration)

	return nil
}

func metricStage(generation int) string {
	return lo.Ternary(generation == 0, metricStageInitial, metricStageRecheck)
}
