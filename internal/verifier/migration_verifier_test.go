package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/cespare/permute/v2"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short-test mode.")
	}
	envVals := map[string]string{}

	for _, name := range []string{"MVTEST_SRC", "MVTEST_DST", "MVTEST_META"} {
		connStr := os.Getenv(name)
		if connStr == "" {
			t.Fatalf("%s requires %#q in environment.", t.Name(), name)
		}

		t.Logf("Found %s: %#q", name, connStr)

		envVals[name] = connStr
	}

	testSuite := &IntegrationTestSuite{
		srcConnStr:  envVals["MVTEST_SRC"],
		dstConnStr:  envVals["MVTEST_DST"],
		metaConnStr: envVals["MVTEST_META"],
	}

	oldLogLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(oldLogLevel)

	suite.Run(t, testSuite)
}

func (suite *IntegrationTestSuite) TestPartitionEmptyCollection() {
	ctx := suite.Context()
	require := require.New(suite.T())

	verifier := suite.BuildVerifier()

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	collName := "stuff"
	require.NoError(db.CreateCollection(ctx, collName))

	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Generation: 0,
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyCollection,
		QueryFilter: QueryFilter{
			Namespace: db.Name() + "." + collName,
			To:        db.Name() + "." + collName,
		},
	}

	partitions, docs, bytes, err := verifier.createPartitionTasksWithSampleRate(ctx, task)
	require.NoError(err, "should partition collection")

	assert.EqualValues(suite.T(), 1, partitions, "should be 1 partition")
	assert.Zero(suite.T(), docs, "should be 0 docs")
	assert.Zero(suite.T(), bytes, "should be 0 bytes")

	taskOpt, err := verifier.FindNextVerifyTaskAndUpdate(ctx)
	require.NoError(err, "should look up task")

	foundTask, gotTask := taskOpt.Get()
	require.True(gotTask, "should find task")

	require.Equal(verificationTaskVerifyDocuments, foundTask.Type, "task type")
	assert.Equal(
		suite.T(),
		bson.MinKey{},
		foundTask.QueryFilter.Partition.Key.Lower,
		"min bound",
	)

	assert.Equal(
		suite.T(),
		bson.MaxKey{},
		foundTask.QueryFilter.Partition.Upper,
		"max bound",
	)
}

func (suite *IntegrationTestSuite) TestProcessVerifyTask_Failure() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()
	t := suite.T()

	dbName := suite.DBNameForTest()
	collName := "coll"

	namespace := dbName + "." + collName

	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: QueryFilter{
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: 123,
				},
				Upper: 234,
			},
			Namespace: namespace,
			To:        namespace,
		},
	}

	err := verifier.ProcessVerifyTask(ctx, 12, task)

	expectedIDHex := task.PrimaryKey.Hex()

	assert.ErrorContains(t, err, expectedIDHex)
}

func (suite *IntegrationTestSuite) TestVerifier_Dotted_Shard_Key() {
	ctx := suite.T().Context()
	require := require.New(suite.T())

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		if suite.GetTopology(client) != util.TopologySharded {
			suite.T().Skip("sharded clusters required")
		}
	}

	dbName := suite.DBNameForTest()
	collName := "coll"

	docs := []bson.D{
		{{"_id", 33}, {"foo", bson.D{{"bar", 100}}}},
		{{"_id", 33}, {"foo", bson.D{{"bar", 200}}}},
	}

	shardKey := bson.D{
		{"foo.bar", 1},
	}

	keyField := shardKey[0].Key
	splitKey := bson.D{{keyField, 150}}

	clientsWithLabels := []struct {
		label  string
		client *mongo.Client
	}{
		{"source", suite.srcMongoClient},
		{"destination", suite.dstMongoClient},
	}

	for _, clientWithLabel := range clientsWithLabels {
		client := clientWithLabel.client
		clientLabel := clientWithLabel.label

		db := client.Database(dbName)
		coll := db.Collection(collName)

		// The DB shouldn’t exist anyway, but just in case.
		require.NoError(db.Drop(ctx), "should drop database")

		shardIds := getShardIds(suite.T(), client)

		admin := client.Database("admin")

		require.NoError(admin.RunCommand(
			ctx,
			bson.D{
				{"enableSharding", db.Name()},
			},
		).Err())

		require.NoError(
			admin.RunCommand(
				ctx,
				bson.D{
					{"shardCollection", FullName(coll)},
					{"key", shardKey},
				},
			).Err(),
			"should shard collection on %s",
			clientLabel,
		)

		require.NoError(
			util.DisableBalancing(ctx, coll),
			"should disable %#q’s balancing on %s",
			FullName(coll),
			clientLabel,
		)

		require.NoError(
			admin.RunCommand(ctx, bson.D{
				{"split", FullName(coll)},
				{"middle", splitKey},
			}).Err(),
			"should split on %s",
			clientLabel,
		)

		require.Eventually(
			func() bool {
				err := admin.RunCommand(ctx, bson.D{
					{"moveChunk", FullName(coll)},
					{"find", bson.D{{keyField, 149}}},
					{"to", shardIds[0]},
					{"_waitForDelete", true},
				}).Err()

				if err != nil {
					suite.T().Logf("Failed to move %s’s lower chunk to shard %#q: %v", clientLabel, shardIds[0], err)
					return false
				}

				return true
			},
			5*time.Minute,
			time.Second,
			"Should move lower chunk to the 1st shard",
		)

		require.Eventually(
			func() bool {
				err := admin.RunCommand(ctx, bson.D{
					{"moveChunk", FullName(coll)},
					{"find", bson.D{{keyField, 151}}},
					{"to", shardIds[1]},
					{"_waitForDelete", true},
				}).Err()

				if err != nil {
					suite.T().Logf("Failed to move %s’s upper chunk to shard %#q: %v", clientLabel, shardIds[1], err)
					return false
				}

				return true
			},
			5*time.Minute,
			time.Second,
			"Should move upper chunk to the 2nd shard",
		)

		_, err := coll.InsertMany(ctx, lo.ToAnySlice(lo.Shuffle(docs)))
		require.NoError(err, "should insert all docs")
	}

	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: QueryFilter{
			Namespace: dbName + "." + collName,
			To:        dbName + "." + collName,
			ShardKeys: lo.Map(
				shardKey,
				func(el bson.E, _ int) string {
					return el.Key
				},
			),
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: bson.MinKey{},
				},
				Upper: bson.MaxKey{},
			},
		},
	}

	verifier := suite.BuildVerifier()
	results, docCount, _, err := verifier.FetchAndCompareDocuments(ctx, 0, task)
	require.NoError(err, "should fetch & compare")
	assert.EqualValues(suite.T(), len(docs), docCount, "expected # of docs")
	assert.Empty(suite.T(), results, "should find no problem")
}

func getShardIds(t *testing.T, client *mongo.Client) []string {
	res, err := runListShards(t.Context(), logger.NewDefaultLogger(), client)
	require.NoError(t, err)

	type shardData struct {
		Id string `bson:"_id"`
	}
	var parsed struct {
		Shards []shardData
	}

	require.NoError(t, res.Decode(&parsed))

	return lo.Map(
		parsed.Shards,
		func(sd shardData, _ int) string {
			return sd.Id
		},
	)
}

func (suite *IntegrationTestSuite) TestVerifier_DocFilter_ObjectID() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()
	t := suite.T()

	dbName := suite.DBNameForTest()
	collName := "coll"

	srcColl := verifier.srcClient.Database(dbName).Collection(collName)
	dstColl := verifier.dstClient.Database(dbName).Collection(collName)

	id1 := bson.NewObjectID()
	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", id1}})
	require.NoError(t, err, "should insert to source")

	id2 := bson.NewObjectID()
	_, err = srcColl.InsertOne(ctx, bson.D{{"_id", id2}})
	require.NoError(t, err, "should insert to source")

	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", id1}})
	require.NoError(t, err, "should insert to destination")

	namespace := dbName + "." + collName

	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Ids:        []any{id1, id2},
		QueryFilter: QueryFilter{
			Namespace: namespace,
			To:        namespace,
		},
	}

	verifier.globalFilter = bson.D{{"_id", id1}}

	results, docCount, _, err := verifier.FetchAndCompareDocuments(ctx, 0, task)
	require.NoError(t, err, "should fetch & compare")
	assert.EqualValues(t, 1, docCount, "should compare 1 doc")
	assert.Empty(t, results, "should find no problem")

	verifier.globalFilter = bson.D{{"_id", id2}}
	results, docCount, _, err = verifier.FetchAndCompareDocuments(ctx, 0, task)
	require.NoError(t, err, "should fetch & compare")
	assert.EqualValues(t, 1, docCount, "should compare 1 doc")
	assert.NotEmpty(t, results, "should find a problem")
}

