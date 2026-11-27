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
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

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

	dstColl := suite.shardCollAcrossTwoShards(ctx, suite.dstMongoClient, dbName, collName)

	// The comparator interleaves source & destination documents in _id
	// order. Source docs 1 & 2, which the destination lacks, ensure that
	// both destination copies of doc 3 reach the comparator before the
	// source’s doc 3 does.
	_, err := suite.srcMongoClient.Database(dbName).Collection(collName).InsertMany(
		ctx,
		[]bson.D{
			{{"_id", 1}, {"sk", -1}},
			{{"_id", 2}, {"sk", -1}},
			{{"_id", 3}, {"sk", -1}},
		},
	)
	require.NoError(err, "should insert on source")

	// These land on different shards, so mongos accepts both.
	_, err = dstColl.InsertMany(
		ctx,
		[]bson.D{
			{{"_id", 3}, {"sk", -1}},
			{{"_id", 3}, {"sk", 1}},
		},
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

	dstColl := suite.shardCollAcrossTwoShards(ctx, suite.dstMongoClient, dbName, collName)

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
	slices.Reverse(srcDocs)

	srcColl := suite.srcMongoClient.Database(dbName).Collection(collName)
	_, err := srcColl.InsertMany(ctx, srcDocs)
	require.NoError(err, "should insert on source")

	// The extra doc 0 lands on a different shard from the rest, so mongos
	// accepts it.
	dstDocs := append(
		slices.Clone(srcDocs),
		bson.D{{"_id", 0}, {"sk", 1}},
	)
	_, err = dstColl.InsertMany(ctx, dstDocs)
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

// shardCollAcrossTwoShards shards the given collection on `sk`, with
// negative `sk` values on one shard and nonnegative ones on another. This
// lets tests put the same _id on multiple shards.
func (suite *IntegrationTestSuite) shardCollAcrossTwoShards(
	ctx context.Context,
	client *mongo.Client,
	dbName, collName string,
) *mongo.Collection {
	require := require.New(suite.T())

	namespace := dbName + "." + collName
	coll := client.Database(dbName).Collection(collName)

	shardIds := getShardIds(suite.T(), client)
	require.Greater(len(shardIds), 1, "need multiple shards; got: %+v", shardIds)

	admin := client.Database("admin")

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

	require.NoError(sharding.DisableBalancing(ctx, coll), "should disable balancing")

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

	return coll
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

// TestDuplicateIDOnShardedSource confirms that, when a sharded source holds
// the same _id on multiple shards, the verifier compares each copy with its
// destination counterpart. It checks this both in generation 0 and in
// rechecks.
func (suite *IntegrationTestSuite) TestDuplicateIDOnShardedSource() {
	suite.skipUnlessShardedSrcAndDst()

	ctx := suite.T().Context()

	dbName := suite.DBNameForTest()
	collName := "coll"
	namespace := dbName + "." + collName

	srcColl := suite.shardCollAcrossTwoShards(ctx, suite.srcMongoClient, dbName, collName)
	dstColl := suite.shardCollAcrossTwoShards(ctx, suite.dstMongoClient, dbName, collName)

	// _ids 2 & 4 exist on both shards. The other documents ensure that the
	// duplicates reach the comparator amid other documents.
	docs := []bson.D{
		{{"_id", 1}, {"sk", -1}, {"v", "a"}},
		{{"_id", 2}, {"sk", -1}, {"v", "a"}},
		{{"_id", 2}, {"sk", 1}, {"v", "b"}},
		{{"_id", 3}, {"sk", -1}, {"v", "a"}},
		{{"_id", 4}, {"sk", -1}, {"v", "a"}},
		{{"_id", 4}, {"sk", 1}, {"v", "b"}},
		{{"_id", 5}, {"sk", 1}, {"v", "b"}},
	}

	for i, coll := range mslices.Of(srcColl, dstColl) {
		_, err := coll.InsertMany(ctx, lo.ToAnySlice(docs))
		suite.Require().NoError(err, "should insert into %s", lo.Ternary(i == 0, "source", "destination"))
	}

	// The comparator alternates between source & destination documents.
	// This destination-only document puts the destination one document
	// ahead, so both source copies of a duplicated _id reach the
	// comparator before their destination counterparts. A comparator that
	// distinguishes documents only by _id would then misreport the second
	// source copy as a duplicate.
	//
	// This document stays mismatched for the whole test, so every
	// generation rechecks it. That keeps the destination one document ahead
	// in rechecks, too.
	_, err := dstColl.InsertOne(ctx, bson.D{{"_id", 0}, {"sk", -1}, {"v", "a"}})
	suite.Require().NoError(err)

	// One copy of _id 4 differs.
	_, err = dstColl.UpdateOne(
		ctx,
		bson.D{{"_id", 4}, {"sk", 1}},
		bson.D{{"$set", bson.D{{"v", "changed"}}}},
	)
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces(mslices.Of(namespace))
	verifier.SetDstNamespaces(mslices.Of(namespace))
	verifier.SetNamespaceMap()

	// This ensures that generation 0 actually partitions the collection.
	// (Collections smaller than a partition skip that.)
	verifier.partitionSizeInBytes = 50

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	suite.assertOnlyMismatchIs(suite.withoutDstOnlyDoc(verifier, 0), 4)

	// Fix the destination’s copy of _id 4. Also update both copies of
	// _id 2, identically on both clusters, so that _id 2 gets rechecked.
	_, err = dstColl.UpdateOne(
		ctx,
		bson.D{{"_id", 4}, {"sk", 1}},
		bson.D{{"$set", bson.D{{"v", "b"}}}},
	)
	suite.Require().NoError(err)

	_, err = dstColl.UpdateMany(
		ctx,
		bson.D{{"_id", 2}},
		bson.D{{"$set", bson.D{{"v", "updated"}}}},
	)
	suite.Require().NoError(err)

	srcUpdateStart := suite.getClusterTime(ctx, suite.srcMongoClient)

	_, err = srcColl.UpdateMany(
		ctx,
		bson.D{{"_id", 2}},
		bson.D{{"$set", bson.D{{"v", "updated"}}}},
	)
	suite.Require().NoError(err)

	// Generation 0’s mismatches already enqueued rechecks for _ids 0 & 4.
	// _id 2’s recheck comes from the source’s change event.
	suite.awaitRecheckEnqueued(verifier, namespace, 2, false, srcUpdateStart)

	generation := suite.runNextGeneration(runner, verifier)

	for _, id := range mslices.Of(0, 2, 4) {
		suite.Assert().True(
			suite.generationRechecked(verifier, generation, id),
			"generation %d should recheck _id %d with the shard key",
			generation,
			id,
		)
	}

	suite.Assert().Empty(
		suite.withoutDstOnlyDoc(verifier, generation),
		"generation %d should find no other mismatches",
		generation,
	)

	// Now make one copy of _id 2 differ. A recheck should find just that.
	//
	// The destination’s update touches both of its shards. That matters
	// because a sharded change stream can’t report an event until all
	// shards’ oplogs pass that event’s time. Idle shards only advance
	// periodically, which would delay the change event that we await.
	dstUpdateStart := suite.getClusterTime(ctx, suite.dstMongoClient)

	_, err = dstColl.UpdateMany(
		ctx,
		bson.D{{"_id", 2}},
		bson.D{{"$set", bson.D{{"v", "changed"}}}},
	)
	suite.Require().NoError(err)

	_, err = srcColl.UpdateOne(
		ctx,
		bson.D{{"_id", 2}, {"sk", 1}},
		bson.D{{"$set", bson.D{{"v", "changed"}}}},
	)
	suite.Require().NoError(err)

	suite.awaitRecheckEnqueued(verifier, namespace, 2, true, dstUpdateStart)

	generation = suite.runNextGeneration(runner, verifier)

	suite.assertOnlyMismatchIs(suite.withoutDstOnlyDoc(verifier, generation), 2)
}

// withoutDstOnlyDoc fetches the given generation’s mismatches for
// TestDuplicateIDOnShardedSource. It asserts that they include its
// destination-only document (_id 0), then returns the other mismatches.
func (suite *IntegrationTestSuite) withoutDstOnlyDoc(
	verifier *Verifier,
	generation int,
) []compare.Result {
	mismatches := suite.getMismatchesForGeneration(verifier, generation)

	missing, others := lo.FilterReject(
		mismatches,
		func(r compare.Result, _ int) bool {
			return r.DocumentIsMissing()
		},
	)

	if suite.Assert().Len(missing, 1, "generation %d should find 1 missing document; mismatches: %+v", generation, mismatches) {
		suite.Assert().Equal(0, lo.Must(bsontools.RawValueTo[int](missing[0].ID)), "missing document’s _id")
		suite.Assert().Equal(constants.ClusterSource, missing[0].Cluster, "missing document’s cluster")
	}

	return others
}

// skipUnlessShardedSrcAndDst skips the test unless both source and
// destination are sharded clusters.
func (suite *IntegrationTestSuite) skipUnlessShardedSrcAndDst() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		suite.T().Skip("sharded source required")
	}

	if suite.GetTopology(suite.dstMongoClient) != util.TopologySharded {
		suite.T().Skip("sharded destination required")
	}
}

