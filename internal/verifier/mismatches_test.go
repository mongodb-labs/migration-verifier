package verifier

import (
	"fmt"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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

func (suite *IntegrationTestSuite) TestSendNamespaceMismatches_Ignore() {
	ctx := suite.Context()

	srcDB := suite.srcMongoClient.Database(suite.DBNameForTest())
	dstDB := suite.dstMongoClient.Database(suite.DBNameForTest())

	_, err := srcDB.Collection("mismatchedIndex").Indexes().CreateMany(
		ctx,
		[]mongo.IndexModel{
			{
				Keys:    bson.D{{"field", 1}},
				Options: options.Index().SetExpireAfterSeconds(123),
			},
			{
				Keys:    bson.D{{"foo", 1}},
				Options: options.Index().SetUnique(true),
			},
		},
	)
	suite.Require().NoError(err)

	_, err = dstDB.Collection("mismatchedIndex").Indexes().CreateMany(
		ctx,
		[]mongo.IndexModel{
			{
				Keys:    bson.D{{"field", 1}},
				Options: options.Index().SetExpireAfterSeconds(12123),
			},
			{
				Keys: bson.D{{"foo", 1}},
			},
		},
	)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	mmChan := make(chan api.MismatchInfo, 100)
	suite.Require().NoError(verifier.SendNamespaceMismatches(
		ctx,
		mslices.Of(api.IndexSpecIgnoreTTL, api.IndexSpecIgnoreUnique),
		mmChan,
	))

	mismatches := lo.ChannelToSlice(mmChan)

	suite.Assert().Empty(mismatches)
}

func (suite *IntegrationTestSuite) TestSendNamespaceMismatches() {
	ctx := suite.Context()

	srcDB := suite.srcMongoClient.Database(suite.DBNameForTest())
	dstDB := suite.dstMongoClient.Database(suite.DBNameForTest())

	suite.Require().NoError(
		dstDB.CreateCollection(ctx, "extraOnDst"),
	)
	suite.Require().NoError(
		srcDB.CreateCollection(ctx, "missingOnDst"),
	)

	suite.Require().NoError(
		dstDB.CreateCollection(ctx, "mismatchedType"),
	)
	suite.Require().NoError(
		srcDB.CreateView(ctx, "mismatchedType", "targetColl", mongo.Pipeline{}),
	)

	suite.Require().NoError(
		srcDB.CreateCollection(
			ctx,
			"mismatchedCollation",
			options.CreateCollection().SetCollation(
				&options.Collation{
					Locale:   "en",
					Strength: 1,
				},
			),
		),
	)
	suite.Require().NoError(
		dstDB.CreateCollection(
			ctx,
			"mismatchedCollation",
			options.CreateCollection().SetCollation(
				&options.Collation{
					Locale:   "en",
					Strength: 2,
				},
			),
		),
	)

	_, err := srcDB.Collection("mismatchedIndex").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.D{{"field", 1}},
			Options: options.Index().SetName("missing_1"),
		},
	)
	suite.Require().NoError(err)

	_, err = dstDB.Collection("mismatchedIndex").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.D{{"someField", 1}},
			Options: options.Index().SetName("extra_1"),
		},
	)
	suite.Require().NoError(err)

	_, err = srcDB.Collection("mismatchedIndex").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.D{{"foo", 1}},
			Options: options.Index().SetName("foo_1"),
		},
	)
	suite.Require().NoError(err)

	_, err = dstDB.Collection("mismatchedIndex").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    bson.D{{"foo", -1}},
			Options: options.Index().SetName("foo_1"),
		},
	)
	suite.Require().NoError(err)

	srcIsSharded := suite.GetTopology(suite.srcMongoClient) == util.TopologySharded
	dstIsSharded := suite.GetTopology(suite.dstMongoClient) == util.TopologySharded

	testShardKey := srcIsSharded && dstIsSharded

	if testShardKey {
		suite.Require().Equal(
			suite.GetTopology(suite.dstMongoClient),
			util.TopologySharded,
		)

		suite.Require().NoError(
			suite.srcMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"enableSharding", srcDB.Name()},
				},
			).Err(),
		)
		suite.Require().NoError(
			suite.dstMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"enableSharding", dstDB.Name()},
				},
			).Err(),
		)

		suite.Require().NoError(
			suite.srcMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"shardCollection", srcDB.Name() + ".mismatchedShardKey"},
					{"key", bson.D{{"_id", "hashed"}}},
				},
			).Err(),
		)
		suite.Require().NoError(
			suite.dstMongoClient.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"shardCollection", dstDB.Name() + ".mismatchedShardKey"},
					{"key", bson.D{{"_id", 1}}},
				},
			).Err(),
		)
	}

	// Now that the namespaces are created, let’s see some mismatches …

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	mmChan := make(chan api.MismatchInfo, 100)
	suite.Require().NoError(verifier.SendNamespaceMismatches(ctx, nil, mmChan))

	mismatches := lo.ChannelToSlice(mmChan)

	expected := []api.MismatchInfo{
		// missing/extra collections:
		{
			Type:      api.MismatchMissing,
			Namespace: srcDB.Name() + ".missingOnDst",
		},
		{
			Type:      api.MismatchExtra,
			Namespace: srcDB.Name() + ".extraOnDst",
		},

		// mismatched namespace type:
		{
			Type:      api.MismatchContent,
			Namespace: srcDB.Name() + ".mismatchedType",
			Field:     option.Some("type"),
			Detail:    option.Some(Mismatch + ": src: view, dst: collection"),
		},

		// mismatched collation:
		{
			Type:      api.MismatchContent,
			Namespace: srcDB.Name() + ".mismatchedCollation",
			ID:        bsontools.ToRawValue("spec"),
			Field:     option.Some("options.collation"),
			Detail:    option.Some(Mismatch),
		},
		{
			Type:      api.MismatchContent,
			Namespace: srcDB.Name() + ".mismatchedCollation",
			ID:        bsontools.ToRawValue("_id"),
			Field:     option.Some("index"),
			Detail: option.Some(fmt.Sprintf(
				"%s: {%s}",
				Mismatch,
				strings.Join(
					mslices.Of(
						`"op":"replace"`,
						`"path":"/collation/strength/$numberInt"`,
						`"value":"2"`,
					),
					",",
				),
			)),
		},
		{
			Type:      api.MismatchContent,
			Namespace: srcDB.Name() + ".mismatchedCollation",
			ID:        bsontools.ToRawValue("_id_"),
			Field:     option.Some("index"),
			Detail: option.Some(fmt.Sprintf(
				"%s: {%s}",
				Mismatch,
				strings.Join(
					mslices.Of(
						`"op":"replace"`,
						`"path":"/collation/strength/$numberInt"`,
						`"value":"2"`,
					),
					",",
				),
			)),
		},

		// missing, extra, & mismatched indexes:
		{
			Type:      api.MismatchMissing,
			Namespace: srcDB.Name() + ".mismatchedIndex",
			ID:        bsontools.ToRawValue("missing_1"),
			Field:     option.Some("index"),
		},
		{
			Type:      api.MismatchExtra,
			Namespace: srcDB.Name() + ".mismatchedIndex",
			ID:        bsontools.ToRawValue("extra_1"),
			Field:     option.Some("index"),
		},
		{
			Type:      api.MismatchContent,
			Namespace: srcDB.Name() + ".mismatchedIndex",
			ID:        bsontools.ToRawValue("foo_1"),
			Field:     option.Some("index"),
			Detail: option.Some(fmt.Sprintf(
				"%s: {%s}",
				Mismatch,
				strings.Join(
					mslices.Of(
						`"op":"replace"`,
						`"path":"/key/foo/$numberInt"`,
						`"value":"-1"`,
					),
					",",
				),
			)),
		},
	}

	if testShardKey {
		expected = append(expected, []api.MismatchInfo{
			{
				Type:      api.MismatchContent,
				Namespace: srcDB.Name() + ".mismatchedShardKey",
				Field:     option.Some(ShardKeyField),
				Detail: option.Some(fmt.Sprintf(
					"%s: src={%s}; dst={%s}",
					Mismatch,
					`"_id": "hashed"`,
					`"_id": {"$numberInt":"1"}`,
				)),
			},
			{
				Type:      api.MismatchMissing,
				Namespace: srcDB.Name() + ".mismatchedShardKey",
				ID:        bsontools.ToRawValue("_id_hashed"),
				Field:     option.Some("index"),
			},
		}...)
	}

	suite.Assert().ElementsMatch(expected, mismatches)
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
