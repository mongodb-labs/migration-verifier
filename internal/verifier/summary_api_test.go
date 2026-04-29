package verifier

import (
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/option"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestComputeCheckETASeconds covers each branch of the ETA formula. The
// "no ETA available" branches (wrong phase, no measured rate yet) must
// return None so the response surfaces BSON null rather than a misleading
// zero. A genuinely-finished check returns Some(0).
func TestComputeCheckETASeconds(t *testing.T) {
	cases := []struct {
		name string
		prog api.Progress
		want option.Option[float64]
	}{
		{
			name: "Recheck phase returns None",
			prog: api.Progress{
				Phase:                     Recheck,
				SrcBytesComparedPerSecond: 1024,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes:    2048,
					SrcBytesCompared: 0,
				},
			},
			want: option.None[float64](),
		},
		{
			name: "Idle phase returns None",
			prog: api.Progress{
				Phase:                     Idle,
				SrcBytesComparedPerSecond: 1024,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes: 2048,
				},
			},
			want: option.None[float64](),
		},
		{
			name: "zero rate returns None",
			prog: api.Progress{
				Phase:                     Check,
				SrcBytesComparedPerSecond: 0,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes: 2048,
				},
			},
			want: option.None[float64](),
		},
		{
			name: "no bytes pending returns Some(0)",
			prog: api.Progress{
				Phase:                     Check,
				SrcBytesComparedPerSecond: 1024,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes:    2048,
					SrcBytesCompared: 2048,
				},
			},
			want: option.Some[float64](0),
		},
		{
			name: "compared exceeds total returns Some(0)",
			prog: api.Progress{
				Phase:                     Check,
				SrcBytesComparedPerSecond: 1024,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes:    1000,
					SrcBytesCompared: 1500,
				},
			},
			want: option.Some[float64](0),
		},
		{
			name: "1024 pending bytes / 512 per sec = Some(2)",
			prog: api.Progress{
				Phase:                     Check,
				SrcBytesComparedPerSecond: 512,
				GenerationStats: api.ProgressGenerationStats{
					TotalSrcBytes:    2048,
					SrcBytesCompared: 1024,
				},
			},
			want: option.Some(2.0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCheckETASeconds(tc.prog)
			gotVal, gotHas := got.Get()
			wantVal, wantHas := tc.want.Get()

			assert.Equal(t, wantHas, gotHas, "Some/None should match")
			if wantHas {
				assert.InDelta(t, wantVal, gotVal, 1e-9)
			}
		})
	}
}

// TestBuildCheckStats verifies that during generation 0 the current
// GenerationStats is returned, and that for later generations the cached
// Gen0Stats is returned (or None when the cache is absent).
func TestBuildCheckStats(t *testing.T) {
	gen0 := api.ProgressGenerationStats{
		DocsCompared:     10,
		TotalDocs:        10,
		SrcBytesCompared: 1024,
		TotalSrcBytes:    1024,
		TotalNamespaces:  1,
	}

	gen1Live := api.ProgressGenerationStats{
		DocsCompared: 5,
		TotalDocs:    5,
	}

	t.Run("generation 0 returns current GenerationStats", func(t *testing.T) {
		got := buildCheckStats(api.Progress{
			Generation:      0,
			GenerationStats: gen0,
		})
		stats, has := got.Get()
		assert.True(t, has, "CheckStats should be present in generation 0")
		assert.Equal(t, 10, stats.DocsCompared)
		assert.Equal(t, 10, stats.TotalDocs)
		assert.Equal(t, 1024, stats.SrcBytesCompared)
		assert.Equal(t, 1024, stats.TotalSrcBytes)
		assert.Equal(t, 1, stats.TotalNamespaces)
	})

	t.Run("generation 1+ with cached Gen0Stats returns cached values", func(t *testing.T) {
		got := buildCheckStats(api.Progress{
			Generation:      1,
			GenerationStats: gen1Live,
			Gen0Stats:       option.Some(gen0),
		})
		stats, has := got.Get()
		assert.True(t, has, "CheckStats should reflect cached Gen0Stats")
		assert.Equal(t, 10, stats.DocsCompared, "should be gen0 cache, not live gen1 stats")
		assert.Equal(t, 1024, stats.SrcBytesCompared, "should be gen0 cache, not live gen1 stats")
	})

	t.Run("generation 1+ without cache returns None", func(t *testing.T) {
		got := buildCheckStats(api.Progress{
			Generation:      1,
			GenerationStats: gen1Live,
			Gen0Stats:       option.None[api.ProgressGenerationStats](),
		})
		assert.True(t, got.IsNone(), "CheckStats should be absent when there is no cached Gen0Stats")
	})
}

// TestBuildNotes covers the two states: a filter is applied (yields one note)
// or it isn't (yields nil).
func TestBuildNotes(t *testing.T) {
	t.Run("no minDurationSecs returns nil", func(t *testing.T) {
		assert.Nil(t, buildNotes(0))
	})

	t.Run("minDurationSecs returns one descriptive note", func(t *testing.T) {
		notes := buildNotes(2)
		assert.Len(t, notes, 1)
		assert.Contains(t, notes[0], "2", "note should mention the filter value")
		assert.Contains(t, notes[0], "minimum duration")
	})
}

