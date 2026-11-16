package verifier

import (
	"context"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/sharding"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"github.com/samber/lo/mutable"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// skipUnlessReplsetSrcAndShardedDst skips the test unless the source is a
// replica set and the destination is a sharded cluster.
func (suite *IntegrationTestSuite) skipUnlessReplsetSrcAndShardedDst() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologyReplset {
		suite.T().Skip("replica-set source required")
	}

	if suite.GetTopology(suite.dstMongoClient) != util.TopologySharded {
		suite.T().Skip("sharded destination required")
	}
}

// shardDstCollAcrossTwoShards shards the given destination collection on
// `sk`, with negative `sk` values on one shard and nonnegative ones on
// another. This lets tests put the same _id on multiple shards.
func (suite *IntegrationTestSuite) shardDstCollAcrossTwoShards(
	ctx context.Context,
	dbName, collName string,
) *mongo.Collection {
	require := require.New(suite.T())

	namespace := dbName + "." + collName
	dstColl := suite.dstMongoClient.Database(dbName).Collection(collName)

	shardIds := getShardIds(suite.T(), suite.dstMongoClient)
	require.Greater(len(shardIds), 1, "need multiple shards; got: %+v", shardIds)

	admin := suite.dstMongoClient.Database("admin")

	require.NoError(admin.RunCommand(ctx, bson.D{{"enableSharding", dbName}}).Err())

	require.NoError(
		admin.RunCommand(
			ctx,
			bson.D{
				{"shardCollection", namespace},
				{"key", bson.D{{"sk", 1}}},
			},
		).Err(),
		"should shard collection",
	)

	require.NoError(sharding.DisableBalancing(ctx, dstColl), "should disable balancing")

	require.NoError(
		admin.RunCommand(ctx, bson.D{
			{"split", namespace},
			{"middle", bson.D{{"sk", 0}}},
		}).Err(),
		"should split",
	)

	for i, findSK := range mslices.Of(-1, 1) {
		require.Eventually(
			func() bool {
				err := admin.RunCommand(ctx, bson.D{
					{"moveChunk", namespace},
					{"find", bson.D{{"sk", findSK}}},
					{"to", shardIds[i]},
					{"_waitForDelete", true},
				}).Err()
				if err != nil {
					suite.T().Logf("Failed to move chunk (sk=%d) to shard %#q: %v", findSK, shardIds[i], err)
					return false
				}

				return true
			},
			5*time.Minute,
			time.Second,
			"should move chunk (sk=%d) to shard %#q",
			findSK,
			shardIds[i],
		)
	}

	return dstColl
}

// assertDuplicateIDReported checks the results for a document whose _id
// appears twice on the destination: one copy with `sk` matching the source,
// and one with a different `sk`.
func (suite *IntegrationTestSuite) assertDuplicateIDReported(idResults []compare.Result) {
	dupeResults := lo.Filter(
		idResults,
		func(r compare.Result, _ int) bool {
			return r.Details == compare.DuplicateID
		},
	)

	if suite.Assert().Len(dupeResults, 1, "should report the duplicate; results: %+v", idResults) {
		dupe := dupeResults[0]
		suite.Assert().Equal(constants.ClusterTarget, dupe.Cluster)
		suite.Assert().True(dupe.DstTimestamp.IsSome(), "should have dst timestamp")
		suite.Assert().True(dupe.SrcTimestamp.IsNone(), "should lack src timestamp")
	}

	// The server doesn’t guarantee which destination copy the comparator
	// pairs with the source document. If it pairs the one with the
	// different shard key value, that yields a content mismatch.
	for _, r := range idResults {
		if r.Details == compare.DuplicateID {
			continue
		}

		suite.Assert().Regexp(
			regexp.MustCompile("^"+Mismatch),
			r.Details,
			"non-duplicate result should be content mismatch: %+v",
			r,
		)
	}

	suite.Assert().LessOrEqual(len(idResults), 2, "at most a duplicate and a content mismatch")
}

