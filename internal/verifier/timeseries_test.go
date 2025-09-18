package verifier

import (
	"testing"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (suite *IntegrationTestSuite) TestTimeSeries_Simple() {
	ctx := suite.Context()

	if suite.BuildVerifier().srcClusterInfo.VersionArray[0] < 6 {
		suite.T().Skipf("Need a source version with time-series support.")
	}

	dbName := suite.DBNameForTest()
	collName := "weather"
	now := time.Now()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		suite.Require().NoError(
			client.Database(dbName).CreateCollection(
				ctx,
				"weather",
				options.CreateCollection().SetTimeSeriesOptions(
					options.TimeSeries().
						SetTimeField("time").
						SetMetaField("metadata"),
				),
			),
		)
	}

	srcDB := suite.srcMongoClient.Database(dbName)
	_, err := srcDB.Collection(collName).InsertOne(ctx, bson.D{
		{"time", now},
		{"metadata", 234.0},
	})
	suite.Require().NoError(err, "should insert measurement")

	copyDocs(
		suite.T(),
		srcDB.Collection("system.buckets."+collName),
		suite.dstMongoClient.Database(dbName).Collection("system.buckets."+collName),
	)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	verificationStatus, err := verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Assert().Equal(
		0,
		verificationStatus.FailedTasks,
		"should be no failed tasks",
	)
	suite.Assert().Equal(
		3,
		verificationStatus.CompletedTasks,
		"should be completed: view meta, buckets meta, and buckets docs",
	)

	_, err = srcDB.Collection("weather").InsertOne(ctx, bson.D{
		{"time", now},
		{"metadata", 234.0},
	})
	suite.Require().NoError(err, "should insert measurement (dupe)")

	suite.Assert().Eventually(
		func() bool {
			suite.Require().NoError(runner.StartNextGeneration())
			suite.Require().NoError(runner.AwaitGenerationEnd())

			verificationStatus, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			return verificationStatus.FailedTasks > 0
		},
		time.Minute,
		50*time.Millisecond,
		"uncopied update on source should trigger failure",
	)
}

func copyDocs(
	t *testing.T,
	srcColl, dstColl *mongo.Collection,
) {
	ctx := t.Context()

	cursor, err := srcColl.Find(ctx, bson.D{})
	require.NoError(t, err, "should open src cursor")

	inserted := 0
	for cursor.Next(ctx) {
		_, err := dstColl.InsertOne(ctx, cursor.Current)
		require.NoError(t, err, "should insert on dst")

		inserted++
	}

	require.NoError(t, cursor.Err(), "should iterate src cursor")

	t.Logf("Copied %d docs %#q -> %#q", inserted, FullName(srcColl), FullName(dstColl))
}