func (suite *IntegrationTestSuite) TestTypesBetweenBoundaries() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: QueryFilter{
			Namespace: "keyhole.dealers",
			To:        "keyhole.dealers",
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: bson.MinKey{},
				},
				Upper: int32(999),
			},
		},
	}

	_, err := verifier.srcClient.Database("keyhole").Collection("dealers").InsertMany(ctx, []any{
		bson.D{{"_id", nil}},
		bson.D{{"_id", int32(123)}},
		bson.D{{"_id", bson.Symbol("oh yeah")}},
	})
	suite.Require().NoError(err)

	_, err = verifier.dstClient.Database("keyhole").Collection("dealers").InsertMany(ctx, []any{
		bson.D{{"_id", nil}},
		bson.D{{"_id", int32(123)}},
		bson.D{{"_id", "oh yeah"}},
	})
	suite.Require().NoError(err)

	cases := []struct {
		label                 string
		lower, upper          any
		docsCount, mismatches int
	}{
		{
			label:     "MinKey to int 999",
			lower:     bson.MinKey{},
			upper:     int32(999),
			docsCount: 2,
		},
		{
			label:     "between numeric types",
			lower:     int64(123),
			upper:     float64(9999),
			docsCount: 1,
		},
		{
			label:     "between adjacent types",
			lower:     int64(1),
			upper:     "hey",
			docsCount: 1,
		},
		{
			label:      "between non-adjacent types, including type of upper",
			lower:      bson.Null{},
			upper:      "zzzz",
			docsCount:  3,
			mismatches: 2,
		},
		{
			label:     "between non-adjacent types, excluding type of upper",
			lower:     bson.Null{},
			upper:     "aaa",
			docsCount: 2,
		},
		{
			label:      "0 to MaxKey",
			lower:      0,
			upper:      bson.MaxKey{},
			docsCount:  2,
			mismatches: 2,
		},
		{
			label:      "long 999 to MaxKey",
			lower:      int64(999),
			upper:      bson.MaxKey{},
			docsCount:  1,
			mismatches: 2,
		},
	}

	for _, curCase := range cases {
		task.QueryFilter.Partition.Key.Lower = curCase.lower
		task.QueryFilter.Partition.Upper = curCase.upper

		suite.Run(
			curCase.label,
			func() {
				results, docCount, byteCount, err := verifier.FetchAndCompareDocuments(ctx, 0, task)
				suite.Require().NoError(err)

				suite.Assert().EqualValues(curCase.docsCount, docCount, "docs count")

				if docCount > 0 {
					suite.Assert().Greater(int(byteCount), 1, "byte count")
				} else {
					suite.Assert().Zero(byteCount, "byte count")
				}

				suite.Assert().Len(results, curCase.mismatches, "expected mismatches")
			},
		)
	}
}

func (suite *IntegrationTestSuite) TestVerifierFetchDocuments() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()
	drop := func() {
		err := verifier.srcClient.Database("keyhole").Drop(ctx)
		suite.Require().NoError(err)
		err = verifier.dstClient.Database("keyhole").Drop(ctx)
		suite.Require().NoError(err)
	}
	drop()
	defer drop()

	// create a basicQueryFilter that sets (source) Namespace and To
	// to the same thing
	basicQueryFilter := func(namespace string) QueryFilter {
		return QueryFilter{
			Namespace: namespace,
			To:        namespace,
		}
	}

	id := rand.Intn(1000)
	_, err := verifier.srcClient.Database("keyhole").Collection("dealers").InsertMany(ctx, []any{
		bson.D{{"_id", id}, {"num", 99}, {"name", "srcTest"}},
		bson.D{{"_id", id + 1}, {"num", 101}, {"name", "srcTest"}},
	})
	suite.Require().NoError(err)
	_, err = verifier.dstClient.Database("keyhole").Collection("dealers").InsertMany(ctx, []any{
		bson.D{{"_id", id}, {"num", 99}, {"name", "dstTest"}},
		bson.D{{"_id", id + 1}, {"num", 101}, {"name", "dstTest"}},
	})
	suite.Require().NoError(err)
	task := &VerificationTask{
		PrimaryKey:  bson.NewObjectID(),
		Generation:  1,
		Ids:         []any{id, id + 1},
		QueryFilter: basicQueryFilter("keyhole.dealers"),
	}

	// Test fetchDocuments without global filter.
	verifier.globalFilter = nil
	results, docCount, byteCount, err := verifier.FetchAndCompareDocuments(ctx, 0, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(2, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs' size")
	suite.Assert().Len(results, 2)
	for _, res := range results {
		suite.Assert().Regexp(regexp.MustCompile("^"+Mismatch), res.Details, "details as expected")
	}

	// Test fetchDocuments for ids with a global filter.

	verifier.globalFilter = bson.D{
		{"num", map[string]any{"$lt": 100}},
	}
	results, docCount, byteCount, err = verifier.FetchAndCompareDocuments(ctx, 0, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(1, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs' size")
	suite.Require().Len(results, 1)
	suite.Assert().Regexp(regexp.MustCompile("^"+Mismatch), results[0].Details, "mismatch expected")
	suite.Assert().EqualValues(
		any(id),
		results[0].ID.AsInt64(),
		"mismatch recorded as expeceted",
	)

	// Test fetchDocuments for a partition with a global filter.
	task.QueryFilter.Partition = &partitions.Partition{
		Ns: &partitions.Namespace{DB: "keyhole", Coll: "dealers"},
	}
	verifier.globalFilter = bson.D{
		{"num", map[string]any{"$lt": 100}},
	}
	results, docCount, byteCount, err = verifier.FetchAndCompareDocuments(ctx, 0, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(1, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs' size")
	suite.Require().Len(results, 1)
	suite.Assert().Regexp(regexp.MustCompile("^"+Mismatch), results[0].Details, "mismatch expeceted")
	suite.Assert().EqualValues(
		any(id),
		results[0].ID.AsInt64(),
		"mismatch recorded as expeceted",
	)
}

func (suite *IntegrationTestSuite) TestGetPersistedNamespaceStatistics_Metadata() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	dbName := suite.DBNameForTest()

	err := verifier.srcClient.Database(dbName).CreateCollection(
		ctx,
		"foo",
	)
	suite.Require().NoError(err)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		mslices.Of(NamespaceStats{
			Namespace: dbName + ".foo",
		}),
		stats,
		"stats should be as expected",
	)

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		mslices.Of(NamespaceStats{
			Namespace: dbName + ".foo",
		}),
		stats,
		"stats should be as expected",
	)
}

func (suite *IntegrationTestSuite) TestGetPersistedNamespaceStatistics_OneDoc() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	bsonDoc := lo.Must(bson.Marshal(bson.D{{"_id", "foo"}}))

	dbName := suite.DBNameForTest()
	_, err := verifier.srcClient.Database(dbName).Collection("foo").
		InsertOne(ctx, bsonDoc)
	suite.Require().NoError(err)

	err = verifier.dstClient.Database(dbName).CreateCollection(
		ctx,
		"foo",
	)
	suite.Require().NoError(err)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Require().NotEmpty(stats)
	suite.Assert().NotZero(stats[0].BytesCompared, "bytes compared should be set")

	suite.Assert().Equal(
		mslices.Of(NamespaceStats{
			Namespace:      dbName + ".foo",
			DocsCompared:   1,
			TotalDocs:      1,
			BytesCompared:  stats[0].BytesCompared,
			TotalBytes:     types.ByteCount(len(bsonDoc)),
			PartitionsDone: 1,
		}),
		stats,
		"stats should be as expected",
	)

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Require().NotEmpty(stats)
	suite.Assert().NotZero(stats[0].BytesCompared, "bytes compared should be set")

	suite.Assert().Equal(
		mslices.Of(NamespaceStats{
			Namespace:      dbName + ".foo",
			DocsCompared:   1,
			TotalDocs:      1,
			BytesCompared:  stats[0].BytesCompared,
			PartitionsDone: 1,

			// NB: TotalBytes is 0 because we can’t compute that from the
			// change stream.
		}),
		stats,
		"stats should be as expected",
	)
}

func (suite *IntegrationTestSuite) TestGetPersistedNamespaceStatistics_Recheck() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	err := verifier.HandleChangeStreamEvents(
		ctx,
		changeEventBatch{
			events: []ParsedEvent{{
				OpType: "insert",
				Ns:     &Namespace{DB: "mydb", Coll: "coll2"},
				DocID:  mbson.ToRawValue("heyhey"),
				ClusterTime: &bson.Timestamp{
					T: uint32(time.Now().Unix()),
				},
			}},
		},
		src,
	)
	suite.Require().NoError(err)

	err = verifier.HandleChangeStreamEvents(
		ctx,
		changeEventBatch{
			events: []ParsedEvent{{
				OpType: "insert",
				Ns:     &Namespace{DB: "mydb", Coll: "coll1"},
				DocID:  mbson.ToRawValue("hoohoo"),
				ClusterTime: &bson.Timestamp{
					T: uint32(time.Now().Unix()),
				},
			}},
		},
		src,
	)
	suite.Require().NoError(err)

	verifier.generation++

	func() {
		verifier.mux.Lock()
		defer func() { verifier.mux.Unlock() }()
		suite.Require().NoError(verifier.GenerateRecheckTasksWhileLocked(ctx))
	}()

	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{
				Namespace:       "mydb.coll1",
				TotalDocs:       1,
				PartitionsAdded: 1,
			},
			{
				Namespace:       "mydb.coll2",
				TotalDocs:       1,
				PartitionsAdded: 1,
			},
		},
		stats,
		"Stats as expected (TotalBytes=0)",
	)
}

