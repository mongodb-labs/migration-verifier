package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"testing"

	"github.com/10gen/migration-verifier/internal/documentmap"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/cespare/permute/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var macArmMongoVersions []string = []string{
	"6.2.0", "6.0.1",
}

var preMacArmMongoVersions []string = []string{
	"5.3.2", "5.0.11",
	"4.4.16", "4.2.22",
}

type MultiDataVersionTestSuite struct {
	WithMongodsTestSuite
}

type MultiSourceVersionTestSuite struct {
	WithMongodsTestSuite
}

type MultiMetaVersionTestSuite struct {
	WithMongodsTestSuite
}

func buildVerifier(t *testing.T, srcMongoInstance MongoInstance, dstMongoInstance MongoInstance, metaMongoInstance MongoInstance) *Verifier {
	qfilter := QueryFilter{Namespace: "keyhole.dealers"}
	task := VerificationTask{QueryFilter: qfilter}

	verifier := NewVerifier(VerifierSettings{})
	//verifier.SetStartClean(true)
	verifier.SetNumWorkers(3)
	verifier.SetGenerationPauseDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(1000)
	require.Nil(t, verifier.SetMetaURI(context.Background(), "mongodb://localhost:"+metaMongoInstance.port))
	require.Nil(t, verifier.SetSrcURI(context.Background(), "mongodb://localhost:"+srcMongoInstance.port))
	require.Nil(t, verifier.SetDstURI(context.Background(), "mongodb://localhost:"+dstMongoInstance.port))
	verifier.SetLogger("stderr")
	verifier.SetMetaDBName("VERIFIER_META")
	require.Nil(t, verifier.verificationTaskCollection().Drop(context.Background()))
	require.Nil(t, verifier.refetchCollection().Drop(context.Background()))
	require.Nil(t, verifier.srcClientCollection(&task).Drop(context.Background()))
	require.Nil(t, verifier.dstClientCollection(&task).Drop(context.Background()))
	require.Nil(t, verifier.verificationDatabase().Collection(recheckQueue).Drop(context.Background()))
	require.Nil(t, verifier.AddMetaIndexes(context.Background()))
	return verifier
}

func getAllVersions(t *testing.T) []string {
	var versions []string
	versions = append(versions, macArmMongoVersions...)

	os, arch := getOSAndArchFromEnv(t)

	if os != "macos" || arch != "arm64" {
		versions = append(versions, preMacArmMongoVersions...)
	}

	return versions
}

func getLatestVersion() string {
	return macArmMongoVersions[0]
}

func TestVerifierMultiversion(t *testing.T) {
	testSuite := new(MultiDataVersionTestSuite)
	srcVersions := getAllVersions(t)
	destVersions := getAllVersions(t)
	metaVersions := []string{getLatestVersion()}
	runMultipleVersionTests(t, testSuite, srcVersions, destVersions, metaVersions)
}

func TestVerifierMultiSourceversion(t *testing.T) {
	testSuite := new(MultiSourceVersionTestSuite)
	srcVersions := getAllVersions(t)
	destVersions := []string{getLatestVersion()}
	metaVersions := []string{getLatestVersion()}
	runMultipleVersionTests(t, testSuite, srcVersions, destVersions, metaVersions)
}

func TestVerifierMultiMetaVersion(t *testing.T) {
	srcVersions := []string{getLatestVersion()}
	destVersions := []string{getLatestVersion()}
	metaVersions := getAllVersions(t)
	testSuite := new(MultiMetaVersionTestSuite)
	runMultipleVersionTests(t, testSuite, srcVersions, destVersions, metaVersions)
}

