package verifier

import (
	"context"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
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
	metricLabelPhase      = attribute.Key("phase")
	metricLabelStage      = attribute.Key("stage")
	metricLabelCluster    = attribute.Key("cluster")
	metricLabelOpType     = attribute.Key("op_type")
	metricLabelCursorType = attribute.Key("cursor_type")

	// metricLabelNamespace is a collection’s namespace (database.collection).
	// Unlike the other labels, its values are customer data, and there can be
	// many of them.
	metricLabelNamespace = attribute.Key("namespace")
)

// metricClusterMeta is the cluster label for the metadata cluster. The
// source and destination use their whichCluster values.
const metricClusterMeta = "metadata"

// Values of the cursor_type label.
const (
	metricCursorTypeCollection   = "collection"
	metricCursorTypeChangeStream = "change_stream"
)

// Values of the stage label.
const (
	metricStageInitial = "initial"
	metricStageRecheck = "recheck"
)

// metricOpTypeOther is the op_type label for change events of any type not in
// metricOpTypes.
const metricOpTypeOther = "other"

// metricOpTypes are the change event types that get their own op_type label.
// Others are counted as metricOpTypeOther, so the label’s values stay bounded.
var metricOpTypes = mapset.NewSet(
	"insert",
	"update",
	"replace",
	"delete",
	"create",
	"modify",
	"createIndexes",
	"dropIndexes",
	"shardCollection",
	"reshardCollection",
	"refineCollectionShardKey",
)

var metricPhases = []string{Check, Recheck, Idle}

// Bucket boundaries, in seconds.
var (
	// A generation can take from seconds to days.
	generationDurationBuckets = []float64{
		1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400, 28800, 86400,
	}

	// Database commands normally take milliseconds. The verifier warns once
	// finding a task takes more than findTaskTimeWarnThreshold. A getMore can
	// take seconds, whether to fill a large batch or to await change events.
	commandDurationBuckets = []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
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

	getMoreDuration otelmetric.Float64Histogram
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

		getMoreDuration: lo.Must(meter.Float64Histogram(
			"migration_verifier_get_more_duration",
			otelmetric.WithUnit("s"),
			otelmetric.WithDescription("How long each successful getMore took, by cluster and cursor type (collection or change_stream). Collection cursors’ getMores also have the collection’s namespace. A change stream’s getMore includes the time it waits for events."),
			otelmetric.WithExplicitBucketBoundaries(commandDurationBuckets...),
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
		if !metricOpTypes.Contains(opType) {
			opType = metricOpTypeOther
		}

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

// recordCollectionGetMore records a getMore on a collection’s cursor.
func (m *verifierMetrics) recordCollectionGetMore(
	ctx context.Context,
	cluster, namespace string,
	elapsed time.Duration,
) {
	m.getMoreDuration.Record(
		ctx,
		elapsed.Seconds(),
		otelmetric.WithAttributes(
			metricLabelCluster.String(cluster),
			metricLabelCursorType.String(metricCursorTypeCollection),
			metricLabelNamespace.String(namespace),
		),
	)
}

// recordChangeStreamGetMore records a getMore on a change stream.
func (m *verifierMetrics) recordChangeStreamGetMore(
	ctx context.Context,
	cluster string,
	elapsed time.Duration,
) {
	m.getMoreDuration.Record(
		ctx,
		elapsed.Seconds(),
		otelmetric.WithAttributes(
			metricLabelCluster.String(cluster),
			metricLabelCursorType.String(metricCursorTypeChangeStream),
		),
	)
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