func (suite *IntegrationTestSuite) TestGetNamespaceStatistics_Gen0() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	stats, err := verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{},
		stats,
		"Stats are empty at first",
	)

	// Now add 2 namespaces. Add them “out of order” to test
	// that we sort the returned array by Namespace.

	task2, err := verifier.InsertCollectionVerificationTask(ctx, "mydb.coll2")
	suite.Require().NoError(err)

	task1, err := verifier.InsertCollectionVerificationTask(ctx, "mydb.coll1")
	suite.Require().NoError(err)

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{Namespace: task1.QueryFilter.Namespace},
			{Namespace: task2.QueryFilter.Namespace},
		},
		stats,
		"One stats struct for each namespace",
	)

	// Now add document counts for each namespace.

	task1.Status = verificationTaskCompleted
	task1.SourceDocumentCount = 1000
	task1.SourceByteCount = 10_000

	task2.Status = verificationTaskCompleted
	task2.SourceDocumentCount = 900
	task2.SourceByteCount = 9_000

	err = verifier.UpdateVerificationTask(ctx, task2)
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(ctx, task1)
	suite.Require().NoError(err)

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{
				Namespace:  task1.QueryFilter.Namespace,
				TotalDocs:  task1.SourceDocumentCount,
				TotalBytes: task1.SourceByteCount,
			},
			{
				Namespace:  task2.QueryFilter.Namespace,
				TotalDocs:  task2.SourceDocumentCount,
				TotalBytes: task2.SourceByteCount,
			},
		},
		stats,
		"Stats after namespaces are scanned (no partitions added)",
	)

	// Now add 2 partitions for each namespace.

	task1parts := [2]*VerificationTask{}
	task2parts := [2]*VerificationTask{}
	for i := range task1parts {
		task1part, err := verifier.InsertPartitionVerificationTask(
			ctx,
			&partitions.Partition{
				Ns: &partitions.Namespace{DB: "mydb", Coll: "coll1"},
			},
			[]string{},
			"faux.dstnamespace",
		)
		suite.Require().NoError(err)

		task1parts[i] = task1part

		task2part, err := verifier.InsertPartitionVerificationTask(
			ctx,
			&partitions.Partition{
				Ns: &partitions.Namespace{DB: "mydb", Coll: "coll2"},
			},
			[]string{},
			"faux.dstnamespace",
		)
		suite.Require().NoError(err)

		task2parts[i] = task2part
	}

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{
				Namespace:       task1.QueryFilter.Namespace,
				TotalDocs:       task1.SourceDocumentCount,
				TotalBytes:      task1.SourceByteCount,
				PartitionsAdded: 2,
			},
			{
				Namespace:       task2.QueryFilter.Namespace,
				TotalDocs:       task2.SourceDocumentCount,
				TotalBytes:      task2.SourceByteCount,
				PartitionsAdded: 2,
			},
		},
		stats,
		"Stats after namespaces are partitioned",
	)

	// Now set one task to status=processing

	task1parts[0].Status = verificationTaskProcessing
	err = verifier.UpdateVerificationTask(ctx, task1parts[0])
	suite.Require().NoError(err)

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{
				Namespace:            task1.QueryFilter.Namespace,
				TotalDocs:            task1.SourceDocumentCount,
				TotalBytes:           task1.SourceByteCount,
				PartitionsAdded:      1,
				PartitionsProcessing: 1,
			},
			{
				Namespace:       task2.QueryFilter.Namespace,
				TotalDocs:       task2.SourceDocumentCount,
				TotalBytes:      task2.SourceByteCount,
				PartitionsAdded: 2,
			},
		},
		stats,
		"Stats after one partition is started",
	)

	// Now set two other tasks to completed/failed.

	task2parts[0].Status = verificationTaskCompleted
	task2parts[0].SourceDocumentCount = task2.SourceDocumentCount / 2
	task2parts[0].SourceByteCount = task2.SourceByteCount / 2

	task2parts[1].Status = verificationTaskCompleted
	task2parts[1].SourceDocumentCount = task2.SourceDocumentCount / 2
	task2parts[1].SourceByteCount = task2.SourceByteCount / 2

	err = verifier.UpdateVerificationTask(ctx, task2parts[0])
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(ctx, task2parts[1])
	suite.Require().NoError(err)

	stats, err = verifier.GetPersistedNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{
			{
				Namespace:            task1.QueryFilter.Namespace,
				TotalDocs:            task1.SourceDocumentCount,
				TotalBytes:           task1.SourceByteCount,
				PartitionsAdded:      1,
				PartitionsProcessing: 1,
			},
			{
				Namespace:      task2.QueryFilter.Namespace,
				TotalDocs:      task2.SourceDocumentCount,
				TotalBytes:     task2.SourceByteCount,
				PartitionsDone: 2,
				DocsCompared:   task2.SourceDocumentCount,
				BytesCompared:  task2.SourceByteCount,
			},
		},
		stats,
		"Stats after one namespace is finished",
	)
}

