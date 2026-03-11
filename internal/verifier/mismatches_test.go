package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMismatchesInfoMarshal(t *testing.T) {
	results := []compare.Result{
		{},
		{
			ID: mbson.ToRawValue("hello"),
		},
		{
			SrcTimestamp: option.Some(bson.Timestamp{12, 23}),
		},
		{
			DstTimestamp: option.Some(bson.Timestamp{12, 23}),
		},
		{
			ID:           mbson.ToRawValue("hello"),
			Field:        "yeah",
			Cluster:      "hoho",
			Details:      "aaa",
			NameSpace:    "xxxxx",
			SrcTimestamp: option.Some(bson.Timestamp{12, 23}),
			DstTimestamp: option.Some(bson.Timestamp{23, 24}),
		},
	}

	for _, result := range results {
		mi := MismatchInfo{
			TaskID: bson.NewObjectID(),
			Detail: result,
		}

		raw := mi.MarshalToBSON()

		var rt MismatchInfo
		require.NoError(t, bson.Unmarshal(raw, &rt))

		assert.Equal(t, mi, rt, "should round-trip")
	}
}

func (suite *IntegrationTestSuite) TestSendDocumentMismatches() {
	ctx := suite.Context()

	srcColl := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("stuff")

	dstColl := suite.dstMongoClient.
		Database(suite.DBNameForTest()).
		Collection("stuff")

	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", "onSource"}})
	suite.Require().NoError(err)

	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", "onDestination"}})
	suite.Require().NoError(err)

	_, err = srcColl.InsertOne(ctx, bson.D{
		{"_id", "onBoth"},
		{"foo", "abc"},
	})
	suite.Require().NoError(err)

	_, err = dstColl.InsertOne(ctx, bson.D{
		{"_id", "onBoth"},
		{"foo", "cba"},
	})
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	mmChan := make(chan api.MismatchInfo, 10)
	suite.Require().NoError(verifier.SendDocumentMismatches(ctx, 0, mmChan))

	mismatches := lo.ChannelToSlice(mmChan)

	suite.Assert().ElementsMatch(
		[]api.MismatchInfo{
			{
				Namespace: FullName(srcColl),
				ID:        bsontools.ToRawValue("onSource"),
				Type:      api.MismatchMissing,
			},
			{
				Namespace: FullName(srcColl),
				ID:        bsontools.ToRawValue("onDestination"),
				Type:      api.MismatchExtra,
			},
			{
				Namespace: FullName(srcColl),
				ID:        bsontools.ToRawValue("onBoth"),
				Type:      api.MismatchContent,
				Field: lo.Ternary(
					verifier.docCompareMethod.ComparesFullDocuments(),
					option.Some("foo"),
					option.None[string](),
				),
				Detail: option.Some(Mismatch),
			},
		},
		mismatches,
	)
}