func runMultipleVersionTests(t *testing.T, testSuite WithMongodsTestingSuite,
	srcVersions, destVersions, metaVersions []string) {
	for _, srcVersion := range srcVersions {
		for _, destVersion := range destVersions {
			for _, metaVersion := range metaVersions {
				testName := srcVersion + "->" + destVersion + ":" + metaVersion
				t.Run(testName, func(t *testing.T) {
					// TODO: this should be able to be run in parallel but we run killall mongod in the start of each of these test cases
					// For now we are going to leave killall in because the tests don't take long and adding the killall makes them very safe
					// t.Parallel()
					testSuite.SetMetaInstance(MongoInstance{
						version: metaVersion,
					})
					testSuite.SetSrcInstance(MongoInstance{
						version: srcVersion,
					})
					testSuite.SetDstInstance(MongoInstance{
						version: destVersion,
					})
					suite.Run(t, testSuite)
				})
			}
		}
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierFetchDocuments() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()
	drop := func() {
		err := verifier.srcClient.Database("keyhole").Drop(ctx)
		suite.Require().NoError(err)
		err = verifier.dstClient.Database("keyhole").Drop(ctx)
		suite.Require().NoError(err)
	}
	drop()
	defer drop()

	expectOneCommonDoc := func(srcMap *documentmap.Map, dstMap *documentmap.Map) {
		onlySrc, onlyDst, both := srcMap.CompareToMap(dstMap)
		suite.Assert().Empty(onlySrc, "no source-only docs")
		suite.Assert().Empty(onlyDst, "no destination-only docs")
		suite.Assert().Equal(1, len(both), "common docs")
		suite.Assert().NotPanics(
			func() {
				doc := srcMap.Fetch(both[0])
				val := doc.Lookup("num")
				suite.Assert().Less(val.AsInt64(), int64(100))
			},
			"doc is fetched",
		)
	}

	expectTwoCommonDocs := func(srcMap *documentmap.Map, dstMap *documentmap.Map) {
		onlySrc, onlyDst, both := srcMap.CompareToMap(dstMap)
		suite.Assert().Empty(onlySrc, "no source-only docs")
		suite.Assert().Empty(onlyDst, "no destination-only docs")
		suite.Assert().Equal(2, len(both), "common docs")
		suite.Assert().NotPanics(
			func() { srcMap.Fetch(both[0]) },
			"doc is fetched",
		)
		suite.Assert().NotPanics(
			func() { dstMap.Fetch(both[1]) },
			"doc is fetched",
		)
	}

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
	task := &VerificationTask{Ids: []any{id, id + 1}, QueryFilter: basicQueryFilter("keyhole.dealers")}

	// Test fetchDocuments without global filter.
	verifier.globalFilter = nil
	srcDocumentMap, dstDocumentMap, err := verifier.fetchDocuments(task)
	suite.Require().NoError(err)
	expectTwoCommonDocs(srcDocumentMap, dstDocumentMap)

	// Test fetchDocuments for ids with a global filter.
	verifier.globalFilter = map[string]any{"num": map[string]any{"$lt": 100}}
	srcDocumentMap, dstDocumentMap, err = verifier.fetchDocuments(task)
	suite.Require().NoError(err)
	expectOneCommonDoc(srcDocumentMap, dstDocumentMap)

	// Test fetchDocuments for a partition with a global filter.
	task.QueryFilter.Partition = &partitions.Partition{
		Ns:       &partitions.Namespace{DB: "keyhole", Coll: "dealers"},
		IsCapped: false,
	}
	verifier.globalFilter = map[string]any{"num": map[string]any{"$lt": 100}}
	srcDocumentMap, dstDocumentMap, err = verifier.fetchDocuments(task)
	suite.Require().NoError(err)
	expectOneCommonDoc(srcDocumentMap, dstDocumentMap)
}

func (suite *MultiMetaVersionTestSuite) TestGetNamespaceStatistics_Recheck() {
	ctx := context.Background()
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	suite.Require().NoError(
		verifier.InsertChangeEventRecheckDoc(
			ctx,
			&ParsedEvent{
				OpType: "insert",
				Ns:     &Namespace{DB: "mydb", Coll: "coll2"},
				DocKey: DocKey{
					ID: "heyhey",
				},
			},
		),
	)

	suite.Require().NoError(
		verifier.InsertChangeEventRecheckDoc(
			ctx,
			&ParsedEvent{
				ID: bson.M{
					"docID": "ID/docID",
				},
				OpType: "insert",
				Ns:     &Namespace{DB: "mydb", Coll: "coll1"},
				DocKey: DocKey{
					ID: "hoohoo",
				},
			},
		),
	)

	verifier.generation++

	func() {
		verifier.mux.Lock()
		defer func() { verifier.mux.Unlock() }()
		suite.Require().NoError(verifier.GenerateRecheckTasks(ctx))
	}()

	stats, err := verifier.GetNamespaceStatistics(ctx)
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

func (suite *MultiMetaVersionTestSuite) TestGetNamespaceStatistics_Gen0() {
	ctx := context.Background()
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	stats, err := verifier.GetNamespaceStatistics(ctx)
	suite.Require().NoError(err)

	suite.Assert().Equal(
		[]NamespaceStats{},
		stats,
		"Stats are empty at first",
	)

	// Now add 2 namespaces. Add them “out of order” to test
	// that we sort the returned array by Namespace.

	task2, err := verifier.InsertCollectionVerificationTask("mydb.coll2")
	suite.Require().NoError(err)

	task1, err := verifier.InsertCollectionVerificationTask("mydb.coll1")
	suite.Require().NoError(err)

	stats, err = verifier.GetNamespaceStatistics(ctx)
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

	err = verifier.UpdateVerificationTask(task2)
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(task1)
	suite.Require().NoError(err)

	stats, err = verifier.GetNamespaceStatistics(ctx)
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
			&partitions.Partition{
				Ns: &partitions.Namespace{DB: "mydb", Coll: "coll1"},
			},
			[]string{},
			"faux.dstnamespace",
		)
		suite.Require().NoError(err)

		task1parts[i] = task1part

		task2part, err := verifier.InsertPartitionVerificationTask(
			&partitions.Partition{
				Ns: &partitions.Namespace{DB: "mydb", Coll: "coll2"},
			},
			[]string{},
			"faux.dstnamespace",
		)
		suite.Require().NoError(err)

		task2parts[i] = task2part
	}

	stats, err = verifier.GetNamespaceStatistics(ctx)
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
	err = verifier.UpdateVerificationTask(task1parts[0])
	suite.Require().NoError(err)

	stats, err = verifier.GetNamespaceStatistics(ctx)
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

	err = verifier.UpdateVerificationTask(task2parts[0])
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(task2parts[1])
	suite.Require().NoError(err)

	stats, err = verifier.GetNamespaceStatistics(ctx)
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

func (suite *MultiMetaVersionTestSuite) TestFailedVerificationTaskInsertions() {
	ctx := context.Background()
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	err := verifier.InsertFailedCompareRecheckDocs("foo.bar", []interface{}{42}, []int{100})
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs("foo.bar", []interface{}{43, 44}, []int{100, 100})
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs("foo.bar2", []interface{}{42}, []int{100})
	suite.Require().NoError(err)
	event := ParsedEvent{
		DocKey: DocKey{ID: int32(55)},
		OpType: "delete",
		Ns: &Namespace{
			DB:   "foo",
			Coll: "bar2",
		},
	}
	err = verifier.HandleChangeStreamEvent(ctx, &event)
	suite.Require().NoError(err)
	event.OpType = "insert"
	err = verifier.HandleChangeStreamEvent(ctx, &event)
	suite.Require().NoError(err)
	event.OpType = "replace"
	err = verifier.HandleChangeStreamEvent(ctx, &event)
	suite.Require().NoError(err)
	event.OpType = "update"
	err = verifier.HandleChangeStreamEvent(ctx, &event)
	suite.Require().NoError(err)
	event.OpType = "flibbity"
	err = verifier.HandleChangeStreamEvent(ctx, &event)
	badEventErr := UnknownEventError{}
	suite.Require().ErrorAs(err, &badEventErr)
	suite.Assert().Equal("flibbity", badEventErr.Event.OpType)

	verifier.generation++
	func() {
		verifier.mux.Lock()
		defer verifier.mux.Unlock()

		err = verifier.GenerateRecheckTasks(ctx)
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
		suite.Require().Equal("added", doc["status"])
		suite.Require().Equal("verify", doc["type"])
		suite.Require().Equal(expectedNamespace, doc["query_filter"].(bson.M)["namespace"])
	}
	verifyTask(bson.A{int32(42), int32(43), int32(44)}, "foo.bar")
	verifyTask(bson.A{int32(42), int32(55)}, "foo.bar2")
	suite.Require().False(cur.Next(ctx))
}

func makeDocMap(t *testing.T, docs []bson.D, indexFields ...string) *documentmap.Map {
	cursor := testutil.DocsToCursor(docs)

	dmap := documentmap.New(logger.NewDebugLogger(), indexFields...)
	err := dmap.ImportFromCursor(context.Background(), cursor)
	require.NoError(t, err)

	return dmap
}

func TestVerifierCompareDocs(t *testing.T) {
	id := rand.Intn(1000)
	verifier := NewVerifier(VerifierSettings{})
	verifier.SetIgnoreBSONFieldOrder(true)

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
					require.Nil(t, mismatchResults[0].ID.(bson.RawValue).Unmarshal(&res))
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
					require.Nil(t, mismatchResults[0].ID.(bson.RawValue).Unmarshal(&res))
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
				assert.Empty(t, mismatchedIds)
			},
		},
	}

	namespace := getDBName(t) + ".testns"

	for _, curTest := range compareTests {
		verifier.SetIgnoreBSONFieldOrder(!curTest.checkOrder)

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
			srcMap := makeDocMap(t, srcDocs, indexFields...)

			dstPermute := permute.Slice(dstDocs)
			for dstPermute.Permute() {
				dstMap := makeDocMap(t, dstDocs, indexFields...)
				mismatchedIds, err := verifier.compareDocuments(srcMap, dstMap, namespace)

				label := curTest.label
				if permuted {
					curPermutation++
					label += fmt.Sprintf(" permutation %d", curPermutation)
				}

				ok := t.Run(
					label,
					func(t *testing.T) {
						require.NoError(t, err)
						curTest.compareFn(t, mismatchedIds)
					},
				)

				if !ok {
					if len(curTest.srcDocs) > 0 {
						t.Logf("src: %v", curTest.srcDocs)
					}
					if len(curTest.dstDocs) > 0 {
						t.Logf("dst: %v", curTest.dstDocs)
					}
				}
			}
		}
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierCompareViews() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	db := getDBName(suite.T())

	err := suite.srcMongoClient.Database(db).CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".sameView",
			To:        db + ".sameView"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskCompleted, task.Status)
	suite.Nil(task.FailedDocs)

	// Views must have the same underlying collection
	err = suite.srcMongoClient.Database(db).CreateView(ctx, "wrongColl", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "wrongColl", "testColl2", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".wrongColl",
			To:        db + ".wrongColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.viewOn")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, db+".wrongColl")
	}

	// Views must have the same underlying pipeline
	err = suite.srcMongoClient.Database(db).CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}, {"a", 0}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".wrongPipeline",
			To:        db + ".wrongPipeline"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.pipeline")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, db+".wrongPipeline")
	}

	// Views must have the same underlying options
	var collation1, collation2 options.Collation
	collation1.Locale = "en_US"
	collation1.CaseLevel = true
	collation2.Locale = "fr"
	collation2.Backwards = true
	err = suite.srcMongoClient.Database(db).CreateView(ctx, "missingOptionsSrc", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "missingOptionsSrc", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation2))
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".missingOptionsSrc",
			To:        db + ".missingOptionsSrc"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterSource)
		suite.Equal(task.FailedDocs[0].Details, "Missing")
		suite.Equal(task.FailedDocs[0].NameSpace, db+".missingOptionsSrc")
	}

	err = suite.srcMongoClient.Database(db).CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".missingOptionsDst",
			To:        db + ".missingOptionsDst"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].Details, "Missing")
		suite.Equal(task.FailedDocs[0].NameSpace, db+".missingOptionsDst")
	}

	err = suite.srcMongoClient.Database(db).CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation2))
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".differentOptions",
			To:        db + ".differentOptions"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, db+".differentOptions")
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierCompareMetadata() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	db := getDBName(suite.T())

	// Collection exists only on source.
	err := suite.srcMongoClient.Database(db).CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl",
			To:        db + ".testColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, db+".testColl")

	// Make sure "To" is respected.
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl",
			To:        db + ".testCollTo"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, db+".testCollTo")

	// Collection exists only on dest.
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "destOnlyColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".destOnlyColl",
			To:        db + ".destOnlyColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterSource)
	suite.Equal(task.FailedDocs[0].NameSpace, db+".destOnlyColl")

	// A view and a collection are different.
	err = suite.srcMongoClient.Database(db).CreateView(ctx, "viewOnSrc", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "viewOnSrc")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".viewOnSrc",
			To:        db + ".viewOnSrc"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Field, "Type")
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, db+".viewOnSrc")

	// Capped should not match uncapped
	err = suite.srcMongoClient.Database(db).CreateCollection(ctx, "cappedOnDst")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "cappedOnDst", options.CreateCollection().SetCapped(true).SetSizeInBytes(1024*1024*100))
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".cappedOnDst",
			To:        db + ".cappedOnDst"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	// Capped and size should differ
	var wrongFields []string
	for _, result := range task.FailedDocs {
		field := result.Field.(string)
		suite.Require().NotNil(field)
		wrongFields = append(wrongFields, field)
	}
	suite.ElementsMatch([]string{"Options.capped", "Options.size"}, wrongFields)

	// Default success case
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl",
			To:        db + ".testColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskCompleted, task.Status)

	// Neither collection exists success case
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testCollDNE",
			To:        db + ".testCollDNE"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskCompleted, task.Status)
}