func (suite *IntegrationTestSuite) TestFailedVerificationTaskInsertions() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()
	err := verifier.InsertFailedCompareRecheckDocs(
		ctx,
		"foo.bar",
		mslices.Of(mbson.ToRawValue(42)),
		[]int{100},
	)
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs(
		ctx,
		"foo.bar",
		mslices.Of(mbson.ToRawValue(43), mbson.ToRawValue(44)),
		[]int{100, 100},
	)
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs(
		ctx,
		"foo.bar2",
		mslices.Of(mbson.ToRawValue(42)),
		[]int{100},
	)
	suite.Require().NoError(err)
	event := ParsedEvent{
		DocID:  mbson.ToRawValue(int32(55)),
		OpType: "delete",
		Ns: &Namespace{
			DB:   "foo",
			Coll: "bar2",
		},
		ClusterTime: &bson.Timestamp{
			T: uint32(time.Now().Unix()),
		},
	}

	batch := changeEventBatch{
		events: mslices.Of(event),
	}

	err = verifier.HandleChangeStreamEvents(ctx, batch, src)
	suite.Require().NoError(err)
	event.OpType = "insert"
	err = verifier.HandleChangeStreamEvents(ctx, batch, src)
	suite.Require().NoError(err)
	event.OpType = "replace"
	err = verifier.HandleChangeStreamEvents(ctx, batch, src)
	suite.Require().NoError(err)
	event.OpType = "update"
	err = verifier.HandleChangeStreamEvents(ctx, batch, src)
	suite.Require().NoError(err)

	batch.events[0].OpType = "flibbity"
	suite.Assert().Panics(
		func() {
			_ = verifier.HandleChangeStreamEvents(ctx, batch, src)
		},
		"HandleChangeStreamEvents should panic if it gets an unknown optype",
	)

	verifier.generation++
	func() {
		verifier.mux.Lock()
		defer verifier.mux.Unlock()

		err = verifier.GenerateRecheckTasksWhileLocked(ctx)
		suite.Require().NoError(err)
	}()

	var doc bson.M
	cur, err := verifier.verificationTaskCollection().Find(ctx, bson.M{"generation": 1})
	verifyTask := func(expectedIds bson.A, expectedNamespace string) {
		more := cur.Next(ctx)
		suite.Require().True(more)
		err = cur.Decode(&doc)
		suite.Require().NoError(err)
		suite.Require().Equal(expectedIds, doc["_ids"])
		suite.Require().EqualValues(verificationTaskAdded, doc["status"])
		suite.Require().EqualValues(verificationTaskVerifyDocuments, doc["type"])
		suite.Require().Equal(
			expectedNamespace,
			lo.Must(cur.Current.LookupErr("query_filter", "namespace")).StringValue(),
		)
	}
	verifyTask(bson.A{int32(42), int32(43), int32(44)}, "foo.bar")
	verifyTask(bson.A{int32(42), int32(55)}, "foo.bar2")
	suite.Require().False(cur.Next(ctx))
}

func TestVerifierCompareDocs(t *testing.T) {
	id := rand.Intn(1000)
	verifier := NewVerifier(VerifierSettings{}, "stderr")
	verifier.SetDocCompareMethod(DocCompareIgnoreOrder)

	type compareTest struct {
		label       string
		srcDocs     []bson.D
		dstDocs     []bson.D
		indexFields []string
		checkOrder  bool
		compareFn   func(*testing.T, []VerificationResult)
	}

	compareTests := []compareTest{
		{
			label: "simple equality",
			srcDocs: []bson.D{
				{{"_id", id}, {"num", 123}, {"name", "foobar"}},
			},
			dstDocs: []bson.D{
				{{"_id", id}, {"num", 123}, {"name", "foobar"}},
			},
			compareFn: func(t *testing.T, mismatchedIds []VerificationResult) {
				assert.Empty(t, mismatchedIds)
			},
		},

		{
			label: "different order (ignore)",
			srcDocs: []bson.D{
				{{"_id", id}, {"name", "foobar"}, {"num", 123}},
			},
			dstDocs: []bson.D{
				{{"_id", id}, {"num", 123}, {"name", "foobar"}},
			},
			compareFn: func(t *testing.T, mismatchedIds []VerificationResult) {
				assert.Empty(t, mismatchedIds)
			},
		},

		{
			label:      "different order (check)",
			checkOrder: true,
			srcDocs: []bson.D{
				{{"_id", id}, {"name", "foobar"}, {"num", 123}},
			},
			dstDocs: []bson.D{
				{{"_id", id}, {"num", 123}, {"name", "foobar"}},
			},
			compareFn: func(t *testing.T, mismatchResults []VerificationResult) {
				if assert.Equal(t, 1, len(mismatchResults)) {
					var res int
					require.Nil(t, mismatchResults[0].ID.Unmarshal(&res))
					assert.Equal(t, id, res)
					assert.Regexp(t, regexp.MustCompile("^"+Mismatch), mismatchResults[0].Details)
				}
			},
		},

		{
			label: "mismatched",
			srcDocs: []bson.D{
				{{"_id", id}, {"num", 1234}, {"name", "foobar"}},
			},
			dstDocs: []bson.D{
				{{"_id", id}, {"num", 123}, {"name", "foobar"}},
			},
			compareFn: func(t *testing.T, mismatchResults []VerificationResult) {
				if assert.Equal(t, 1, len(mismatchResults)) {
					var res int
					require.Nil(t, mismatchResults[0].ID.Unmarshal(&res))
					assert.Equal(t, id, res)
					assert.Regexp(t, regexp.MustCompile("^"+Mismatch), mismatchResults[0].Details)
				}
			},
		},

		{
			label: "document missing on destination",
			srcDocs: []bson.D{
				{{"_id", id}, {"num", 1234}, {"name", "foobar"}},
			},
			dstDocs: []bson.D{},
			compareFn: func(t *testing.T, mismatchedIds []VerificationResult) {
				if assert.Equal(t, 1, len(mismatchedIds)) {
					assert.Equal(t, mismatchedIds[0].Details, Missing)
					assert.Equal(t, mismatchedIds[0].Cluster, ClusterTarget)
				}
			},
		},

		{
			label:   "document missing on source",
			srcDocs: []bson.D{},
			dstDocs: []bson.D{
				{{"_id", id}, {"num", 1234}, {"name", "foobar"}},
			},
			compareFn: func(t *testing.T, mismatchedIds []VerificationResult) {
				if assert.Equal(t, 1, len(mismatchedIds)) {
					assert.Equal(t, mismatchedIds[0].Details, Missing)
					assert.Equal(t, mismatchedIds[0].Cluster, ClusterSource)
				}
			},
		},

		{
			label:       "duplicate ID",
			indexFields: []string{"sharded"},
			srcDocs: []bson.D{
				{{"_id", id}, {"sharded", 123}},
				{{"_id", id}, {"sharded", 234}},
				{{"_id", id}, {"sharded", 345}},
			},
			dstDocs: []bson.D{
				{{"_id", id}, {"sharded", 234}},
				{{"_id", id}, {"sharded", 345}},
				{{"_id", id}, {"sharded", 123}},
			},
			compareFn: func(t *testing.T, mismatchedIds []VerificationResult) {
				assert.Empty(t, mismatchedIds, "should be no problems")
			},
		},
	}

	namespace := "testdb.testns"

	makeDocChannel := func(docs []bson.D) <-chan docWithTs {
		theChan := make(chan docWithTs, len(docs))

		for d, doc := range docs {
			theChan <- docWithTs{
				doc: testutil.MustMarshal(doc),
				ts:  bson.Timestamp{1, uint32(d)},
			}
		}

		close(theChan)

		return theChan
	}

	for _, curTest := range compareTests {
		verifier.SetDocCompareMethod(
			lo.Ternary(
				!curTest.checkOrder,
				DocCompareIgnoreOrder,
				DocCompareBinary,
			),
		)

		indexFields := curTest.indexFields
		if indexFields == nil {
			indexFields = []string{}
		}

		srcDocs := curTest.srcDocs
		dstDocs := curTest.dstDocs

		permuted := len(srcDocs)*len(dstDocs) > 1
		curPermutation := 0

		srcPermute := permute.Slice(srcDocs)
		for srcPermute.Permute() {

			dstPermute := permute.Slice(dstDocs)
			for dstPermute.Permute() {
				srcChannel := makeDocChannel(srcDocs)
				dstChannel := makeDocChannel(dstDocs)

				fauxTask := VerificationTask{
					PrimaryKey: bson.NewObjectID(),
					QueryFilter: QueryFilter{
						Namespace: namespace,
						ShardKeys: indexFields,
					},
				}
				var results []VerificationResult
				var docCount types.DocumentCount
				var byteCount types.ByteCount

				err := retry.New().WithCallback(
					func(ctx context.Context, fi *retry.FuncInfo) error {
						var err error

						results, docCount, byteCount, err = verifier.compareDocsFromChannels(
							ctx,
							0,
							fi,
							&fauxTask,
							srcChannel,
							dstChannel,
						)

						return err
					},
					"comparing documents",
				).Run(context.Background(), logger.NewDefaultLogger())

				assert.EqualValues(t, len(srcDocs), docCount)
				assert.Equal(t, len(srcDocs) > 0, byteCount > 0, "byte count should match docs")

				label := curTest.label
				if permuted {
					curPermutation++
					label += fmt.Sprintf(" permutation %d", curPermutation)
				}

				ok := t.Run(
					label,
					func(t *testing.T) {
						require.NoError(t, err)
						curTest.compareFn(t, results)
					},
				)

				if !ok {
					if len(curTest.srcDocs) > 0 {
						t.Logf("%#q src: %+v", label, curTest.srcDocs)
					}
					if len(curTest.dstDocs) > 0 {
						t.Logf("%#q dst: %+v", label, curTest.dstDocs)
					}
				}
			}
		}
	}
}