// runNextGeneration runs the verifier’s next generation and returns that
// generation’s number.
func (suite *IntegrationTestSuite) runNextGeneration(
	runner *CheckRunner,
	verifier *Verifier,
) int {
	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	generation, _ := verifier.getGeneration()

	return generation
}

// awaitRecheckEnqueued waits until the verifier has enqueued, for its next
// generation, a recheck of the given _id that a change event caused after
// the given cluster time.
//
// This requires the verifier to be paused between generations.
func (suite *IntegrationTestSuite) awaitRecheckEnqueued(
	verifier *Verifier,
	namespace string,
	id int,
	fromDst bool,
	after bson.Timestamp,
) {
	ctx := suite.T().Context()

	dbName, collName := mmongo.SplitNamespace(namespace)

	generation, _ := verifier.getGeneration()
	recheckColl := verifier.getRecheckQueueCollection(1 + generation)

	filter := bson.D{
		{"_id.db", dbName},
		{"_id.coll", collName},
		{"_id.docID", id},
		{"changeOpTime", bson.D{{"$gt", after}}},
		{"fromDst", lo.Ternary(
			fromDst,
			bson.D{{"$eq", true}},
			bson.D{{"$ne", true}},
		)},
	}

	suite.Require().Eventually(
		func() bool {
			count, err := recheckColl.CountDocuments(ctx, filter)
			suite.Require().NoError(err)

			return count > 0
		},
		time.Minute,
		10*time.Millisecond,
		"%s change event should enqueue a recheck of _id %d",
		lo.Ternary(fromDst, "destination", "source"),
		id,
	)
}

