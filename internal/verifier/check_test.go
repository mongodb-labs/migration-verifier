package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

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

func (suite *IntegrationTestSuite) TestCheckMissingNamespace() {
	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	runTest := func(
		t *testing.T,
		client *mongo.Client,
		collName string,
	) {
		defer func() {
			err := client.Database(dbName).Collection(collName).Drop(ctx)
			assert.NoError(t, err, "should clean up collection after test")
		}()

		verifier := suite.BuildVerifier()
		verifier.SetMetaDBName(suite.DBNameForTest("verifier-" + collName))
		verifier.SetSrcNamespaces(mslices.Of(dbName + "." + collName))
		verifier.SetDstNamespaces(mslices.Of(dbName + "." + collName))
		verifier.SetNamespaceMap()

		require.NoError(
			t,
			client.Database(dbName).CreateCollection(ctx, collName),
		)

		runner := RunVerifierCheck(ctx, t, verifier)
		err := runner.AwaitGenerationEnd()

		assert.ErrorContains(t, err, collName)
	}

	suite.T().Run("src-only", func(t *testing.T) {
		runTest(t, suite.srcMongoClient, "srcOnlyColl")
	})

	suite.T().Run("dst-only", func(t *testing.T) {
		runTest(t, suite.dstMongoClient, "dstOnlyColl")
	})
}