func (suite *IntegrationTestSuite) getFailuresForTask(
	verifier *Verifier,
	taskID bson.ObjectID,
) []VerificationResult {
	discrepancies, err := getMismatchesForTasks(
		suite.Context(),
		verifier.verificationDatabase(),
		mslices.Of(taskID),
	)

	require.NoError(suite.T(), err)

	return slices.Collect(maps.Values(discrepancies))[0]
}

func (suite *IntegrationTestSuite) TestVerifierCompareViews() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	err := suite.srcMongoClient.Database("testDb").CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.sameView",
			To:        "testDb.sameView"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskCompleted, task.Status)
	suite.Empty(suite.getFailuresForTask(verifier, task.PrimaryKey))

	// Views must have the same underlying collection
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "wrongColl", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "wrongColl", "testColl2", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.wrongColl",
			To:        "testDb.wrongColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures := suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(failures[0].Field, "Options.viewOn")
		suite.Equal(failures[0].Cluster, ClusterTarget)
		suite.Equal(failures[0].NameSpace, "testDb.wrongColl")
	}

	// Views must have the same underlying pipeline
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}, {"a", 0}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.wrongPipeline",
			To:        "testDb.wrongPipeline"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(failures[0].Field, "Options.pipeline")
		suite.Equal(failures[0].Cluster, ClusterTarget)
		suite.Equal(failures[0].NameSpace, "testDb.wrongPipeline")
	}

	// Views must have the same underlying options
	var collation1, collation2 options.Collation
	collation1.Locale = "en_US"
	collation1.CaseLevel = true
	collation2.Locale = "fr"
	collation2.Backwards = true
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "missingOptionsSrc", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "missingOptionsSrc", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation2))
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.missingOptionsSrc",
			To:        "testDb.missingOptionsSrc"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(failures[0].Field, "Options.collation")
		suite.Equal(failures[0].Cluster, ClusterSource)
		suite.Equal(failures[0].Details, "Missing")
		suite.Equal(failures[0].NameSpace, "testDb.missingOptionsSrc")
	}

	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.missingOptionsDst",
			To:        "testDb.missingOptionsDst"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(failures[0].Field, "Options.collation")
		suite.Equal(failures[0].Cluster, ClusterTarget)
		suite.Equal(failures[0].Details, "Missing")
		suite.Equal(failures[0].NameSpace, "testDb.missingOptionsDst")
	}

	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation2))
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.differentOptions",
			To:        "testDb.differentOptions"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(failures[0].Field, "Options.collation")
		suite.Equal(failures[0].Cluster, ClusterTarget)
		suite.Equal(failures[0].NameSpace, "testDb.differentOptions")
	}
}

func (suite *IntegrationTestSuite) TestVerifierCompareMetadata() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	// Collection exists only on source.
	err := suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures := suite.getFailuresForTask(verifier, task.PrimaryKey)
	suite.Equal(1, len(failures))
	suite.Equal(failures[0].Details, Missing)
	suite.Equal(failures[0].Cluster, ClusterTarget)
	suite.Equal(failures[0].NameSpace, "testDb.testColl")

	// Make sure "To" is respected.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testCollTo"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	suite.Equal(1, len(failures))
	suite.Equal(failures[0].Details, Missing)
	suite.Equal(failures[0].Cluster, ClusterTarget)
	suite.Equal(failures[0].NameSpace, "testDb.testCollTo")

	// Collection exists only on dest.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "destOnlyColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.destOnlyColl",
			To:        "testDb.destOnlyColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	suite.Equal(1, len(failures))
	suite.Equal(failures[0].Details, Missing)
	suite.Equal(failures[0].Cluster, ClusterSource)
	suite.Equal(failures[0].NameSpace, "testDb.destOnlyColl")

	// A view and a collection are different.
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "viewOnSrc", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "viewOnSrc")
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.viewOnSrc",
			To:        "testDb.viewOnSrc"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	suite.Equal(1, len(failures))
	suite.Equal(failures[0].Field, "Type")
	suite.Equal(failures[0].Cluster, ClusterTarget)
	suite.Equal(failures[0].NameSpace, "testDb.viewOnSrc")

	// Capped should not match uncapped
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst", options.CreateCollection().SetCapped(true).SetSizeInBytes(1024*1024*100))
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.cappedOnDst",
			To:        "testDb.cappedOnDst"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	// Capped and size should differ
	var wrongFields []string

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	for _, result := range failures {
		field := result.Field
		suite.Require().NotNil(field)
		wrongFields = append(wrongFields, field)
	}
	suite.ElementsMatch([]string{"Options.capped", "Options.size"}, wrongFields)

	// Default success case
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskCompleted, task.Status)

	// Neither collection exists success case
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testCollDNE",
			To:        "testDb.testCollDNE"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskCompleted, task.Status)
}

func (suite *IntegrationTestSuite) TestVerifierCompareIndexes() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	// Missing index on destination.
	err := suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl1")
	srcColl := suite.srcMongoClient.Database("testDb").Collection("testColl1")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	dstColl := suite.dstMongoClient.Database("testDb").Collection("testColl1")
	suite.Require().NoError(err)
	srcIndexNames, err := srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	_, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	task := &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl1",
			To:        "testDb.testColl1",
		},
	}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)

	failures := suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(mbson.ToRawValue(srcIndexNames[1]), failures[0].ID)
		suite.Equal(Missing, failures[0].Details)
		suite.Equal(ClusterTarget, failures[0].Cluster)
		suite.Equal("testDb.testColl1", failures[0].NameSpace)
	}

	// Missing index on source
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl2")
	srcColl = suite.srcMongoClient.Database("testDb").Collection("testColl2")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl2")
	suite.Require().NoError(err)
	_, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	dstIndexNames, err := dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl2",
			To:        "testDb.testColl2"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	suite.T().Logf("failures: %+v", failures)

	if suite.Equal(1, len(failures)) {
		suite.Equal(mbson.ToRawValue(dstIndexNames[1]), failures[0].ID)
		suite.Equal(Missing, failures[0].Details)
		suite.Equal(ClusterSource, failures[0].Cluster)
		suite.Equal("testDb.testColl2", failures[0].NameSpace)
	}

	// Different indexes on each
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl3")
	srcColl = suite.srcMongoClient.Database("testDb").Collection("testColl3")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl3")
	suite.Require().NoError(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl3",
			To:        "testDb.testColl3"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)

	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(2, len(failures)) {
		sort.Slice(failures, func(i, j int) bool {
			return failures[i].ID.StringValue() < failures[j].ID.StringValue()
		})
		suite.Equal(mbson.ToRawValue(dstIndexNames[1]), failures[0].ID)
		suite.Equal(Missing, failures[0].Details)
		suite.Equal(ClusterSource, failures[0].Cluster)
		suite.Equal("testDb.testColl3", failures[0].NameSpace)
		suite.Equal(mbson.ToRawValue(srcIndexNames[0]), failures[1].ID)
		suite.Equal(Missing, failures[1].Details)
		suite.Equal(ClusterTarget, failures[1].Cluster)
		suite.Equal("testDb.testColl3", failures[1].NameSpace)
	}

	// Indexes with same names are different
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl4")
	srcColl = suite.srcMongoClient.Database("testDb").Collection("testColl4")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl4")
	suite.Require().NoError(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}, Options: options.Index().SetName("wrong")}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	suite.Require().Equal("wrong", srcIndexNames[0])
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}, Options: options.Index().SetName("wrong")}})
	suite.Require().NoError(err)
	suite.Require().Equal("wrong", dstIndexNames[1])
	task = &VerificationTask{
		PrimaryKey: bson.NewObjectID(),
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl4",
			To:        "testDb.testColl4"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	failures = suite.getFailuresForTask(verifier, task.PrimaryKey)
	if suite.Equal(1, len(failures)) {
		suite.Equal(mbson.ToRawValue("wrong"), failures[0].ID)
		suite.Regexp(regexp.MustCompile("^"+Mismatch), failures[0].Details)
		suite.Equal(ClusterTarget, failures[0].Cluster)
		suite.Equal("testDb.testColl4", failures[0].NameSpace)
	}
}