// generationRechecked indicates whether the given generation rechecked the
// given document _id with a task that stores the shard key (sk) of
// TestDuplicateIDOnShardedSource’s collection.
//
// The shard key check matters because the test’s mismatch assertions catch
// a recheck task that lacks the shard key only if the clusters return the
// duplicate copies in a revealing order, which they don’t guarantee.
func (suite *IntegrationTestSuite) generationRechecked(
	verifier *Verifier,
	generation int,
	id int,
) bool {
	count, err := verifier.verificationTaskCollection().CountDocuments(
		suite.T().Context(),
		bson.D{
			{"generation", generation},
			{"type", tasks.VerifyDocuments},
			{"_ids", id},
			{"query_filter.shardkeys", "sk"},
		},
	)
	suite.Require().NoError(err)

	return count > 0
}

func (suite *IntegrationTestSuite) getMismatchesForGeneration(
	verifier *Verifier,
	generation int,
) []compare.Result {
	ctx := suite.T().Context()

	cursor, err := verifier.verificationDatabase().Collection(mismatchesCollectionName).Find(
		ctx,
		bson.D{{"generation", generation}},
	)
	suite.Require().NoError(err)

	var infos []MismatchInfo
	suite.Require().NoError(cursor.All(ctx, &infos))

	return lo.Map(
		infos,
		func(mi MismatchInfo, _ int) compare.Result {
			return mi.Detail
		},
	)
}

// assertOnlyMismatchIs asserts that the given mismatches are all content
// mismatches for the given _id.
//
// There may be more than one such mismatch because a document whose _id is
// a partition boundary gets compared in both adjacent partitions.
//
// The test’s duplicated _ids have different `sk` values. So if the verifier
// compared a source copy with the wrong destination copy, it would report
// either a duplicate _id or a mismatch in `sk`.
func (suite *IntegrationTestSuite) assertOnlyMismatchIs(
	mismatches []compare.Result,
	id int,
) {
	if !suite.Assert().NotEmpty(mismatches, "should find a mismatch") {
		return
	}

	for _, mismatch := range mismatches {
		suite.Assert().Equal(id, lo.Must(bsontools.RawValueTo[int](mismatch.ID)), "mismatched _id: %+v", mismatch)
		suite.Assert().NotEqual(compare.DuplicateID, mismatch.Details, "should not report a duplicate: %+v", mismatch)
		suite.Assert().False(mismatch.DocumentIsMissing(), "document should not be missing: %+v", mismatch)
		suite.Assert().NotEqual("sk", mismatch.Field, "should compare the matching copy: %+v", mismatch)
	}
}
