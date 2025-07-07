package verifier

func (suite *IntegrationTestSuite) TestPersistedMetadataVersionMismatch() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()
	verifier.SetNamespaceMap()

	genColl := verifier.metaClient.
		Database(verifier.metaDBName).
		Collection(generationCollName)

	_, err := genColl.InsertOne(
		ctx,
		generationDoc{
			MetadataVersion: verifierMetadataVersion - 1,
		},
	)
	suite.Require().NoError(err, "should write old generation doc")

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	err = runner.AwaitGenerationEnd()

	mme := metadataMismatchErr{}
	suite.Require().ErrorAs(err, &mme)
}
