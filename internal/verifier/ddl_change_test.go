package verifier

import (
	"strings"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestDDLChangeEvents_Index verifies that when createIndexes occurs on the
// source while the verifier runs in warnMost mode, a warning is logged.
func (suite *IntegrationTestSuite) TestDDLChangeEvents_Index() {
	suite.SkipUnlessSrcHasDDLEvents()

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		suite.Require().NoError(client.Database(dbName).CreateCollection(ctx, "mycoll"))
	}

	verifier, logBuf := suite.BuildVerifierWarnMost()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	_, err := suite.srcMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{Keys: bson.D{{"x", 1}}},
	)
	suite.Require().NoError(err, "should create src index")

	suite.Assert().Eventually(
		func() bool {
			if strings.Contains(string(logBuf.Bytes()), "DDL event detected on source") {
				return true
			}
			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())
			return false
		},
		time.Minute,
		time.Second,
		"should see a DDL warning in logs",
	)
}

// TestDDLChangeEvents_LastRecheckedTimestampUnaffected verifies that DDL
// events do not update srcLastRecheckedTS or dstLastRecheckedTS — those must
// stay zero because DDL events never produce recheck tasks.
func (suite *IntegrationTestSuite) TestDDLChangeEvents_LastRecheckedTimestampUnaffected() {
	suite.SkipUnlessSrcHasDDLEvents()

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		suite.Require().NoError(client.Database(dbName).CreateCollection(ctx, "mycoll"))
	}

	verifier, logBuf := suite.BuildVerifierWarnMost()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	_, err := suite.srcMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{Keys: bson.D{{"x", 1}}},
	)
	suite.Require().NoError(err, "should create src index")

	// Run until the warning appears, confirming the event was processed.
	suite.Assert().Eventually(
		func() bool {
			if strings.Contains(string(logBuf.Bytes()), "DDL event detected on source") {
				return true
			}
			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())
			return false
		},
		time.Minute,
		time.Second,
		"should see a DDL warning in logs",
	)

	// DDL events must not update the last-rechecked timestamps.
	var srcTS, dstTS bson.Timestamp
	verifier.srcLastRecheckedTS.Load(func(t bson.Timestamp) { srcTS = t })
	verifier.dstLastRecheckedTS.Load(func(t bson.Timestamp) { dstTS = t })
	suite.Assert().True(srcTS.IsZero(), "srcLastRecheckedTS should remain zero after DDL-only events")
	suite.Assert().True(dstTS.IsZero(), "dstLastRecheckedTS should remain zero after DDL-only events")
}
