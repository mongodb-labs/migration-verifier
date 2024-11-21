package verifier

import "context"

func (suite *IntegrationTestSuite) TestGetClusterTime() {
	ctx := context.Background()
	logger, _ := getLoggerAndWriter("stdout")

	ts, err := GetClusterTime(ctx, logger, suite.srcMongoClient)
	suite.Require().NoError(err)

	suite.Assert().NotZero(ts, "timestamp should be nonzero")
}
