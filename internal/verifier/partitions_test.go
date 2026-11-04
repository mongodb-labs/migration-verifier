package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/timeseries"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (suite *IntegrationTestSuite) TestGetSizeAndDocumentCount() {
	ctx := suite.T().Context()

	db := suite.srcMongoClient.Database(suite.DBNameForTest())

	_, err := db.Collection("stuff").InsertOne(ctx, bson.D{})
	suite.Require().NoError(err, "must insert")

	srcInfo := suite.BuildVerifier().srcClusterInfo
	hasRawData := util.ClusterHasRawData([2]int(srcInfo.VersionArray))

	opts := partitions.GetSizeAndDocumentCountOptions{
		RawData: hasRawData,
	}

	byteSize, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		logger.NewDebugLogger(),
		db.Collection("stuff"),
		opts,
	)
	suite.Require().NoError(err, "must get data")

	suite.Assert().Positive(byteSize, "must have nonzero size")
	suite.Assert().EqualValues(1, docsCount, "docs count")
	suite.Assert().False(isCapped, "must say not capped")

	collName := "weather"

	if srcInfo.VersionArray[0] >= 5 {
		err = db.CreateCollection(
			ctx,
			collName,
			options.CreateCollection().
				SetTimeSeriesOptions(
					options.TimeSeries().
						SetTimeField("time"),
				),
		)
		suite.Require().NoError(err, "must create timeseries")

		_, err := db.Collection(collName).InsertOne(
			ctx,
			bson.D{
				{"time", time.Now()},
			},
		)
		suite.Require().NoError(err, "must insert timeseries")

		bucketColl := lo.Ternary(
			util.ClusterHasRawData([2]int(srcInfo.VersionArray)),
			collName,
			timeseries.BucketPrefix+collName,
		)

		byteSize, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
			ctx,
			logger.NewDebugLogger(),
			db.Collection(bucketColl),
			opts,
		)
		suite.Require().NoError(err, "must get data")

		suite.Assert().Positive(byteSize, "must have nonzero size")
		suite.Assert().EqualValues(1, docsCount, "docs count")
		suite.Assert().False(isCapped, "must say not capped")

		if !hasRawData {
			// Ensure that the view triggers an error:
			_, _, _, err = partitions.GetSizeAndDocumentCount(
				ctx,
				logger.NewDebugLogger(),
				db.Collection(bucketColl),
				opts,
			)
			suite.Require().Error(err, "expect failure on timeseries view")
			suite.Assert().ErrorContains(err, timeseries.BucketPrefix)
		}
	}

}
