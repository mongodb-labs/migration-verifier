package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
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
	// gen0Stats fresh — that is what exercises the fix.
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

	// Without the fix, getComparisonStatistics would have added fakeWorkerDocs
	// and fakeWorkerBytes to gen0Stats unconditionally.
	suite.Assert().EqualValues(expectedDocs, gen0Stats.DocsCompared,
		"gen0Stats.DocsCompared must not include live gen-1 worker counts")
	suite.Assert().EqualValues(expectedBytes, gen0Stats.SrcBytesCompared,
		"gen0Stats.SrcBytesCompared must not include live gen-1 worker counts")
}
