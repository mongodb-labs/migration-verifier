package verifier

import (
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/exp/slices"
)

// TestGetProgress_Gen0Stats verifies that gen0Stats is absent while generation
// 0 is active and is correctly populated (and cached) once generation 1 begins.
func (suite *IntegrationTestSuite) TestGetProgress_Gen0Stats() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	const numDocs = 5
	docs := make([]any, numDocs)
	for i := range docs {
		docs[i] = bson.D{{"_id", i}}
	}

	// Insert matching docs on src and dst so every task completes cleanly.
	_, err := suite.srcMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = suite.dstMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// gen0Stats must be absent while the verifier is still in generation 0.
	progress, err := verifier.GetProgress(ctx)
	suite.Require().NoError(err)
	suite.Assert().Equal(0, progress.Generation)
	suite.Assert().True(progress.Gen0Stats.IsNone(), "gen0Stats should be absent during generation 0")

	suite.Require().NoError(runner.StartNextGeneration())

	// Poll until generation 1 is reflected in /progress.
	suite.Require().Eventually(
		func() bool {
			progress, err = verifier.GetProgress(ctx)
			suite.Require().NoError(err)
			return progress.Generation == 1
		},
		time.Minute,
		time.Millisecond,
		"should reach generation 1",
	)

	gen0Stats, hasGen0Stats := progress.Gen0Stats.Get()
	suite.Assert().True(hasGen0Stats, "gen0Stats should be present once generation > 0")
	suite.Assert().EqualValues(1, gen0Stats.TotalNamespaces, "one namespace was verified")
	suite.Assert().EqualValues(numDocs, gen0Stats.TotalDocs, "total docs should match what was inserted")
	suite.Assert().EqualValues(numDocs, gen0Stats.DocsCompared, "all docs should have been compared")
	suite.Assert().NotZero(gen0Stats.TotalSrcBytes, "byte count should be non-zero")

	// A second call must return the same gen0Stats (values are cached, not re-queried).
	progress2, err := verifier.GetProgress(ctx)
	suite.Require().NoError(err)
	suite.Assert().Equal(progress.Gen0Stats, progress2.Gen0Stats, "gen0Stats should be stable across calls")
}

// TestGetProgress_Gen0StatsExcludesActiveWorkerCounts verifies that live
// in-memory worker counts for the current generation are not added to the
// cached gen0Stats.
func (suite *IntegrationTestSuite) TestGetProgress_Gen0StatsExcludesActiveWorkerCounts() {
	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	const numDocs = 3
	docs := make([]bson.D, numDocs)
	for i := range docs {
		docs[i] = bson.D{{"_id", i}}
	}

	_, err := suite.srcMongoClient.Database(dbName).Collection("coll").InsertMany(ctx, docs)
	suite.Require().NoError(err)

	// The src & dst must mismatch so that gen1 has >=1 task.
	// Otherwise /progress in gen1 will return empty.
	_, err = suite.dstMongoClient.Database(dbName).Collection("coll").InsertMany(
		ctx,
		append(
			slices.Clone(docs),
			bson.D{{"_id", "extra"}},
		),
	)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())
	suite.Require().NoError(runner.StartNextGeneration())

	suite.Require().Eventually(
		func() bool {
			p, err := verifier.GetProgress(ctx)
			suite.Require().NoError(err)
			return p.Generation == 1
		},
		time.Minute,
		time.Millisecond,
		"should reach generation 1",
	)

	// Read the persisted gen 0 counts directly — these are the ground truth.
	gen0NSStats, err := verifier.GetPersistedNamespaceStatistics(ctx, 0)
	suite.Require().NoError(err)
	suite.Require().NotEmpty(gen0NSStats)

	var expectedDocs types.DocumentCount
	var expectedBytes types.ByteCount
	for _, ns := range gen0NSStats {
		expectedDocs += ns.DocsCompared
		expectedBytes += ns.BytesCompared
	}

	// The Eventually loop above already triggered a GetProgress call that
	// populated cachedGen0Stats. Clear it so the call below recomputes
	// gen0Stats fresh.
	verifier.cachedGen0Stats.Store(nil)

	// Inject fake in-progress counts. Use a high-numbered worker slot to avoid
	// colliding with real gen-1 workers, which start from slot 0.
	const fakeSlot = 999
	const fakeWorkerDocs = types.DocumentCount(999)
	const fakeWorkerBytes = types.ByteCount(99999)
	fakeTask := tasks.Task{
		PrimaryKey:  bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{Namespace: dbName + ".coll"},
	}
	verifier.workerTracker.Set(fakeSlot, fakeTask)
	verifier.workerTracker.SetSrcCounts(fakeSlot, fakeWorkerDocs, fakeWorkerBytes)
	defer verifier.workerTracker.Unset(fakeSlot)

	progress, err := verifier.GetProgress(ctx)
	suite.Require().NoError(err)

	gen0Stats, hasGen0Stats := progress.Gen0Stats.Get()
	suite.Require().True(hasGen0Stats, "gen0Stats must be present in generation 1+")

	// Ensure that getComparisonStatistics doesn’t add fakeWorkerDocs
	// and fakeWorkerBytes to gen0Stats unconditionally.
	suite.Assert().EqualValues(expectedDocs, gen0Stats.DocsCompared,
		"gen0Stats.DocsCompared must not include live gen-1 worker counts")
	suite.Assert().EqualValues(expectedBytes, gen0Stats.SrcBytesCompared,
		"gen0Stats.SrcBytesCompared must not include live gen-1 worker counts")
}