func (suite *MultiDataVersionTestSuite) TestVerifierCompareIndexes() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	db := getDBName(suite.T())

	// Missing index on destination.
	err := suite.srcMongoClient.Database(db).CreateCollection(ctx, "testColl1")
	srcColl := suite.srcMongoClient.Database(db).Collection("testColl1")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	dstColl := suite.dstMongoClient.Database(db).Collection("testColl1")
	suite.Require().NoError(err)
	srcIndexNames, err := srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	_, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl1",
			To:        db + ".testColl1",
		},
	}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(srcIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[0].Cluster)
		suite.Equal(db+".testColl1", task.FailedDocs[0].NameSpace)
	}

	// Missing index on source
	err = suite.srcMongoClient.Database(db).CreateCollection(ctx, "testColl2")
	srcColl = suite.srcMongoClient.Database(db).Collection("testColl2")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database(db).Collection("testColl2")
	suite.Require().NoError(err)
	_, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	dstIndexNames, err := dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl2",
			To:        db + ".testColl2"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(dstIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterSource, task.FailedDocs[0].Cluster)
		suite.Equal(db+".testColl2", task.FailedDocs[0].NameSpace)
	}

	// Different indexes on each
	err = suite.srcMongoClient.Database(db).CreateCollection(ctx, "testColl3")
	srcColl = suite.srcMongoClient.Database(db).Collection("testColl3")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database(db).Collection("testColl3")
	suite.Require().NoError(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl3",
			To:        db + ".testColl3"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(2, len(task.FailedDocs)) {
		sort.Slice(task.FailedDocs, func(i, j int) bool {
			return task.FailedDocs[i].ID.(string) < task.FailedDocs[j].ID.(string)
		})
		suite.Equal(dstIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterSource, task.FailedDocs[0].Cluster)
		suite.Equal(db+".testColl3", task.FailedDocs[0].NameSpace)
		suite.Equal(srcIndexNames[0], task.FailedDocs[1].ID)
		suite.Equal(Missing, task.FailedDocs[1].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[1].Cluster)
		suite.Equal(db+".testColl3", task.FailedDocs[1].NameSpace)
	}

	// Indexes with same names are different
	err = suite.srcMongoClient.Database(db).CreateCollection(ctx, "testColl4")
	srcColl = suite.srcMongoClient.Database(db).Collection("testColl4")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db).CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	dstColl = suite.dstMongoClient.Database(db).Collection("testColl4")
	suite.Require().NoError(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}, Options: options.Index().SetName("wrong")}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().NoError(err)
	suite.Require().Equal("wrong", srcIndexNames[0])
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}, Options: options.Index().SetName("wrong")}})
	suite.Require().NoError(err)
	suite.Require().Equal("wrong", dstIndexNames[1])
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: db + ".testColl4",
			To:        db + ".testColl4"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal("wrong", task.FailedDocs[0].ID)
		suite.Regexp(regexp.MustCompile("^"+Mismatch), task.FailedDocs[0].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[0].Cluster)
		suite.Equal(db+".testColl4", task.FailedDocs[0].NameSpace)
	}
}

