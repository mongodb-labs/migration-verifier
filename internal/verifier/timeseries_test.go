package verifier

import (
	"testing"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/timeseries"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestTimeSeries_BucketsOnly confirms the verifier’s time-series coverage
// when only the buckets exist. This is important when verifying shard-to-shard.
func (suite *IntegrationTestSuite) TestTimeSeries_BucketsOnly() {
	ctx := suite.Context()

	if suite.BuildVerifier().srcClusterInfo.VersionArray[0] < 6 {
		suite.T().Skipf("Need a source version with time-series support.")
	}

	dbName := suite.DBNameForTest()
	db := suite.srcMongoClient.Database(dbName)
	collName := "weather"
	bucketsCollName := timeseries.BucketPrefix + collName

	suite.Require().NoError(
		db.CreateCollection(
			ctx,
			collName,
			options.CreateCollection().SetTimeSeriesOptions(
				options.TimeSeries().
					SetTimeField("time").
					SetMetaField("sensor"),
			),
		),
	)

	coll := db.Collection(collName)
	_, err := coll.InsertOne(ctx, bson.D{
		{"time", time.Now()},
		{"sensor", 1},
	})
	suite.Require().NoError(err, "should insert first measurement")

	_, err = coll.InsertOne(ctx, bson.D{
		{"time", time.Now()},
		{"sensor", 2},
	})
	suite.Require().NoError(err, "should insert second measurement")

	cursor, err := coll.Database().Collection(bucketsCollName).
		Find(ctx, bson.D{})
	suite.Require().NoError(err, "should count buckets")

	var buckets []bson.D
	suite.Require().NoError(cursor.All(ctx, &buckets))
	suite.Require().Greater(len(buckets), 1, "we need >=1 bucket")

	suite.Require().NoError(coll.Drop(ctx))

	runVerifier := func() (*Verifier, *CheckRunner, *VerificationStatus) {
		verifier := suite.BuildVerifier()
		verifier.SetVerifyAll(true)
		runner := RunVerifierCheck(ctx, suite.T(), verifier)
		suite.Require().NoError(runner.AwaitGenerationEnd())

		verificationStatus, err := verifier.GetVerificationStatus(ctx)
		suite.Require().NoError(err)

		return verifier, runner, verificationStatus
	}

	dstDB := suite.dstMongoClient.Database(dbName)

	suite.Run(
		"generation 0",
		func() {
			// The server forbids creation of a buckets collection without the
			// relevant time-series options.
			suite.Require().NoError(
				db.CreateCollection(
					ctx,
					bucketsCollName,
					options.CreateCollection().SetTimeSeriesOptions(
						options.TimeSeries().
							SetTimeField("time").
							SetMetaField("sensor"),
					),
				),
				"should create source buckets collection",
			)

			_, err := db.Collection(bucketsCollName).InsertMany(
				ctx,
				lo.ToAnySlice(buckets),
			)
			suite.Require().NoError(err, "should insert source buckets")

			verifier, runner, verificationStatus := runVerifier()
			suite.Require().NoError(verifier.WritesOff(ctx))
			suite.Require().NoError(runner.Await())
			suite.Require().NoError(verifier.verificationDatabase().Drop(ctx))

			suite.Assert().NotZero(
				verificationStatus.FailedTasks,
				"missing buckets collection should show mismatch (status: %+v)",
				verificationStatus,
			)

			suite.Require().NoError(
				dstDB.CreateCollection(
					ctx,
					bucketsCollName,
					options.CreateCollection().SetTimeSeriesOptions(
						options.TimeSeries().
							SetTimeField("time").
							SetMetaField("sensor"),
					),
				),
				"should create destination buckets collection",
			)

			_, err = dstDB.
				Collection(bucketsCollName).
				InsertOne(ctx, buckets[0])
			suite.Require().NoError(err)

			verifier, runner, verificationStatus = runVerifier()
			suite.Require().NoError(verifier.WritesOff(ctx))
			suite.Require().NoError(runner.Await())
			suite.Require().NoError(verifier.verificationDatabase().Drop(ctx))

			suite.Assert().NotZero(
				verificationStatus.FailedTasks,
				"1 bucket missing should show mismatch",
			)

			_, err = suite.dstMongoClient.
				Database(dbName).
				Collection(bucketsCollName).
				InsertOne(ctx, buckets[1])
			suite.Require().NoError(err)

			verifier, runner, verificationStatus = runVerifier()
			suite.Require().NoError(verifier.WritesOff(ctx))
			suite.Require().NoError(runner.Await())
			suite.Require().NoError(verifier.verificationDatabase().Drop(ctx))

			suite.Assert().Zero(
				verificationStatus.FailedTasks,
				"if both buckets exist there should be no mismatch",
			)
		},
	)

	suite.Require().NoError(db.Collection(bucketsCollName).Drop(ctx))
	suite.Require().NoError(dstDB.Collection(bucketsCollName).Drop(ctx))

	suite.Run(
		"missing bucket gets added during recheck",
		func() {
			suite.Require().NoError(
				db.CreateCollection(
					ctx,
					bucketsCollName,
					options.CreateCollection().SetTimeSeriesOptions(
						options.TimeSeries().
							SetTimeField("time").
							SetMetaField("sensor"),
					),
				),
				"should create source buckets collection",
			)

			_, err := db.Collection(bucketsCollName).InsertMany(
				ctx,
				lo.ToAnySlice(buckets),
			)
			suite.Require().NoError(err)

			suite.Require().NoError(
				dstDB.CreateCollection(
					ctx,
					bucketsCollName,
					options.CreateCollection().SetTimeSeriesOptions(
						options.TimeSeries().
							SetTimeField("time").
							SetMetaField("sensor"),
					),
				),
				"should create destination buckets collection",
			)

			_, err = suite.dstMongoClient.
				Database(dbName).
				Collection(bucketsCollName).
				InsertOne(ctx, buckets[0])
			suite.Require().NoError(err)

			verifier, runner, verificationStatus := runVerifier()
			defer func() {
				suite.Require().NoError(verifier.WritesOff(ctx))
				suite.Assert().NoError(runner.Await())
				suite.Require().NoError(verifier.verificationDatabase().Drop(ctx))
			}()

			suite.Assert().NotZero(
				verificationStatus.FailedTasks,
				"1 bucket missing should show mismatch",
			)

			_, err = suite.dstMongoClient.
				Database(dbName).
				Collection(bucketsCollName).
				InsertOne(ctx, buckets[1])
			suite.Require().NoError(err)

			suite.Assert().Eventually(
				func() bool {
					suite.Require().NoError(runner.StartNextGeneration())
					suite.Require().NoError(runner.AwaitGenerationEnd())

					verificationStatus, err := verifier.GetVerificationStatus(ctx)
					suite.Require().NoError(err)

					return verificationStatus.FailedTasks == 0
				},
				time.Minute,
				time.Second,
				"eventually verifier should see that buckets now match",
			)
		},
	)
}

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
				collName,
				options.CreateCollection().SetTimeSeriesOptions(
					options.TimeSeries().
						SetTimeField("time").
						SetMetaField("metadata"),
				),
			),
		)

		// v7+ automatically creates this:
		_, err := client.Database(dbName).Collection("weather").Indexes().
			CreateOne(
				ctx,
				mongo.IndexModel{
					Keys: bson.D{
						{"metadata", 1},
						{"time", 1},
					},
				},
			)
		suite.Require().NoError(err, "should create index")
	}

	srcDB := suite.srcMongoClient.Database(dbName)
	_, err := srcDB.Collection(collName).InsertOne(ctx, bson.D{
		{"time", now},
		{"metadata", 234.0},
	})
	suite.Require().NoError(err, "should insert measurement")

	copyDocs(
		suite.T(),
		srcDB.Collection(timeseries.BucketPrefix+collName),
		suite.dstMongoClient.Database(dbName).Collection(timeseries.BucketPrefix+collName),
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
		"should be no failed tasks (status: %+v)",
		verificationStatus,
	)
	suite.Assert().Equal(
		verificationStatus.TotalTasks,
		verificationStatus.CompletedTasks,
		"should be completed: view meta, buckets meta, and buckets docs (tasks: %+v)",
		verificationStatus,
	)

	suite.T().Logf("verificationStatus")

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
