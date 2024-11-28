package verifier

import (
	"fmt"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func (suite *IntegrationTestSuite) TestShardingMismatch() {
	ctx := suite.Context()
	srcInfo, err := util.GetClusterInfo(ctx, suite.srcMongoClient)
	suite.Require().NoError(err, "should fetch src cluster info")

	dstInfo, err := util.GetClusterInfo(ctx, suite.dstMongoClient)
	suite.Require().NoError(err, "should fetch dst cluster info")

	dbname := suite.DBNameForTest()

	shardCollection := func(client *mongo.Client, collName string, key bson.D, label string) {
		suite.Require().NoError(
			client.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"shardCollection", dbname + "." + collName},
					{"key", key},
				},
			).Err(),
			fmt.Sprintf("%s: should shard %#q", label, dbname+"."+collName),
		)
	}

	collections := mslices.Of(
		"idonly",
		"numtype",
		"id_and_foo",
		"sharded_dst",
	)

	for c, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		for _, collName := range collections {
			suite.Require().NoError(
				client.Database(dbname).CreateCollection(
					ctx,
					collName,
				),
				"should create %#q on "+lo.Ternary(c == 0, "source", "destinatinon"),
				collName,
			)
		}
	}

	if srcInfo.Topology == util.TopologySharded {
		suite.Require().NoError(
			suite.srcMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{{"enableSharding", dbname}},
			).Err(),
		)

		shardCollection(
			suite.srcMongoClient,
			"idonly",
			bson.D{{"_id", 1}},
			"src",
		)
		shardCollection(
			suite.srcMongoClient,
			"numtype",
			bson.D{{"foo", 1}},
			"src",
		)
		shardCollection(
			suite.srcMongoClient,
			"id_and_foo",
			bson.D{{"_id", "hashed"}, {"foo", 1}},
			"src",
		)
	}

	if dstInfo.Topology == util.TopologySharded {
		suite.Require().NoError(
			suite.dstMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{{"enableSharding", dbname}},
			).Err(),
		)

		shardCollection(
			suite.dstMongoClient,
			"idonly",
			bson.D{{"_id", 1}},
			"dst",
		)
		shardCollection(
			suite.dstMongoClient,
			"numtype",
			bson.D{{"foo", float64(1)}},
			"dst",
		)
		shardCollection(
			suite.dstMongoClient,
			"id_and_foo",
			bson.D{{"foo", 1}, {"_id", "hashed"}},
			"dst",
		)
		shardCollection(
			suite.dstMongoClient,
			"sharded_dst",
			bson.D{{"foo", 1}, {"_id", "hashed"}},
			"dst",
		)
	}

	verifier := suite.BuildVerifier()

	namespaces := lo.Map(
		collections,
		func(collName string, _ int) string {
			return dbname + "." + collName
		},
	)
	verifier.SetSrcNamespaces(namespaces)
	verifier.SetDstNamespaces(namespaces)
	verifier.SetNamespaceMap()

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	cursor, err := verifier.verificationTaskCollection().Find(
		ctx,
		bson.M{
			"generation": 0,
			"type":       verificationTaskVerifyCollection,
		},
	)
	suite.Require().NoError(err)

	var tasks []VerificationTask
	suite.Require().NoError(cursor.All(ctx, &tasks))

	suite.Require().Len(tasks, len(collections))

	if srcInfo.Topology == util.TopologySharded && dstInfo.Topology == util.TopologySharded {
		taskMap := mslices.ToMap(
			tasks,
			func(task VerificationTask) string {
				return task.QueryFilter.Namespace
			},
		)

		suite.Assert().Equal(
			verificationTaskCompleted,
			taskMap[dbname+".idonly"].Status,
			"full match",
		)

		suite.Assert().Equal(
			verificationTaskCompleted,
			taskMap[dbname+".numtype"].Status,
			"number type differences are ignored",
		)

		suite.Assert().Equal(
			verificationTaskMetadataMismatch,
			taskMap[dbname+".id_and_foo"].Status,
			"catch field order difference",
		)

		suite.Assert().Equal(
			verificationTaskMetadataMismatch,
			taskMap[dbname+".sharded_dst"].Status,
			"catch dst-only sharded",
		)

		suite.T().Logf("tasks: %+v", tasks)
	} else {
		for _, task := range tasks {
			suite.Assert().Equal(
				verificationTaskCompleted,
				task.Status,
				"mismatched topologies, so task should have succeeded: %v", task,
			)
		}
	}
}