func TestVerifierCompareIndexSpecs(t *testing.T) {
	db := getDBName(t)

	// Index specification
	keysDoc1 := bson.D{{"a", 1}, {"b", -1}}
	// We marshal the key document twice so they are physically separate memory.
	keysRaw1, err := bson.Marshal(keysDoc1)
	require.NoError(t, err)
	keysRaw2, err := bson.Marshal(keysDoc1)
	require.NoError(t, err)
	simpleIndexSpec1 := mongo.IndexSpecification{
		Name:         "testIndex",
		Namespace:    db + ".testIndex",
		KeysDocument: keysRaw1,
		Version:      1}

	simpleIndexSpec2 := mongo.IndexSpecification{
		Name:         "testIndex",
		Namespace:    db + ".testIndex",
		KeysDocument: keysRaw2,
		Version:      2}

	results := compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec2)
	assert.Nil(t, results)

	// Changing version should not be an issue
	simpleIndexSpec3 := simpleIndexSpec2
	simpleIndexSpec3.Version = 4
	results = compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec3)
	assert.Nil(t, results)

	// Changing the key spec order matters
	keysDoc3 := bson.D{{"b", -1}, {"a", 1}}
	keysRaw3, err := bson.Marshal(keysDoc3)
	require.NoError(t, err)
	simpleIndexSpec3 = simpleIndexSpec2
	simpleIndexSpec3.KeysDocument = keysRaw3
	results = compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "KeysDocument", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}

	// Shortening the key mattes
	keysDoc3 = bson.D{{"a", 1}}
	keysRaw3, err = bson.Marshal(keysDoc3)
	require.NoError(t, err)
	simpleIndexSpec3 = simpleIndexSpec2
	simpleIndexSpec3.KeysDocument = keysRaw3
	results = compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "KeysDocument", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}

	var expireAfterSeconds30, expireAfterSeconds0_1, expireAfterSeconds0_2 int32
	expireAfterSeconds30 = 30
	expireAfterSeconds0_1, expireAfterSeconds0_2 = 0, 0
	sparseTrue := true
	sparseFalse_1, sparseFalse_2 := false, false
	uniqueTrue := true
	uniqueFalse_1, uniqueFalse_2 := false, false
	clusteredTrue := true
	clusteredFalse_1, clusteredFalse_2 := false, false
	fullIndexSpec1 := mongo.IndexSpecification{
		Name:               "testIndex",
		Namespace:          db + ".testIndex",
		KeysDocument:       keysRaw1,
		Version:            1,
		ExpireAfterSeconds: &expireAfterSeconds0_1,
		Sparse:             &sparseFalse_1,
		Unique:             &uniqueFalse_1,
		Clustered:          &clusteredFalse_1}

	fullIndexSpec2 := mongo.IndexSpecification{
		Name:               "testIndex",
		Namespace:          db + ".testIndex",
		KeysDocument:       keysRaw2,
		Version:            2,
		ExpireAfterSeconds: &expireAfterSeconds0_2,
		Sparse:             &sparseFalse_2,
		Unique:             &uniqueFalse_2,
		Clustered:          &clusteredFalse_2}

	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec2)
	assert.Nil(t, results)

	// The full index spec should not equal the equivalent simple index spec.
	results = compareIndexSpecifications(&fullIndexSpec1, &simpleIndexSpec2)
	var diffFields []interface{}
	for _, result := range results {
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
		diffFields = append(diffFields, result.Field)
	}
	assert.ElementsMatch(t, []string{"Sparse", "Unique", "ExpireAfterSeconds", "Clustered"}, diffFields)

	fullIndexSpec3 := fullIndexSpec2
	fullIndexSpec3.ExpireAfterSeconds = &expireAfterSeconds30
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "ExpireAfterSeconds", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Sparse = &sparseTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "Sparse", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Unique = &uniqueTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "Unique", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Clustered = &clusteredTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, db+".testIndex", result.NameSpace)
		assert.Equal(t, "Clustered", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		assert.NotRegexp(t, regexp.MustCompile("0x"), result.Details)
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierNamespaceList() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	db1 := getDBName(suite.T(), "1")
	db2 := getDBName(suite.T(), "2")

	// Collections on source only
	err := suite.srcMongoClient.Database(db1).CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	err = suite.srcMongoClient.Database(db1).CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2"}, verifier.dstNamespaces)

	// Multiple DBs on source
	err = suite.srcMongoClient.Database(db2).CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	err = suite.srcMongoClient.Database(db2).CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4"},
		verifier.dstNamespaces)

	// Same namespaces on dest
	err = suite.dstMongoClient.Database(db1).CreateCollection(ctx, "testColl1")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db1).CreateCollection(ctx, "testColl2")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db2).CreateCollection(ctx, "testColl3")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db2).CreateCollection(ctx, "testColl4")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4"},
		verifier.dstNamespaces)

	db3 := getDBName(suite.T(), "3")
	db4 := getDBName(suite.T(), "4")

	// Additional namespaces on dest
	err = suite.dstMongoClient.Database(db3).CreateCollection(ctx, "testColl5")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db4).CreateCollection(ctx, "testColl6")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("local").CreateCollection(ctx, "testColl7")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("mongosync_reserved_for_internal_use").CreateCollection(ctx, "globalState")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("mongosync_reserved_for_verification_src_metadata").CreateCollection(ctx, "auditor")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("mongosync_reserved_for_verification_dst_metadata").CreateCollection(ctx, "auditor")
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4",
		db3 + ".testColl5", db4 + ".testColl6"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db2 + ".testColl3", db2 + ".testColl4",
		db3 + ".testColl5", db4 + ".testColl6"},
		verifier.dstNamespaces)

	err = suite.srcMongoClient.Database(db2).Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db2).Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db3).Drop(ctx)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db4).Drop(ctx)
	suite.Require().NoError(err)

	// Views should be found
	pipeline := bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}
	err = suite.srcMongoClient.Database(db1).CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database(db1).CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().NoError(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().NoError(err)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db1 + ".testView1"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db1 + ".testView1"}, verifier.dstNamespaces)

	// Collections in admin, config, and local should not be found
	err = suite.srcMongoClient.Database("local").CreateCollection(ctx, "islocalSrc")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("local").CreateCollection(ctx, "islocalDest")
	suite.Require().NoError(err)
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
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db1 + ".testView1"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{db1 + ".testColl1", db1 + ".testColl2", db1 + ".testView1"}, verifier.dstNamespaces)
}