func (suite *IntegrationTestSuite) TestVerifierDocMismatches() {
	ctx := suite.Context()

	suite.Require().NoError(
		suite.srcMongoClient.
			Database("test").
			Collection("coll").Drop(ctx),
	)
	suite.Require().NoError(
		suite.dstMongoClient.
			Database("test").
			Collection("coll").Drop(ctx),
	)

	_, err := suite.srcMongoClient.
		Database("test").
		Collection("coll").
		InsertMany(
			ctx,
			lo.RepeatBy(
				20,
				func(index int) any {
					return bson.D{
						{"_id", 100000 + index},
						{"foo", 3},
					}
				},
			),
		)
	suite.Require().NoError(err)

	// The first has a mismatched `foo` value,
	// and the 2nd lacks `foo` entirely.
	_, err = suite.dstMongoClient.
		Database("test").
		Collection("coll").
		InsertMany(ctx, lo.ToAnySlice([]bson.D{
			{{"_id", 100000}, {"foo", 1}},
			{{"_id", 100001}},
		}))
	suite.Require().NoError(err)

	verifier := suite.BuildVerifier()
	verifier.failureDisplaySize = 10

	ns := "test.coll"
	verifier.SetSrcNamespaces([]string{ns})
	verifier.SetDstNamespaces([]string{ns})
	verifier.SetNamespaceMap()

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	builder := &strings.Builder{}
	_, _, err = verifier.reportDocumentMismatches(ctx, builder)
	suite.Require().NoError(err)

	suite.Assert().Contains(
		builder.String(),
		"100009",
		"summary should show an early mismatch",
	)

	suite.Assert().Contains(
		builder.String(),
		" 10 ",
		"summary should show the # of missing docs shown",
	)

	suite.Assert().Contains(
		builder.String(),
		" 18 ",
		"summary should show the total # of missing/changed documents",
	)

	suite.Assert().NotContains(
		builder.String(),
		"100019",
		"summary should NOT show a late mismatch",
	)
}

func (suite *IntegrationTestSuite) TestVerifierCompareIndexSpecs() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	cases := []struct {
		label       string
		src         bson.D
		dst         bson.D
		shouldMatch bool
	}{
		{
			label: "simple",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 123}},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 123}},
			},
			shouldMatch: true,
		},

		{
			label: "ignore `ns` field",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 123}},
				{"ns", "foo.bar"},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 123}},
			},
			shouldMatch: true,
		},

		{
			label: "ignore number types",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 123}},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": float64(123)}},
			},
			shouldMatch: true,
		},

		{
			label: "ignore number types, deep",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo.bar": float64(123)}},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo.bar": 123}},
			},
			shouldMatch: true,
		},

		{
			label: "find number differences",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": 1}},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.M{"foo": -1}},
			},
			shouldMatch: false,
		},

		{
			label: "key order differences",
			src: bson.D{
				{"name", "testIndex"},
				{"key", bson.D{{"foo", 1}, {"bar", 1}}},
			},
			dst: bson.D{
				{"name", "testIndex"},
				{"key", bson.D{{"bar", 1}, {"foo", 1}}},
			},
			shouldMatch: false,
		},
	}

	for _, curCase := range cases {
		matchYN, err := verifier.doIndexSpecsMatch(
			ctx,
			testutil.MustMarshal(curCase.src),
			testutil.MustMarshal(curCase.dst),
		)
		suite.Require().NoError(err)
		suite.Assert().Equal(curCase.shouldMatch, matchYN, curCase.label)
	}
}

func (suite *IntegrationTestSuite) TestVerifierNamespaceList() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	// Collections on source only
	err := suite.srcMongoClient.Database("testDb1").CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	err = suite.srcMongoClient.Database("testDb1").CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.dstNamespaces)

	// Multiple DBs on source
	err = suite.srcMongoClient.Database("testDb2").CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	err = suite.srcMongoClient.Database("testDb2").CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.dstNamespaces)

	// Same namespaces on dest
	err = suite.dstMongoClient.Database("testDb1").CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb1").CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb2").CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb2").CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.dstNamespaces)

	// Additional namespaces on dest
	err = suite.dstMongoClient.Database("testDb3").CreateCollection(ctx, "testColl5")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb4").CreateCollection(ctx, "testColl6")
	suite.Require().NoError(err)

	if suite.GetTopology(suite.dstMongoClient) != util.TopologySharded {
		err = suite.dstMongoClient.Database("local").CreateCollection(ctx, "testColl7")
		suite.Require().NoError(err)
	}

	err = suite.dstMongoClient.Database("mongosync_reserved_for_internal_use").CreateCollection(ctx, "globalState")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("mongosync_reserved_for_verification_src_metadata").CreateCollection(ctx, "auditor")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("mongosync_reserved_for_verification_dst_metadata").CreateCollection(ctx, "auditor")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4",
		"testDb3.testColl5", "testDb4.testColl6"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4",
		"testDb3.testColl5", "testDb4.testColl6"},
		verifier.dstNamespaces)

	err = suite.srcMongoClient.Database("testDb2").Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb2").Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb3").Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb4").Drop(ctx)
	suite.Require().NoError(err)

	// Views should be found
	pipeline := bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}
	err = suite.srcMongoClient.Database("testDb1").CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb1").CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb1.testView1"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb1.testView1"}, verifier.dstNamespaces)

	// Collections in admin, config, and local should not be found
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		err = suite.srcMongoClient.Database("local").CreateCollection(ctx, "islocalSrc")
		suite.Require().NoError(err)
	}

	if suite.GetTopology(suite.dstMongoClient) != util.TopologySharded {
		err = suite.dstMongoClient.Database("local").CreateCollection(ctx, "islocalDest")
		suite.Require().NoError(err)
	}

	err = suite.srcMongoClient.Database("admin").CreateCollection(ctx, "isAdminSrc")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("admin").CreateCollection(ctx, "isAdminDest")
	suite.Require().NoError(err)
	err = suite.srcMongoClient.Database("config").CreateCollection(ctx, "isConfigSrc")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("config").CreateCollection(ctx, "isConfigDest")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb1.testView1"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb1.testView1"}, verifier.dstNamespaces)
}

