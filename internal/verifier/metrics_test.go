package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/metrics"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/option"
	prometheusclient "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type fakeChangeReaderGaugeSource struct {
	cluster    whichCluster
	timestamps option.Option[readerCurrentTimestamps]
	saturation float64
}

var _ changeReaderGaugeSource = fakeChangeReaderGaugeSource{}

func (f fakeChangeReaderGaugeSource) getWhichCluster() whichCluster { return f.cluster }

func (f fakeChangeReaderGaugeSource) getCurrentTimestamps() option.Option[readerCurrentTimestamps] {
	return f.timestamps
}

func (f fakeChangeReaderGaugeSource) getBufferSaturation() float64 { return f.saturation }

// The verifier’s instruments are valid, and they reach /metrics with the
// Prometheus names, types, and labels that dashboards rely on.
func TestVerifierMetrics_Exported(t *testing.T) {
	pipeline, err := metrics.Setup(logger.NewDebugLogger(), metrics.Settings{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pipeline.Shutdown(t.Context())) })

	m := newVerifierMetrics(pipeline.MeterProvider())
	ctx := t.Context()

	m.recordPhase(ctx, Recheck)
	m.recordGeneration(ctx, 3)
	m.recordCompared(ctx, 0, 10, 1000)
	m.recordCompared(ctx, 2, 5, 500)
	m.recordGenerationDuration(ctx, 1, 90*time.Second)
	m.recordChangeEvents(ctx, src, map[string]int64{"insert": 4, "someNewOpType": 1})
	m.recordFindTaskDuration(ctx, 20*time.Millisecond)
	m.recordRecheckPersistDuration(ctx, 200*time.Millisecond)

	require.NoError(t, m.observeChangeReaders(
		fakeChangeReaderGaugeSource{
			cluster: src,
			timestamps: option.Some(readerCurrentTimestamps{
				LastHandled:   bson.Timestamp{T: 100},
				LastOperation: bson.Timestamp{T: 107},
			}),
			saturation: 0.25,
		},
		fakeChangeReaderGaugeSource{cluster: dst},
	))

	families := scrapeFamilies(t, pipeline.Handler())

	expectedTypes := map[string]prometheusclient.MetricType{
		"migration_verifier_phase":                                 prometheusclient.MetricType_GAUGE,
		"migration_verifier_generation":                            prometheusclient.MetricType_GAUGE,
		"migration_verifier_compared_docs_total":                   prometheusclient.MetricType_COUNTER,
		"migration_verifier_compared_bytes_total":                  prometheusclient.MetricType_COUNTER,
		"migration_verifier_generation_duration_seconds":           prometheusclient.MetricType_HISTOGRAM,
		"migration_verifier_change_events_total":                   prometheusclient.MetricType_COUNTER,
		"migration_verifier_change_reader_lag_seconds":             prometheusclient.MetricType_GAUGE,
		"migration_verifier_change_reader_buffer_saturation_ratio": prometheusclient.MetricType_GAUGE,
		"migration_verifier_find_task_duration_seconds":            prometheusclient.MetricType_HISTOGRAM,
		"migration_verifier_recheck_persist_duration_seconds":      prometheusclient.MetricType_HISTOGRAM,
	}
	for name, metricType := range expectedTypes {
		if assert.Contains(t, families, name) {
			assert.Equal(t, metricType, families[name].GetType(), name)
		}
	}

	assert.Equal(
		t,
		map[string]float64{"check": 0, "recheck": 1, "idle": 0},
		valuesByLabel(families["migration_verifier_phase"], "phase"),
	)

	assert.Equal(
		t,
		map[string]float64{"initial": 10, "recheck": 5},
		valuesByLabel(families["migration_verifier_compared_docs_total"], "stage"),
	)

	assert.Equal(
		t,
		map[string]float64{"insert": 4, "someNewOpType": 1},
		valuesByLabel(families["migration_verifier_change_events_total"], "op_type"),
		"each op type is its own label value",
	)

	assert.Equal(
		t,
		map[string]float64{"source": 7},
		valuesByLabel(families["migration_verifier_change_reader_lag_seconds"], "cluster"),
		"a reader without timestamps reports no lag",
	)

	assert.Equal(
		t,
		map[string]float64{"source": 0.25, "destination": 0},
		valuesByLabel(families["migration_verifier_change_reader_buffer_saturation_ratio"], "cluster"),
	)
}

// Replacing the change readers replaces what the gauges observe.
func TestVerifierMetrics_ObserveChangeReadersReplaces(t *testing.T) {
	pipeline, err := metrics.Setup(logger.NewDebugLogger(), metrics.Settings{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pipeline.Shutdown(t.Context())) })

	m := newVerifierMetrics(pipeline.MeterProvider())

	require.NoError(t, m.observeChangeReaders(fakeChangeReaderGaugeSource{cluster: src, saturation: 0.5}))
	require.NoError(t, m.observeChangeReaders(fakeChangeReaderGaugeSource{cluster: dst, saturation: 0.75}))

	families := scrapeFamilies(t, pipeline.Handler())

	assert.Equal(
		t,
		map[string]float64{"destination": 0.75},
		valuesByLabel(families["migration_verifier_change_reader_buffer_saturation_ratio"], "cluster"),
	)
}

func scrapeFamilies(t *testing.T, handler http.Handler) map[string]*prometheusclient.MetricFamily {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(rec.Body)
	require.NoError(t, err)

	return families
}