// TestVerifier_DuplicateIDOnShardedDestination confirms that, when a
// replica-set source’s document appears on multiple destination shards,
// the verifier reports the extra copy rather than silently discarding it.
func (suite *IntegrationTestSuite) TestVerifier_DuplicateIDOnShardedDestination() {
	suite.skipUnlessReplsetSrcAndShardedDst()

	ctx := suite.T().Context()
	require := require.New(suite.T())

	dbName := suite.DBNameForTest()
	collName := "coll"
	namespace := dbName + "." + collName

	dstColl := suite.shardDstCollAcrossTwoShards(ctx, dbName, collName)

	// The comparator interleaves source & destination documents in _id
	// order. Source docs 1 & 2, which the destination lacks, ensure that
	// both destination copies of doc 3 reach the comparator before the
	// source’s doc 3 does.
	_, err := suite.srcMongoClient.Database(dbName).Collection(collName).InsertMany(
		ctx,
		lo.ToAnySlice(mslices.Of(
			bson.D{{"_id", 1}, {"sk", -1}},
			bson.D{{"_id", 2}, {"sk", -1}},
			bson.D{{"_id", 3}, {"sk", -1}},
		)),
	)
	require.NoError(err, "should insert on source")

	// These land on different shards, so mongos accepts both.
	_, err = dstColl.InsertMany(
		ctx,
		lo.ToAnySlice(mslices.Of(
			bson.D{{"_id", 3}, {"sk", -1}},
			bson.D{{"_id", 3}, {"sk", 1}},
		)),
	)
	require.NoError(err, "should insert duplicate _id on destination")

	task := &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{
			Namespace: namespace,
			To:        namespace,
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: bsontools.ToRawValue(bson.MinKey{}),
				},
				Upper: bsontools.ToRawValue(bson.MaxKey{}),
			},
		},
	}

	verifier := suite.BuildVerifier()
	require.NoError(verifier.startChangeHandling(ctx))

	results, docCount, _, err := runFetchAndCompareDocuments(ctx, verifier, task)
	require.NoError(err, "should fetch & compare")
	suite.Assert().EqualValues(3, docCount, "should count source docs")

	resultsByID := lo.GroupBy(
		results,
		func(r compare.Result) int {
			return lo.Must(bsontools.RawValueTo[int](r.ID))
		},
	)

	for _, id := range mslices.Of(1, 2) {
		if suite.Assert().Len(resultsByID[id], 1, "doc %d should have 1 result", id) {
			suite.Assert().True(resultsByID[id][0].DocumentIsMissing(), "doc %d should be missing", id)
			suite.Assert().Equal(constants.ClusterTarget, resultsByID[id][0].Cluster)
		}
	}

	suite.assertDuplicateIDReported(resultsByID[3])

	suite.Assert().Len(
		results,
		2+len(resultsByID[3]),
		"should have no results for unexpected doc IDs: %+v",
		results,
	)
}

// TestNaturalPartitionDuplicateIDOnShardedDestination confirms that a
// natural-partition comparison handles a destination query that returns
// more documents than the source sent, as happens when an _id appears on
// multiple destination shards.
func (suite *IntegrationTestSuite) TestNaturalPartitionDuplicateIDOnShardedDestination() {
	suite.skipUnlessReplsetSrcAndShardedDst()

	ctx := suite.T().Context()
	require := require.New(suite.T())

	dbName := suite.DBNameForTest()
	collName := "coll"

	dstColl := suite.shardDstCollAcrossTwoShards(ctx, dbName, collName)

	// The source’s natural-order cursor returns these in a single batch,
	// so the destination runs a single query for all of them. That query
	// returns one document more than ToComparatorBatchSize, so the
	// destination sends the comparator more messages than the source does.
	const docsCount = compare.ToComparatorBatchSize

	// Insert in descending _id order so that the source’s natural order is
	// the reverse of the destination’s _id order. That way both destination
	// copies of doc 0 reach the comparator before the source’s doc 0 does.
	srcDocs := lo.RepeatBy(
		docsCount,
		func(i int) bson.D {
			return bson.D{{"_id", i}, {"sk", -1}}
		},
	)
	mutable.Reverse(srcDocs)

	srcColl := suite.srcMongoClient.Database(dbName).Collection(collName)
	_, err := srcColl.InsertMany(ctx, lo.ToAnySlice(srcDocs))
	require.NoError(err, "should insert on source")

	// The extra doc 0 lands on a different shard from the rest, so mongos
	// accepts it.
	dstDocs := append(
		slices.Clone(srcDocs),
		bson.D{{"_id", 0}, {"sk", 1}},
	)
	_, err = dstColl.InsertMany(ctx, lo.ToAnySlice(dstDocs))
	require.NoError(err, "should insert on destination")

	verifier := suite.BuildVerifier()
	verifier.SetPartitioningScheme(partitions.SchemeNatural)

	require.NoError(verifier.startChangeHandling(ctx))

	_, hostname := getDirectClientAndHostname(
		ctx,
		suite.T(),
		suite.srcMongoClient,
		suite.srcConnStr,
	)

	task := &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyCollection,
		QueryFilter: tasks.QueryFilter{
			Namespace: FullName(srcColl),
			To:        FullName(srcColl),
			Partition: &partitions.Partition{
				Natural:         true,
				HostnameAndPort: option.Some(hostname),
				Upper:           bsontools.ToRawValue(math.MaxInt64),
			},
		},
	}

	results, docCount, _, err := runFetchAndCompareDocuments(ctx, verifier, task)
	require.NoError(err, "should fetch & compare")
	suite.Assert().EqualValues(docsCount, docCount, "should count source docs")

	resultsByID := lo.GroupBy(
		results,
		func(r compare.Result) int {
			return lo.Must(bsontools.RawValueTo[int](r.ID))
		},
	)

	suite.assertDuplicateIDReported(resultsByID[0])

	suite.Assert().Len(
		results,
		len(resultsByID[0]),
		"only doc 0 should have results: %+v",
		results,
	)
}