func (suite *IntegrationTestSuite) TestVerificationStatus() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	metaColl := verifier.verificationDatabase().Collection(verificationTasksCollection)
	_, err := metaColl.InsertMany(ctx, []any{
		bson.M{
			"generation": 0,
			"status":     verificationTaskAdded,
			"type":       verificationTaskVerifyDocuments,
		},
		bson.M{
			"generation": 0,
			"status":     verificationTaskProcessing,
			"type":       verificationTaskVerifyDocuments,
		},
		bson.M{
			"generation": 0,
			"status":     verificationTaskFailed,
			"type":       verificationTaskVerifyDocuments,
		},
		bson.M{
			"generation": 0,
			"status":     verificationTaskMetadataMismatch,
			"type":       verificationTaskVerifyDocuments,
		},
		bson.M{
			"generation": 0,
			"status":     verificationTaskCompleted,
			"type":       verificationTaskVerifyDocuments,
		},
	})
	suite.Require().NoError(err)

	status, err := verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Equal(1, status.AddedTasks, "added tasks not equal")
	suite.Equal(1, status.ProcessingTasks, "processing tasks not equal")
	suite.Equal(1, status.FailedTasks, "failed tasks not equal")
	suite.Equal(1, status.MetadataMismatchTasks, "metadata mismatch tasks not equal")
	suite.Equal(1, status.CompletedTasks, "completed tasks not equal")
}

func (suite *IntegrationTestSuite) TestMetadataMismatchAndPartitioning() {
	ctx := suite.Context()

	srcColl := suite.srcMongoClient.Database(suite.DBNameForTest()).Collection("coll")
	dstColl := suite.dstMongoClient.Database(suite.DBNameForTest()).Collection("coll")

	verifier := suite.BuildVerifier()

	ns := srcColl.Database().Name() + "." + srcColl.Name()
	verifier.SetSrcNamespaces([]string{ns})
	verifier.SetDstNamespaces([]string{ns})
	verifier.SetNamespaceMap()

	for _, coll := range mslices.Of(srcColl, dstColl) {
		_, err := coll.InsertOne(ctx, bson.M{"_id": 1, "x": 42})
		suite.Require().NoError(err)
	}

	_, err := srcColl.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: bson.D{{"foo", 1}},
		},
	)
	suite.Require().NoError(err)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	sortedTaskTypes := mslices.Of(
		verificationTaskVerifyDocuments,
		verificationTaskVerifyCollection,
	)

	cursor, err := verifier.verificationTaskCollection().Aggregate(
		ctx,
		append(
			mongo.Pipeline{
				bson.D{{"$match", bson.D{{"generation", 0}}}},
			},
			testutil.SortByListAgg("type", sortedTaskTypes)...,
		),
	)
	suite.Require().NoError(err)

	var tasks []VerificationTask
	suite.Require().NoError(cursor.All(ctx, &tasks))

	suite.Require().Len(tasks, 2)
	suite.Require().Equal(verificationTaskVerifyDocuments, tasks[0].Type)
	suite.Require().Equal(verificationTaskCompleted, tasks[0].Status)
	suite.Require().Equal(verificationTaskVerifyCollection, tasks[1].Type)
	suite.Require().Equal(verificationTaskMetadataMismatch, tasks[1].Status)

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	cursor, err = verifier.verificationTaskCollection().Aggregate(
		ctx,
		append(
			mongo.Pipeline{
				bson.D{{"$match", bson.D{{"generation", 1}}}},
			},
			testutil.SortByListAgg("type", sortedTaskTypes)...,
		),
	)
	suite.Require().NoError(err)

	suite.Require().NoError(cursor.All(ctx, &tasks))

	suite.Require().Len(tasks, 1, "generation 1 should only have done 1 task")
	suite.Require().Equal(verificationTaskVerifyCollection, tasks[0].Type)
	suite.Require().Equal(verificationTaskMetadataMismatch, tasks[0].Status)
}

func (suite *IntegrationTestSuite) TestGenerationalRechecking() {
	dbname1 := suite.DBNameForTest("1")
	dbname2 := suite.DBNameForTest("2")

	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	defer zerolog.SetGlobalLevel(zerolog.DebugLevel)
	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbname1 + ".testColl1"})
	verifier.SetDstNamespaces([]string{dbname2 + ".testColl3"})
	verifier.SetNamespaceMap()

	ctx := suite.Context()

	srcColl := suite.srcMongoClient.Database(dbname1).Collection("testColl1")
	dstColl := suite.dstMongoClient.Database(dbname2).Collection("testColl3")
	_, err := srcColl.InsertOne(ctx, bson.M{"_id": 1, "x": 42})
	suite.Require().NoError(err)
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 2, "x": 43})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 1, "x": 42})
	suite.Require().NoError(err)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)

	waitForTasks := func() *VerificationStatus {
		status, err := verifier.GetVerificationStatus(ctx)
		suite.Require().NoError(err)

		for status.TotalTasks == 0 && verifier.generation < 50 {
			delay := time.Second

			suite.T().Logf("TotalTasks is 0 (generation=%d); waiting %s then will run another generation …", verifier.generation, delay)

			time.Sleep(delay)
			suite.Require().NoError(runner.StartNextGeneration())
			suite.Require().NoError(runner.AwaitGenerationEnd())
			status, err = verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)
		}
		return status
	}

	// wait for one generation to finish
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status := waitForTasks()
	suite.Require().Equal(VerificationStatus{TotalTasks: 2, FailedTasks: 1, CompletedTasks: 1}, *status)

	// now patch up the destination
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 2, "x": 43})
	suite.Require().NoError(err)

	// tell check to start the next generation
	suite.Require().NoError(runner.StartNextGeneration())

	// wait for generation to finish
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status = waitForTasks()
	// there should be no failures now, since they are equivalent at this point in time
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// The next generation should process the recheck task caused by inserting {_id: 2} on the destination.
	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status = waitForTasks()
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// now insert in the source, this should come up next generation
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 3, "x": 44})
	suite.Require().NoError(err)

	// tell check to start the next generation
	suite.Require().NoError(runner.StartNextGeneration())

	// wait for one generation to finish
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status = waitForTasks()

	// there should be a failure from the src insert
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, FailedTasks: 1}, *status)

	// now patch up the destination
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 3, "x": 44})
	suite.Require().NoError(err)

	// continue
	suite.Require().NoError(runner.StartNextGeneration())

	// wait for it to finish again, this should be a clean run
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status = waitForTasks()

	// there should be no failures now, since they are equivalent at this point in time
	suite.Assert().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// We could just abandon this verifier, but we might as well shut it down
	// gracefully. That prevents a spurious error in the log from “drop”
	// change events.
	suite.Require().NoError(verifier.WritesOff(ctx))
	suite.Require().NoError(runner.Await())
}

func (suite *IntegrationTestSuite) TestVerifierWithFilter() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	dbname1 := suite.DBNameForTest("1")
	dbname2 := suite.DBNameForTest("2")

	filter := bson.D{{"inFilter", bson.M{"$ne": false}}}
	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbname1 + ".testColl1"})
	verifier.SetDstNamespaces([]string{dbname2 + ".testColl3"})
	verifier.SetNamespaceMap()
	verifier.SetDocCompareMethod(DocCompareIgnoreOrder)
	// Set this value low to test the verifier with multiple partitions.
	verifier.partitionSizeInBytes = 50

	ctx := suite.Context()

	srcColl := suite.srcMongoClient.Database(dbname1).Collection("testColl1")
	dstColl := suite.dstMongoClient.Database(dbname2).Collection("testColl3")

	// Documents with _id in [0, 100) should match.
	var docs []any
	for i := 0; i < 100; i++ {
		docs = append(docs, bson.M{"_id": i, "x": i, "inFilter": true})
	}
	_, err := srcColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = dstColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)

	// Documents with _id in [100, 200) should be ignored because they're not in the filter.
	docs = []any{}
	for i := 100; i < 200; i++ {
		docs = append(docs, bson.M{"_id": i, "x": i, "inFilter": false})
	}
	_, err = srcColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)

	checkDoneChan := make(chan struct{})
	checkContinueChan := make(chan struct{})
	go func() {
		err := verifier.CheckDriver(ctx, filter, checkDoneChan, checkContinueChan)
		suite.Require().NoError(err)
	}()

	waitForTasks := func() *VerificationStatus {
		status, err := verifier.GetVerificationStatus(ctx)
		suite.Require().NoError(err)

		for status.TotalTasks == 0 && verifier.generation < 50 {
			delay := time.Second

			suite.T().Logf("TotalTasks is 0 (generation=%d); waiting %s then will run another generation …", verifier.generation, delay)

			time.Sleep(delay)
			checkContinueChan <- struct{}{}
			<-checkDoneChan
			status, err = verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)
		}
		return status
	}

	// Wait for one generation to finish.
	<-checkDoneChan
	status := waitForTasks()
	suite.Require().Greater(status.CompletedTasks, 1)
	suite.Require().Greater(status.TotalTasks, 1)
	suite.Require().Zero(status.FailedTasks, "there should be no failed tasks")

	// Insert another document that is not in the filter.
	// This should trigger a recheck despite being outside the filter.
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 200, "x": 200, "inFilter": false})
	suite.Require().NoError(err)

	// Tell check to start the next generation.
	checkContinueChan <- struct{}{}

	// Wait for generation to finish.
	<-checkDoneChan

	status = waitForTasks()

	// There should be no failures, since the inserted document is not in the filter.
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// Now insert in the source. This should come up next generation.
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 201, "x": 201, "inFilter": true})
	suite.Require().NoError(err)

	// Tell check to start the next generation.
	checkContinueChan <- struct{}{}

	// Wait for one generation to finish.
	<-checkDoneChan
	status = waitForTasks()

	// There should be a failure from the src insert of a document in the filter.
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, FailedTasks: 1}, *status)

	// Now patch up the destination.
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 201, "x": 201, "inFilter": true})
	suite.Require().NoError(err)

	// Continue.
	checkContinueChan <- struct{}{}

	// Wait for it to finish again, this should be a clean run.
	<-checkDoneChan
	status = waitForTasks()

	// There should be no failures now, since they are equivalent at this point in time.
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// Turn writes off.
	suite.Require().NoError(verifier.WritesOff(ctx))

	// Tell CheckDriver to do one more pass. This should terminate the change stream.
	checkContinueChan <- struct{}{}
	<-checkDoneChan
}