// valuesByLabel returns each gauge or counter sample’s value, keyed by the
// sample’s value for label.
func valuesByLabel(family *prometheusclient.MetricFamily, label string) map[string]float64 {
	values := map[string]float64{}

	for _, m := range family.GetMetric() {
		for _, pair := range m.GetLabel() {
			if pair.GetName() != label {
				continue
			}

			switch {
			case m.GetCounter() != nil:
				values[pair.GetValue()] = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				values[pair.GetValue()] = m.GetGauge().GetValue()
			}
		}
	}

	return values
}

// A check plus a recheck records every Tier 1 metric with plausible values.
func (suite *IntegrationTestSuite) TestVerifierMetrics_CheckAndRecheck() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	docs := []any{bson.D{{"_id", 1}}, bson.D{{"_id", 2}}, bson.D{{"_id", 3}}}
	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		_, err := client.Database(dbName).Collection("coll").InsertMany(ctx, docs)
		suite.Require().NoError(err)
	}

	pipeline, err := metrics.Setup(logger.NewDebugLogger(), metrics.Settings{})
	suite.Require().NoError(err)
	defer func() { suite.Require().NoError(pipeline.Shutdown(context.Background())) }()

	verifier := suite.BuildVerifier()
	verifier.SetMeterProvider(pipeline.MeterProvider())
	verifier.SetSrcNamespaces([]string{dbName + ".coll"})
	verifier.SetDstNamespaces([]string{dbName + ".coll"})
	verifier.SetNamespaceMap()

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	families := scrapeFamilies(suite.T(), pipeline.Handler())
	suite.Assert().Equal(
		map[string]float64{"check": 1, "recheck": 0, "idle": 0},
		valuesByLabel(families["migration_verifier_phase"], "phase"),
		"the initial check is in progress",
	)
	suite.Assert().Equal(
		map[string]float64{"initial": 3},
		valuesByLabel(families["migration_verifier_compared_docs_total"], "stage"),
	)

	_, err = suite.srcMongoClient.Database(dbName).Collection("coll").InsertOne(ctx, bson.D{{"_id", 4}})
	suite.Require().NoError(err)

	// The change event may take a few generations to arrive.
	suite.Require().Eventually(
		func() bool {
			suite.Require().NoError(runner.StartNextGeneration())
			suite.Require().NoError(runner.AwaitGenerationEnd())

			families = scrapeFamilies(suite.T(), pipeline.Handler())
			compared := valuesByLabel(families["migration_verifier_compared_docs_total"], "stage")

			return compared["recheck"] > 0
		},
		time.Minute,
		100*time.Millisecond,
	)

	suite.Assert().Equal(
		map[string]float64{"check": 0, "recheck": 1, "idle": 0},
		valuesByLabel(families["migration_verifier_phase"], "phase"),
	)
	suite.Assert().GreaterOrEqual(
		families["migration_verifier_generation"].GetMetric()[0].GetGauge().GetValue(),
		1.0,
	)
	suite.Assert().Positive(
		valuesByLabel(families["migration_verifier_compared_bytes_total"], "stage")["initial"],
	)

	events := families["migration_verifier_change_events_total"]
	suite.Require().NotNil(events)
	suite.Assert().Positive(
		sampleValue(events, map[string]string{"cluster": "source", "op_type": "insert"}),
		"the source insert is counted",
	)

	suite.Assert().Contains(
		valuesByLabel(families["migration_verifier_change_reader_buffer_saturation_ratio"], "cluster"),
		"source",
	)
	suite.Assert().Contains(
		valuesByLabel(families["migration_verifier_change_reader_lag_seconds"], "cluster"),
		"source",
	)

	suite.Assert().Positive(
		histogramCountByLabel(families["migration_verifier_generation_duration_seconds"], "stage")["initial"],
	)
	suite.Assert().Positive(
		histogramCountByLabel(families["migration_verifier_find_task_duration_seconds"], "")[""],
	)
	suite.Assert().Positive(
		histogramCountByLabel(families["migration_verifier_recheck_persist_duration_seconds"], "")[""],
	)

	// Finish the verification.
	suite.Require().NoError(verifier.WritesOff(ctx))
	suite.Require().NoError(runner.Await())

	families = scrapeFamilies(suite.T(), pipeline.Handler())

	durationCounts := histogramCountByLabel(families["migration_verifier_generation_duration_seconds"], "stage")
	suite.Assert().EqualValues(
		verifier.generation+1,
		durationCounts["initial"]+durationCounts["recheck"],
		"each generation, including the final one, records its duration once",
	)

	suite.Assert().Equal(
		map[string]float64{"check": 0, "recheck": 0, "idle": 1},
		valuesByLabel(families["migration_verifier_phase"], "phase"),
		"a finished verification is idle",
	)
}

// sampleValue returns the value of the gauge or counter sample with all of
// labels, or 0 if there is none.
func sampleValue(family *prometheusclient.MetricFamily, labels map[string]string) float64 {
	for _, m := range family.GetMetric() {
		matched := 0
		for _, pair := range m.GetLabel() {
			if want, has := labels[pair.GetName()]; has && want == pair.GetValue() {
				matched++
			}
		}

		if matched == len(labels) {
			return m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}

	return 0
}

// histogramCountByLabel returns each histogram sample’s count, keyed by the
// sample’s value for label. If label is empty, every sample is keyed by "".
func histogramCountByLabel(family *prometheusclient.MetricFamily, label string) map[string]uint64 {
	counts := map[string]uint64{}

	for _, m := range family.GetMetric() {
		key := ""
		for _, pair := range m.GetLabel() {
			if pair.GetName() == label {
				key = pair.GetValue()
			}
		}

		counts[key] += m.GetHistogram().GetSampleCount()
	}

	return counts
}