// TestGetSummary_CheckStatsAcrossGenerations is the integration-level
// counterpart of TestGetProgress_Gen0Stats: it verifies that CheckStats
// reflects the in-flight generation during gen 0 and the cached final gen-0
// stats once gen 1 begins.
func (suite *IntegrationTestSuite) TestGetSummary_CheckStatsAcrossGenerations() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	const numDocs = 5
	docs := make([]any, numDocs)
	for i := range docs {
		docs[i] = bson.D{{"_id", i}}
	}

	_, err := suite.srcMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = suite.dstMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// During gen 0 CheckStats should reflect the just-completed comparison.
	summary, err := verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)

	gen0Stats, has := summary.CheckStats.Get()
	suite.Require().True(has, "CheckStats should be present during generation 0")
	suite.Assert().Equal(numDocs, gen0Stats.TotalDocs)
	suite.Assert().Equal(numDocs, gen0Stats.DocsCompared)
	suite.Assert().Equal(1, gen0Stats.TotalNamespaces)
	suite.Assert().NotZero(gen0Stats.TotalSrcBytes)

	// Move to gen 1; CheckStats must still be the gen-0 numbers (cached).
	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().Eventually(func() bool {
		p, err := verifier.GetProgress(ctx)
		suite.Require().NoError(err)
		return p.Generation == 1
	}, time.Minute, time.Millisecond, "should reach generation 1")

	summary, err = verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)

	gen1View, has := summary.CheckStats.Get()
	suite.Require().True(has, "CheckStats should still be present in generation 1+")
	suite.Assert().Equal(gen0Stats, gen1View, "CheckStats should be the cached gen-0 values once we leave generation 0")
}

// TestGetSummary_DocMismatches verifies that document mismatches discovered in
// a comparison flow through to DocMismatches: a total count plus per-type and
// per-namespace tallies.
func (suite *IntegrationTestSuite) TestGetSummary_DocMismatches() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	srcColl := suite.srcMongoClient.Database(dbName).Collection("stuff")
	dstColl := suite.dstMongoClient.Database(dbName).Collection("stuff")

	// One doc only on source (missing on dst), one only on dst (extra), and
	// one on both with differing content (content mismatch).
	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", "onSrcOnly"}})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", "onDstOnly"}})
	suite.Require().NoError(err)
	_, err = srcColl.InsertOne(ctx, bson.D{{"_id", "onBoth"}, {"x", 1}})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", "onBoth"}, {"x", 2}})
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	summary, err := verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)

	suite.Assert().Equal(3, summary.DocMismatches.Total)

	ns := dbName + ".stuff"
	suite.Assert().Equal(3, summary.DocMismatches.ByNamespace[ns],
		"all three mismatches should be in this namespace")

	suite.Assert().Equal(1, summary.DocMismatches.ByType[string(api.MismatchMissing)])
	suite.Assert().Equal(1, summary.DocMismatches.ByType[string(api.MismatchExtra)])
	suite.Assert().Equal(1, summary.DocMismatches.ByType[string(api.MismatchContent)])
}

// TestGetSummary_EmptyMismatchesAreNonNilSlice guards against the API field
// serializing as JSON null when there are no mismatches: GetSummary must
// substitute an empty slice so the response shape is stable.
func (suite *IntegrationTestSuite) TestGetSummary_EmptyMismatchesAreNonNilSlice() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	docs := []any{bson.D{{"_id", 1}}}
	_, err := suite.srcMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = suite.dstMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	summary, err := verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)

	suite.Assert().NotNil(summary.NSMismatches, "NSMismatches must be a non-nil slice even when empty")
	suite.Assert().Empty(summary.NSMismatches)
	suite.Assert().Zero(summary.DocMismatches.Total)
}

// TestGetSummary_TotalRechecksAndChangeEvents is the summary counterpart of
// TestGetProgress_CountTotalRechecks: TotalRechecks accumulates across
// generations, and SrcChangeEvents/DstChangeEvents reflect the underlying
// progress totals.
func (suite *IntegrationTestSuite) TestGetSummary_TotalRechecksAndChangeEvents() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	srcColl := suite.srcMongoClient.Database(dbName).Collection("stuff")
	dstDB := suite.dstMongoClient.Database(srcColl.Database().Name())
	suite.Require().NoError(dstDB.CreateCollection(ctx, srcColl.Name()))

	const numDocs = 25
	docs := make([]any, numDocs)
	for i := range docs {
		docs[i] = bson.D{{"_id", i}}
	}
	_, err := srcColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	summary, err := verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(0, summary.TotalRechecks, "no rechecks in gen 0")

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	progress, err := verifier.GetProgress(ctx)
	suite.Require().NoError(err)

	summary, err = verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)

	suite.Assert().EqualValues(progress.TotalRechecksDone, summary.TotalRechecks,
		"TotalRechecks should mirror progress.TotalRechecksDone")
	suite.Assert().EqualValues(numDocs, summary.TotalRechecks,
		"every gen-0 doc should have been rechecked once in gen 1")

	suite.Assert().Equal(progress.SrcChangeStats.EventCounts, summary.SrcChangeEvents)
	suite.Assert().Equal(progress.DstChangeStats.EventCounts, summary.DstChangeEvents)
}

// TestGetSummary_NotesReflectMinDurationFilter exercises the integration-level
// path for buildNotes: a Some() filter must surface as a note in the response,
// while None() must produce no notes.
func (suite *IntegrationTestSuite) TestGetSummary_NotesReflectMinDurationFilter() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	docs := []any{bson.D{{"_id", 1}}}
	_, err := suite.srcMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = suite.dstMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	summary, err := verifier.GetSummary(ctx, 0)
	suite.Require().NoError(err)
	suite.Assert().Empty(summary.Notes, "no notes when no filter is applied")

	summary, err = verifier.GetSummary(ctx, 1)
	suite.Require().NoError(err)
	suite.Require().Len(summary.Notes, 1)
	suite.Assert().Contains(summary.Notes[0], "minimum duration")
}