func (suite *IntegrationTestSuite) awaitEnqueueOfRechecks(verifier *Verifier, minDocs int) {
	var lastNonzeroRechecksCount int

	suite.Eventually(func() bool {
		cursor, err := verifier.getRecheckQueueCollection(verifier.generation).
			Find(suite.Context(), bson.D{})
		var rechecks []bson.D
		suite.Require().NoError(err)
		suite.Require().NoError(cursor.All(suite.Context(), &rechecks))

		if len(rechecks) >= minDocs {
			return true
		}

		if len(rechecks) > 0 {
			// Note any progress toward our minimum rechecks count.
			if lastNonzeroRechecksCount != len(rechecks) {
				suite.T().Logf(
					"%d recheck(s) are enqueued, but we want %d.",
					len(rechecks),
					minDocs,
				)

				lastNonzeroRechecksCount = len(rechecks)
			}

			return false
		}

		return false
	}, 1*time.Minute, 100*time.Millisecond)
}

func (suite *IntegrationTestSuite) TestChangesOnDstBeforeSrc() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	defer zerolog.SetGlobalLevel(zerolog.DebugLevel)

	ctx := suite.Context()

	collName := "mycoll"

	srcDB := suite.srcMongoClient.Database(suite.DBNameForTest())
	dstDB := suite.dstMongoClient.Database(suite.DBNameForTest())
	suite.Require().NoError(srcDB.CreateCollection(ctx, collName))
	suite.Require().NoError(dstDB.CreateCollection(ctx, collName))

	verifier := suite.BuildVerifier()
	runner := RunVerifierCheck(ctx, suite.T(), verifier)

	// Dry run generation 0 to make sure change stream reader is started.
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// Insert two documents in generation 1. They should be batched and become a verify task in generation 2.
	suite.Require().NoError(runner.StartNextGeneration())
	_, err := dstDB.Collection(collName).InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = dstDB.Collection(collName).InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)
	suite.Require().NoError(runner.AwaitGenerationEnd())
	suite.awaitEnqueueOfRechecks(verifier, 2)

	// Run generation 2 and get verification status.
	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())
	status, err := verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Assert().Equal(
		1,
		status.FailedTasks,
	)

	// Patch up only one of the two mismatched documents in generation 3.
	suite.Require().NoError(runner.StartNextGeneration())
	_, err = srcDB.Collection(collName).InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	suite.Require().NoError(runner.AwaitGenerationEnd())
	suite.awaitEnqueueOfRechecks(verifier, 1)

	status, err = verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Assert().Equal(
		1,
		status.FailedTasks,
		"failed tasks as expected (status=%+v)",
		status,
	)

	// Patch up the other mismatched document in generation 4.
	suite.Require().NoError(runner.StartNextGeneration())
	_, err = srcDB.Collection(collName).InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)
	suite.Require().NoError(runner.AwaitGenerationEnd())
	suite.awaitEnqueueOfRechecks(verifier, 1)

	// Everything should match by the end of it.
	status, err = verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Assert().Zero(status.FailedTasks)
}

func (suite *IntegrationTestSuite) TestBackgroundInIndexSpec() {
	ctx := suite.Context()

	srcDB := suite.srcMongoClient.Database(suite.DBNameForTest())
	dstDB := suite.dstMongoClient.Database(suite.DBNameForTest())

	suite.Require().NoError(
		srcDB.RunCommand(
			ctx,
			bson.D{
				{"createIndexes", "mycoll"},
				{"indexes", []bson.D{
					{
						{"name", "index1"},
						{"key", bson.D{{"someField", 1}}},
					},
				}},
			},
		).Err(),
	)

	suite.Require().NoError(
		dstDB.RunCommand(
			ctx,
			bson.D{
				{"createIndexes", "mycoll"},
				{"indexes", []bson.D{
					{
						{"name", "index1"},
						{"key", bson.D{{"someField", 1}}},
						{"background", 1},
					},
				}},
			},
		).Err(),
	)

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{srcDB.Name() + ".mycoll"})
	verifier.SetDstNamespaces([]string{dstDB.Name() + ".mycoll"})
	verifier.SetNamespaceMap()

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	status, err := verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)
	suite.Assert().Zero(
		status.MetadataMismatchTasks,
		"no metadata mismatch",
	)
}

func (suite *IntegrationTestSuite) TestPartitionWithFilter() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	dbname := suite.DBNameForTest()

	ctx := suite.Context()

	// Make a filter that filters on field "n".
	filter := bson.D{
		{"$expr", map[string]any{
			"$and": []map[string]any{
				{"$gte": []any{"$n", 100}},
				{"$lt": []any{"$n", 200}},
			},
		}},
	}

	// Set up the verifier for testing.
	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbname + ".testColl1"})
	verifier.SetDstNamespaces([]string{dbname + ".testColl1"})
	verifier.SetNamespaceMap()
	verifier.globalFilter = filter
	// Use a small partition size so that we can test creating multiple partitions.
	verifier.partitionSizeInBytes = 30

	// Insert documents into the source.
	srcColl := suite.srcMongoClient.Database(dbname).Collection("testColl1")

	// 30 documents with _ids [0, 30) are in the filter.
	for i := 0; i < 30; i++ {
		_, err := srcColl.InsertOne(ctx, bson.M{"_id": i, "n": rand.Intn(100) + 100})
		suite.Require().NoError(err)
	}

	// 100 documents with _ids [30, 130) are out of the filter.
	for i := 30; i < 130; i++ {
		_, err := srcColl.InsertOne(ctx, bson.M{"_id": i, "n": rand.Intn(100)})
		suite.Require().NoError(err)
	}

	// Create partitions with the filter.
	partitions, _, _, _, err := verifier.partitionAndInspectNamespace(ctx, dbname+".testColl1")
	suite.Require().NoError(err)

	// Check that each partition have bounds in the filter.
	for _, partition := range partitions {
		if _, isMinKey := partition.Key.Lower.(bson.MinKey); !isMinKey {
			suite.Require().GreaterOrEqual(partition.Key.Lower.(bson.RawValue).AsInt64(), int64(0))
		}
		if _, isMaxKey := partition.Upper.(bson.MaxKey); !isMaxKey {
			suite.Require().Less(partition.Upper.(bson.RawValue).AsInt64(), int64(30))
		}
	}
}
