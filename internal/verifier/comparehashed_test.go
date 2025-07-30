package verifier

import (
	"math"

	"github.com/10gen/migration-verifier/internal/comparehashed"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestCompare_Hashed ensures that $toHashedIndexKey-based
// verification detects certain mismatches.
func (suite *IntegrationTestSuite) TestCompare_Hashed() {
	ctx := suite.Context()

	decimal128_42, err := primitive.ParseDecimal128("42")
	suite.Require().NoError(err, "should parse `42` as decimal128")

	cases := []struct {
		label  string
		srcVal any
		dstVal any
	}{
		// Plain $toHashedIndexKey treats these the same; the code uses
		// $_internalKeyStringValue to disambiguate them.
		{"high float", math.Pow(2, 70), math.Pow(2, 71)},

		// Again, plain $toHashedIndexKey yields the same hash from all
		// of these values. These we can disambiguate by checking the
		// document length. (That, of course, won’t catch long-vs.-double
		// since those are same-length types.)
		{"int to long", int32(42), int64(42)},
		{"int to double", int32(42), float64(42)},
		{"int to decimal128", int32(42), decimal128_42},
		{"long to decimal128", int64(42), decimal128_42},
		{"double to decimal128", float64(42), decimal128_42},
	}

	for _, curCase := range cases {
		suite.Run(
			curCase.label,
			func() {
				srcColl := suite.srcMongoClient.Database(suite.DBNameForTest()).Collection("coll")
				dstColl := suite.dstMongoClient.Database(suite.DBNameForTest()).Collection("coll")

				_, err := srcColl.ReplaceOne(
					ctx,
					bson.D{},
					bson.D{
						{"_id", 0},
						{"n", curCase.srcVal},
					},
					options.Replace().SetUpsert(true),
				)
				suite.Require().NoError(err, "should create source docs (n=%v)", curCase.srcVal)

				_, err = dstColl.ReplaceOne(
					ctx,
					bson.D{},
					bson.D{
						{"_id", 0},
						{"n", curCase.dstVal},
					},
					options.Replace().SetUpsert(true),
				)
				suite.Require().NoError(err, "should create dest docs")

				verifier := suite.BuildVerifier()
				suite.Require().NoError(
					verifier.verificationDatabase().Drop(ctx),
					"should drop verification metadata database",
				)

				// We only check the source since the destination should be more recent.
				if !comparehashed.CanCompareDocsViaToHashedIndexKey(verifier.srcClusterInfo.VersionArray) {
					suite.T().Skipf("source (%v) can’t do hashed comparison", verifier.srcClusterInfo.VersionArray)
				}
				ns := srcColl.Database().Name() + "." + srcColl.Name()
				verifier.SetSrcNamespaces([]string{ns})
				verifier.SetDstNamespaces([]string{ns})
				verifier.SetNamespaceMap()
				verifier.SetDocCompareMethod(DocCompareToHashedIndexKey)

				runner := RunVerifierCheck(ctx, suite.T(), verifier)
				suite.Require().NoError(runner.AwaitGenerationEnd())

				status, err := verifier.GetVerificationStatus(ctx)
				suite.Require().NoError(err)
				suite.Assert().NotZero(status.FailedTasks, "mismatch should show")
			},
		)
	}
}
