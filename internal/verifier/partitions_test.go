package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (suite *IntegrationTestSuite) TestGetSizeAndDocumentCount() {
	ctx := suite.T().Context()

	db := suite.srcMongoClient.Database(suite.DBNameForTest())

	_, err := db.Collection("stuff").InsertOne(ctx, bson.D{})
	suite.Require().NoError(err, "must insert")

	byteSize, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		logger.NewDebugLogger(),
		db.Collection("stuff"),
	)
	suite.Require().NoError(err, "must get data")

	suite.Assert().Positive(byteSize, "must have nonzero size")
	suite.Assert().EqualValues(1, docsCount, "docs count")
	suite.Assert().False(isCapped, "must say not capped")

	if suite.BuildVerifier().srcClusterInfo.VersionArray[0] >= 5 {
		err = db.CreateCollection(
			ctx,
			"weather",
			options.CreateCollection().
				SetTimeSeriesOptions(
					options.TimeSeries().
						SetTimeField("time"),
				),
		)
		suite.Require().NoError(err, "must create timeseries")

		_, err := db.Collection("weather").InsertOne(
			ctx,
			bson.D{
				{"time", time.Now()},
			},
		)
		suite.Require().NoError(err, "must insert timeseries")

		byteSize, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
			ctx,
			logger.NewDebugLogger(),
			db.Collection("system.buckets.weather"),
		)
		suite.Require().NoError(err, "must get data")

		suite.Assert().Positive(byteSize, "must have nonzero size")
		suite.Assert().EqualValues(1, docsCount, "docs count")
		suite.Assert().False(isCapped, "must say not capped")
	}

}