func (suite *MultiDataVersionTestSuite) TestVerificationStatus() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	metaColl := verifier.verificationDatabase().Collection(verificationTasksCollection)
	_, err := metaColl.InsertMany(ctx, []interface{}{
		bson.M{"generation": 0, "status": "added", "type": "verify"},
		bson.M{"generation": 0, "status": "processing", "type": "verify"},
		bson.M{"generation": 0, "status": "failed", "type": "verify"},
		bson.M{"generation": 0, "status": "mismatch", "type": "verify"},
		bson.M{"generation": 0, "status": "completed", "type": "verify"},
	})
	suite.Require().NoError(err)

	status, err := verifier.GetVerificationStatus()
	suite.Require().NoError(err)
	suite.Equal(1, status.AddedTasks, "added tasks not equal")
	suite.Equal(1, status.ProcessingTasks, "processing tasks not equal")
	suite.Equal(1, status.FailedTasks, "failed tasks not equal")
	suite.Equal(1, status.MetadataMismatchTasks, "metadata mismatch tasks not equal")
	suite.Equal(1, status.CompletedTasks, "completed tasks not equal")
}

func (suite *MultiDataVersionTestSuite) TestGenerationalRechecking() {
	db1 := getDBName(suite.T(), "1")
	db2 := getDBName(suite.T(), "2")

	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	verifier.SetSrcNamespaces([]string{db1 + ".testColl1"})
	verifier.SetDstNamespaces([]string{db2 + ".testColl3"})
	verifier.SetNamespaceMap()

	ctx := context.Background()

	srcColl := suite.srcMongoClient.Database(db1).Collection("testColl1")
	dstColl := suite.dstMongoClient.Database(db2).Collection("testColl3")
	_, err := srcColl.InsertOne(ctx, bson.M{"_id": 1, "x": 42})
	suite.Require().NoError(err)
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 2, "x": 43})
	suite.Require().NoError(err)
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 1, "x": 42})
	suite.Require().NoError(err)

	checkDoneChan := make(chan struct{})
	checkContinueChan := make(chan struct{})
	go func() {
		err := verifier.CheckDriver(ctx, nil, checkDoneChan, checkContinueChan)
		suite.Require().NoError(err)
	}()

	waitForTasks := func() *VerificationStatus {
		status, err := verifier.GetVerificationStatus()
		suite.Require().NoError(err)

		for status.TotalTasks == 0 && verifier.generation < 10 {
			suite.T().Logf("TotalTasks is 0 (generation=%d); waiting another generation …", verifier.generation)
			checkContinueChan <- struct{}{}
			<-checkDoneChan
			status, err = verifier.GetVerificationStatus()
			suite.Require().NoError(err)
		}
		return status
	}

	// wait for one generation to finish
	<-checkDoneChan
	status := waitForTasks()
	suite.Require().Equal(VerificationStatus{TotalTasks: 2, FailedTasks: 1, CompletedTasks: 1}, *status)

	// now patch up the destination
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 2, "x": 43})
	suite.Require().NoError(err)

	// tell check to start the next generation
	checkContinueChan <- struct{}{}

	// wait for generation to finish
	<-checkDoneChan
	status = waitForTasks()
	// there should be no failures now, since they are are equivalent at this point in time
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// now insert in the source, this should come up next generation
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 3, "x": 44})
	suite.Require().NoError(err)

	// tell check to start the next generation
	checkContinueChan <- struct{}{}

	// wait for one generation to finish
	<-checkDoneChan
	status = waitForTasks()

	// there should be a failure from the src insert
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, FailedTasks: 1}, *status)

	// now patch up the destination
	_, err = dstColl.InsertOne(ctx, bson.M{"_id": 3, "x": 44})
	suite.Require().NoError(err)

	// continue
	checkContinueChan <- struct{}{}

	// wait for it to finish again, this should be a clean run
	<-checkDoneChan
	status = waitForTasks()

	// there should be no failures now, since they are are equivalent at this point in time
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// turn writes off
	verifier.WritesOff(ctx)
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 1019, "x": 1019})
	suite.Require().NoError(err)
	checkContinueChan <- struct{}{}
	<-checkDoneChan
	// now write to the source, this should not be seen by the change stream which should have ended
	// because of the calls to WritesOff
	status, err = verifier.GetVerificationStatus()
	suite.Require().NoError(err)
	// there should be a failure from the src insert
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, FailedTasks: 1}, *status)
}

