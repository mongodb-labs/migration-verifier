package verifier

// TestFailChangeReaderOptChange confirms that verifier fails if it restarts
// with different change-reader settings.
func (suite *IntegrationTestSuite) TestFailChangeReaderOptChange() {
	ctx := suite.Context()

	v1 := suite.BuildVerifier()
	suite.Require().NoError(
		v1.SetSrcChangeReaderMethod(ChangeReaderOptChangeStream),
	)
	suite.Require().NoError(
		v1.SetDstChangeReaderMethod(ChangeReaderOptChangeStream),
	)

	v1Runner := RunVerifierCheck(ctx, suite.T(), v1)
	suite.Require().NoError(
		v1Runner.AwaitGenerationEnd(),
	)

	badSrcOptVerifier := suite.BuildVerifier()
	suite.Require().NoError(
		badSrcOptVerifier.SetSrcChangeReaderMethod(ChangeReaderOptOplog),
	)
	suite.Require().NoError(
		badSrcOptVerifier.SetDstChangeReaderMethod(ChangeReaderOptChangeStream),
	)

	err := RunVerifierCheck(ctx, suite.T(), badSrcOptVerifier).
		AwaitGenerationEnd()

	suite.Require().Error(err, "wrong source opt should fail")
	suite.Assert().ErrorAs(err, &changeReaderOptMismatchErr{})

	badDstOptVerifier := suite.BuildVerifier()
	suite.Require().NoError(
		badDstOptVerifier.SetSrcChangeReaderMethod(ChangeReaderOptChangeStream),
	)
	suite.Require().NoError(
		badDstOptVerifier.SetDstChangeReaderMethod(ChangeReaderOptOplog),
	)

	err = RunVerifierCheck(ctx, suite.T(), badDstOptVerifier).
		AwaitGenerationEnd()

	suite.Require().Error(err, "wrong destination opt should fail")
	suite.Assert().ErrorAs(err, &changeReaderOptMismatchErr{})
}