// TestGetProgress_ChangeEventCountsPersistAcrossRestarts verifies that
// cumulative change event tallies survive verifier restarts: counts are
// restored from the persisted resume-token document and new events are
// accumulated on top.
func (suite *IntegrationTestSuite) TestGetProgress_ChangeEventCountsPersistAcrossRestarts() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	srcColl := suite.srcMongoClient.Database(dbName).Collection("coll")
	dstColl := suite.dstMongoClient.Database(dbName).Collection("coll")

	suite.Require().NoError(
		suite.srcMongoClient.Database(dbName).CreateCollection(ctx, "coll"),
	)
	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).CreateCollection(ctx, "coll"),
	)

	// Insert matching docs so gen 0 tasks complete without mismatches.
	initialDocs := []any{bson.D{{"_id", 0}}}
	_, err := srcColl.InsertMany(ctx, initialDocs)
	suite.Require().NoError(err)
	_, err = dstColl.InsertMany(ctx, initialDocs)
	suite.Require().NoError(err)

	v1Ctx, v1Cancel := contextplus.WithCancelCause(ctx)
	defer v1Cancel(nil)

	verifier1 := suite.BuildVerifier()
	verifier1.SetVerifyAll(true)

	runner1 := RunVerifierCheck(v1Ctx, suite.T(), verifier1)
	suite.Require().NoError(runner1.AwaitGenerationEnd())

	// Insert change events on both src and dst after gen 0 completes.
	_, err = srcColl.InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = srcColl.InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", 3}})
	suite.Require().NoError(err)

	const wantSrcInserts = 2
	const wantDstInserts = 1

	// Wait for GetProgress to reflect both inserts. Use >= to tolerate extra
	// events the oplog reader may have seen before verification started.
	var progress1 api.Progress
	suite.Require().Eventually(func() bool {
		var err error
		progress1, err = verifier1.GetProgress(ctx)
		suite.Require().NoError(err)
		return progress1.SrcChangeStats.EventCounts.Insert >= wantSrcInserts &&
			progress1.DstChangeStats.EventCounts.Insert >= wantDstInserts
	}, time.Minute, time.Millisecond,
		"GetProgress should reflect src inserts (≥%d) and dst inserts (≥%d)",
		wantSrcInserts, wantDstInserts,
	)

	v1SrcCounts := progress1.SrcChangeStats.EventCounts
	v1DstCounts := progress1.DstChangeStats.EventCounts

	// The in-memory reader counts must be >= the GetProgress snapshot (the
	// snapshot could be slightly stale relative to the live counters).
	suite.Assert().GreaterOrEqual(
		verifier1.srcChangeReader.GetCumulativeEventCounts().Insert, v1SrcCounts.Insert,
		"src reader counts should be >= GetProgress snapshot",
	)
	suite.Assert().GreaterOrEqual(
		verifier1.dstChangeReader.GetCumulativeEventCounts().Insert, v1DstCounts.Insert,
		"dst reader counts should be >= GetProgress snapshot",
	)

	// Wait for the counts to be written to MongoDB alongside the resume token.
	// BuildVerifier sets resumeTokenPersistInterval to 10ms, so batches are persisted frequently.
	metaDB := verifier1.metaClient.Database(verifier1.metaDBName)
	changeReaderColl := metaDB.Collection(changeReaderCollectionName)

	waitForPersistedCounts := func(wantSrc, wantDst int) {
		suite.T().Helper()
		suite.Require().Eventually(func() bool {
			var srcDoc, dstDoc resumeTokenDoc
			srcErr := changeReaderColl.FindOne(ctx, bson.D{{"_id", "srcResumeToken"}}).Decode(&srcDoc)
			dstErr := changeReaderColl.FindOne(ctx, bson.D{{"_id", "dstResumeToken"}}).Decode(&dstDoc)
			return srcErr == nil && dstErr == nil &&
				srcDoc.EventCounts.Insert >= wantSrc &&
				dstDoc.EventCounts.Insert >= wantDst
		},
			time.Minute,
			time.Millisecond,
			"event counts (src≥%d, dst≥%d) should be written to metaDB", wantSrc, wantDst,
		)
	}

	waitForPersistedCounts(wantSrcInserts, wantDstInserts)

	v1Cancel(fmt.Errorf("stopping verifier 1"))

	// --- Restart 1: verifier2 should restore verifier1's persisted counts ---

	v2Ctx, v2Cancel := contextplus.WithCancelCause(ctx)
	defer v2Cancel(nil)

	verifier2 := suite.BuildVerifier()
	verifier2.SetVerifyAll(true)

	runner2 := RunVerifierCheck(v2Ctx, suite.T(), verifier2)
	suite.Require().NoError(runner2.AwaitGenerationEnd())

	// GetProgress should expose counts >= those persisted by verifier1.
	progress2, err := verifier2.GetProgress(ctx)
	suite.Require().NoError(err)
	suite.Assert().GreaterOrEqual(progress2.SrcChangeStats.EventCounts.Insert, v1SrcCounts.Insert,
		"verifier2 should restore src insert counts from persisted state")
	suite.Assert().GreaterOrEqual(progress2.DstChangeStats.EventCounts.Insert, v1DstCounts.Insert,
		"verifier2 should restore dst insert counts from persisted state")

	// The in-memory reader counts must be >= the GetProgress snapshot.
	suite.Assert().GreaterOrEqual(
		verifier2.srcChangeReader.GetCumulativeEventCounts().Insert,
		progress2.SrcChangeStats.EventCounts.Insert,
	)
	suite.Assert().GreaterOrEqual(
		verifier2.dstChangeReader.GetCumulativeEventCounts().Insert,
		progress2.DstChangeStats.EventCounts.Insert,
	)

	// Add more events and confirm they accumulate on top of restored counts.
	_, err = srcColl.InsertOne(ctx, bson.D{{"_id", 4}})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", 5}})
	suite.Require().NoError(err)

	// Use GetProgress as a consistent snapshot to avoid TOCTOU between reader
	// and progress reads.
	var progress2Final api.Progress
	suite.Require().Eventually(func() bool {
		var err error
		progress2Final, err = verifier2.GetProgress(ctx)
		suite.Require().NoError(err)
		return progress2Final.SrcChangeStats.EventCounts.Insert > progress2.SrcChangeStats.EventCounts.Insert &&
			progress2Final.DstChangeStats.EventCounts.Insert > progress2.DstChangeStats.EventCounts.Insert
	}, time.Minute, time.Millisecond, "verifier2 should tally new inserts on top of restored counts")

	waitForPersistedCounts(progress2Final.SrcChangeStats.EventCounts.Insert, progress2Final.DstChangeStats.EventCounts.Insert)

	v2Cancel(fmt.Errorf("stopping verifier 2"))

	// --- Restart 2: verifier3 should restore verifier2's final counts ---

	verifier3 := suite.BuildVerifier()
	verifier3.SetVerifyAll(true)

	runner3 := RunVerifierCheck(ctx, suite.T(), verifier3)
	suite.Require().NoError(runner3.AwaitGenerationEnd())

	// GetProgress should expose counts >= verifier2's final persisted counts.
	progress3, err := verifier3.GetProgress(ctx)
	suite.Require().NoError(err)
	suite.Assert().GreaterOrEqual(progress3.SrcChangeStats.EventCounts.Insert,
		progress2Final.SrcChangeStats.EventCounts.Insert,
		"verifier3 should restore verifier2's final src counts")
	suite.Assert().GreaterOrEqual(progress3.DstChangeStats.EventCounts.Insert,
		progress2Final.DstChangeStats.EventCounts.Insert,
		"verifier3 should restore verifier2's final dst counts")

	// In-memory reader counts must be >= the GetProgress snapshot.
	suite.Assert().GreaterOrEqual(
		verifier3.srcChangeReader.GetCumulativeEventCounts().Insert,
		progress3.SrcChangeStats.EventCounts.Insert,
	)
	suite.Assert().GreaterOrEqual(
		verifier3.dstChangeReader.GetCumulativeEventCounts().Insert,
		progress3.DstChangeStats.EventCounts.Insert,
	)

	_ = runner1
	_ = runner2
	_ = runner3
}