func (suite *MultiDataVersionTestSuite) TestVerifierWithFilter() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	db1 := getDBName(suite.T(), "1")
	db2 := getDBName(suite.T(), "2")

	filter := map[string]any{"inFilter": map[string]any{"$ne": false}}
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	verifier.SetSrcNamespaces([]string{db1 + ".testColl1"})
	verifier.SetDstNamespaces([]string{db2 + ".testColl3"})
	verifier.SetNamespaceMap()
	verifier.SetIgnoreBSONFieldOrder(true)
	// Set this value low to test the verifier with multiple partitions.
	verifier.partitionSizeInBytes = 50

	ctx := context.Background()

	srcColl := suite.srcMongoClient.Database(db1).Collection("testColl1")
	dstColl := suite.dstMongoClient.Database(db2).Collection("testColl3")

	// Documents with _id in [0, 100) should match.
	var docs []interface{}
	for i := 0; i < 100; i++ {
		docs = append(docs, bson.M{"_id": i, "x": i, "inFilter": true})
	}
	_, err := srcColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)
	_, err = dstColl.InsertMany(ctx, docs)
	suite.Require().NoError(err)

	// Documents with _id in [100, 200) should be ignored because they're not in the filter.
	docs = []interface{}{}
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
		status, err := verifier.GetVerificationStatus()
		suite.Require().NoError(err)

		for status.TotalTasks == 0 && verifier.generation < 10 {
			suite.T().Logf("TotalTasks is 0 (generation=%d); waiting another generation …", verifier.generation)
			checkContinueChan <- struct{}{}
			<-checkDoneChan
			status, err = verifier.GetVerificationStatus()
			suite.Require().NoError(err)
		}
		return status
	}

	// Wait for one generation to finish.
	<-checkDoneChan
	status := waitForTasks()
	suite.Require().Greater(status.CompletedTasks, 1)
	suite.Require().Greater(status.TotalTasks, 1)
	suite.Require().Equal(status.FailedTasks, 0)

	// Insert another document that is not in the filter.
	_, err = srcColl.InsertOne(ctx, bson.M{"_id": 200, "x": 200, "inFilter": false})
	suite.Require().NoError(err)

	// Tell check to start the next generation.
	checkContinueChan <- struct{}{}

	// Wait for generation to finish.
	<-checkDoneChan
	status = waitForTasks()
	// There should be no failures, since the inserted document is not in the filter.
	suite.Require().Equal(VerificationStatus{TotalTasks: 1, CompletedTasks: 1}, *status)

	// Now insert in the source, this should come up next generation.
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
	verifier.WritesOff(ctx)

	// Tell CheckDriver to do one more pass. This should terminate the change stream.
	checkContinueChan <- struct{}{}
	<-checkDoneChan
}

