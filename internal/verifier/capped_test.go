package verifier

import (
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestCappedCollection_Partitioning verifies a capped collection that is much
// larger than the target partition size. Like any other collection, it
// should get multiple partitions, each of which compares only its own
// documents.
func (suite *IntegrationTestSuite) TestCappedCollection_Partitioning() {
	ctx := suite.T().Context()

	dbName := suite.DBNameForTest()
	collName := "capped"
	ns := dbName + "." + collName

	docs := lo.Times(
		1_000,
		func(i int) any {
			return bson.D{{"_id", i}, {"payload", testutil.RandomString(100)}}
		},
	)

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		db := client.Database(dbName)

		suite.Require().NoError(
			db.CreateCollection(
				ctx,
				collName,
				options.CreateCollection().
					SetCapped(true).
					SetSizeInBytes(10*1024*1024),
			),
		)

		_, err := db.Collection(collName).InsertMany(ctx, docs)
		suite.Require().NoError(err)
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces(mslices.Of(ns))
	verifier.SetDstNamespaces(mslices.Of(ns))
	verifier.SetNamespaceMap()

	// The collection is over 100 KB, so this should yield dozens of
	// partitions.
	verifier.partitionSizeInBytes = 2_000

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	cursor, err := verifier.verificationTaskCollection().Find(
		ctx,
		bson.D{
			{"generation", 0},
			{"type", tasks.VerifyDocuments},
			{"query_filter.namespace", ns},
		},
	)
	suite.Require().NoError(err)

	var docTasks []tasks.Task
	suite.Require().NoError(cursor.All(ctx, &docTasks))

	suite.Assert().Greater(len(docTasks), 1, "capped collection should get multiple partitions")

	for _, task := range docTasks {
		suite.Assert().Equal(tasks.Completed, task.Status, "partition %v should match", task.PrimaryKey)
	}

	stats, err := verifier.GetPersistedNamespaceStatistics(ctx, 0)
	suite.Require().NoError(err)

	// Adjacent partitions share a boundary value, so each boundary document
	// is compared twice. Every other document is compared once.
	if suite.Assert().Len(stats, 1) {
		suite.Assert().GreaterOrEqual(
			stats[0].DocsCompared,
			types.DocumentCount(len(docs)),
			"every document should be compared",
		)

		suite.Assert().LessOrEqual(
			stats[0].DocsCompared,
			types.DocumentCount(len(docs)+len(docTasks)-1),
			"only boundary documents should be compared twice",
		)
	}
}
