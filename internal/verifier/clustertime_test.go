package verifier

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (suite *IntegrationTestSuite) TestGetNewClusterTime() {
	ctx := suite.Context()
	logger, _ := getLoggerAndWriter("stdout")

	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)

	_, err = suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("mycoll").
		InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{})
	suite.Require().NoError(err)

	clusterTimeVal, err := sess.ClusterTime().LookupErr("$clusterTime", "clusterTime")
	suite.Require().NoError(err, "should extract cluster time from %+v", sess.ClusterTime())

	clusterT, clusterI, ok := clusterTimeVal.TimestampOK()
	suite.Require().True(ok, "session cluster time (%s: %v) must be a timestamp", clusterTimeVal.Type, clusterTimeVal)

	ts, err := GetNewClusterTime(ctx, logger, suite.srcMongoClient)
	suite.Require().NoError(err)

	suite.Require().NotZero(ts, "timestamp should be nonzero")
	suite.Assert().True(ts.After(bson.Timestamp{T: clusterT, I: clusterI}))
}
