package verifier

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
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