func (suite *MultiDataVersionTestSuite) TestPartitionWithFilter() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	ctx := context.Background()

	// Make a filter that filters on field "n".
	filter := map[string]any{"$expr": map[string]any{"$and": []map[string]any{
		{"$gte": []any{"$n", 100}}, {"$lt": []any{"$n", 200}}}}}

	db1 := getDBName(suite.T(), "1")

	// Set up the verifier for testing.
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	verifier.SetSrcNamespaces([]string{db1 + ".testColl1"})
	verifier.SetNamespaceMap()
	verifier.globalFilter = filter
	// Use a small partition size so that we can test creating multiple partitions.
	verifier.partitionSizeInBytes = 30

	// Insert documents into the source.
	srcColl := suite.srcMongoClient.Database(db1).Collection("testColl1")

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
	partitions, _, _, _, err := verifier.partitionAndInspectNamespace(ctx, db1+".testColl1")
	suite.Require().NoError(err)

	// Check that each partition have bounds in the filter.
	for _, partition := range partitions {
		if _, isMinKey := partition.Key.Lower.(primitive.MinKey); !isMinKey {
			suite.Require().GreaterOrEqual(partition.Key.Lower.(bson.RawValue).AsInt64(), int64(0))
		}
		if _, isMaxKey := partition.Upper.(primitive.MaxKey); !isMaxKey {
			suite.Require().Less(partition.Upper.(bson.RawValue).AsInt64(), int64(30))
		}
	}
}
