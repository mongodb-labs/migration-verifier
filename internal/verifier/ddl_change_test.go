package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (suite *IntegrationTestSuite) TestDDLChangeEvents_Shard() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		suite.T().Skip("need sharded source")
	}

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		db := client.Database(dbName)
		coll := db.Collection("mycoll")
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	suite.Require().NoError(
		suite.srcMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard collection",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard collection on dst",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see metadata matches after dst shardCollection",
	)
}

func (suite *IntegrationTestSuite) TestDDLChangeEvents_ShardKeyMismatch() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		suite.T().Skip("need sharded source")
	}

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		_, err := client.Database(dbName).Collection("mycoll").Indexes().CreateMany(
			ctx,
			[]mongo.IndexModel{
				{
					Keys: bson.D{{"foo", 1}},
				},
				{
					Keys: bson.D{{"foo", 1}, {"bar", 1}},
				},
			},
		)
		suite.Require().NoError(err, "should create indexes on %v", client)
	}

	suite.Require().NoError(
		suite.srcMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard source ",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}, {"bar", 1}}},
			},
		).Err(),
		"should shard destination with different shard key",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)
}

func (suite *IntegrationTestSuite) TestDDLChangeEvents_Index() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		db := client.Database(dbName)
		coll := db.Collection("mycoll")
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	indexKey := bson.D{
		{"foo", 1},
		{"barbar", 1},
	}

	indexName, err := suite.srcMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: indexKey,
		},
	)
	suite.Require().NoError(err, "should create src index")

	suite.T().Logf("created index: %s", indexName)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)

	_, err = suite.dstMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: indexKey,
		},
	)
	suite.Require().NoError(err, "should create dst index")

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks == 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see no metadata mismatches after dst index creation",
	)

	suite.Require().NoError(
		suite.srcMongoClient.Database(dbName).RunCommand(
			ctx,
			bson.D{
				{"collMod", "mycoll"},
				{"index", bson.D{
					{"name", indexName},
					{"hidden", true},
				}},
			},
		).Err(),
		"should hide index on source",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch after hiding source index",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).RunCommand(
			ctx,
			bson.D{
				{"collMod", "mycoll"},
				{"index", bson.D{
					{"name", indexName},
					{"hidden", true},
				}},
			},
		).Err(),
		"should hide index on destination",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks == 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see no metadata mismatches after hiding dst index",
	)
}
