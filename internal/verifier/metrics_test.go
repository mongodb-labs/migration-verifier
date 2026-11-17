package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/metrics"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/option"
	prometheusclient "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	m.recordCollectionRead(ctx, src, "mydb.coll", 30*time.Millisecond, 3000)
	m.recordChangeRead(ctx, dst, time.Second, 200)

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
		"migration_verifier_change_read_duration_seconds":          prometheusclient.MetricType_HISTOGRAM,
		"migration_verifier_change_read_size_bytes":                prometheusclient.MetricType_HISTOGRAM,
		"migration_verifier_collection_read_duration_seconds":      prometheusclient.MetricType_HISTOGRAM,
		"migration_verifier_collection_read_size_bytes":            prometheusclient.MetricType_HISTOGRAM,
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

	assert.Equal(
		t,
		[]map[string]string{{"cluster": "destination"}},
		histogramLabelSets(families["migration_verifier_change_read_duration_seconds"], "cluster", "namespace"),
		"change reads have no namespace",
	)

	assert.Equal(
		t,
		[]map[string]string{{"cluster": "source", "namespace": "mydb.coll"}},
		histogramLabelSets(families["migration_verifier_collection_read_duration_seconds"], "cluster", "namespace"),
	)

	assert.Equal(
		t,
		map[string]float64{"source": 3000},
		histogramSumByLabel(families["migration_verifier_collection_read_size_bytes"], "cluster"),
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

	for _, cluster := range mslices.Of(src, dst) {
		// Without events, a change reader’s read returns only once its
		// await time passes.
		suite.Assert().EventuallyWithT(
			func(c *assert.CollectT) {
				families := scrapeFamilies(suite.T(), pipeline.Handler())

				for _, name := range mslices.Of(
					"migration_verifier_change_read_duration_seconds",
					"migration_verifier_change_read_size_bytes",
				) {
					assert.Contains(
						c,
						histogramLabelSets(families[name], "cluster"),
						map[string]string{"cluster": string(cluster)},
						"%s change reader’s reads are in %s",
						cluster,
						name,
					)
				}
			},
			10*maxChangeStreamAwaitTime,
			100*time.Millisecond,
		)
	}
}

// A check records its reads of each cluster’s documents, by namespace.
func (suite *IntegrationTestSuite) TestVerifierMetrics_CollectionRead() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	// More documents than a find’s default first batch (101), so that
	// reading them needs getMores.
	docs := lo.Times(300, func(i int) any { return bson.D{{"_id", i}} })
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

	for _, name := range mslices.Of(
		"migration_verifier_collection_read_duration_seconds",
		"migration_verifier_collection_read_size_bytes",
	) {
		suite.Assert().ElementsMatch(
			[]map[string]string{
				{"cluster": string(src), "namespace": dbName + ".coll"},
				{"cluster": string(dst), "namespace": dbName + ".coll"},
			},
			histogramLabelSets(families[name], "cluster", "namespace"),
			name,
		)
	}
}

// BatchReadTimer records each batch that a getMore fetches, but not the
// cursor’s first batch.
func (suite *IntegrationTestSuite) TestBatchReadTimer() {
	ctx := suite.Context()
	coll := suite.srcMongoClient.Database(suite.DBNameForTest()).Collection("coll")

	docs := lo.Times(5, func(i int) any { return bson.D{{"_id", i}} })
	_, err := coll.InsertMany(ctx, docs)
	suite.Require().NoError(err)

	docSize := len(lo.Must(bson.Marshal(docs[0])))

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(2).SetSort(bson.D{{"_id", 1}}))
	suite.Require().NoError(err)

	var batchSizes []int
	timer := compare.NewBatchReadTimer(func(_ context.Context, elapsed time.Duration, bytes int) {
		suite.Assert().Positive(elapsed)
		batchSizes = append(batchSizes, bytes)
	})

	count := 0
	for timer.Next(ctx, cursor) {
		count++
	}
	suite.Require().NoError(cursor.Err())
	suite.Require().Equal(5, count)

	suite.Assert().Equal(
		[]int{2 * docSize, docSize},
		batchSizes,
		"the batches after the first (2 documents, then 1) are recorded",
	)
}

// The verifier learns each replica set’s name from its cluster info.
func (suite *IntegrationTestSuite) TestVerifierReplSetNames() {
	verifier := suite.BuildVerifier()

	for _, tc := range []struct {
		info *util.ClusterInfo
		name option.Option[string]
	}{
		{verifier.srcClusterInfo, verifier.GetSrcReplSetName()},
		{verifier.dstClusterInfo, verifier.GetDstReplSetName()},
	} {
		if tc.info.Topology == util.TopologyReplset {
			suite.Assert().NotEmpty(tc.name.OrZero(), "a replica set has a name")
		} else {
			suite.Assert().Equal(option.None[string](), tc.name, "only a replica set has a name")
		}
	}
}

// histogramSumByLabel returns each histogram sample’s sum, keyed by the
// sample’s value for label.
func histogramSumByLabel(family *prometheusclient.MetricFamily, label string) map[string]float64 {
	sums := map[string]float64{}

	for _, m := range family.GetMetric() {
		key := ""
		for _, pair := range m.GetLabel() {
			if pair.GetName() == label {
				key = pair.GetValue()
			}
		}

		sums[key] += m.GetHistogram().GetSampleSum()
	}

	return sums
}

// histogramLabelSets returns each histogram sample’s values for labels. A
// sample that lacks a label has no entry for it.
func histogramLabelSets(family *prometheusclient.MetricFamily, labels ...string) []map[string]string {
	var sets []map[string]string

	for _, m := range family.GetMetric() {
		set := map[string]string{}
		for _, pair := range m.GetLabel() {
			if slices.Contains(labels, pair.GetName()) {
				set[pair.GetName()] = pair.GetValue()
			}
		}

		sets = append(sets, set)
	}

	return sets
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
