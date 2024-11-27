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
	"os"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/cespare/permute/v2"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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

		envVals[name] = connStr
	}

	testSuite := &IntegrationTestSuite{
		srcConnStr:  envVals["MVTEST_SRC"],
		dstConnStr:  envVals["MVTEST_DST"],
		metaConnStr: envVals["MVTEST_META"],
	}

	suite.Run(t, testSuite)
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
	task := &VerificationTask{Ids: []any{id, id + 1}, QueryFilter: basicQueryFilter("keyhole.dealers")}

	// Test fetchDocuments without global filter.
	verifier.globalFilter = nil
	results, docCount, byteCount, err := verifier.FetchAndCompareDocuments(ctx, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(2, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs’ size")
	suite.Assert().Len(results, 2)
	suite.Assert().Equal(
		[]any{Mismatch, Mismatch},
		lo.Map(
			results,
			func(result VerificationResult, _ int) any {
				return result.Details
			},
		),
		"details as expected",
	)

	// Test fetchDocuments for ids with a global filter.

	verifier.globalFilter = map[string]any{"num": map[string]any{"$lt": 100}}
	results, docCount, byteCount, err = verifier.FetchAndCompareDocuments(ctx, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(1, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs’ size")
	suite.Require().Len(results, 1)
	suite.Assert().Equal(Mismatch, results[0].Details)
	suite.Assert().EqualValues(
		any(id),
		results[0].ID.(bson.RawValue).AsInt64(),
		"mismatch recorded as expeceted",
	)

	// Test fetchDocuments for a partition with a global filter.
	task.QueryFilter.Partition = &partitions.Partition{
		Ns: &partitions.Namespace{DB: "keyhole", Coll: "dealers"},
	}
	verifier.globalFilter = map[string]any{"num": map[string]any{"$lt": 100}}
	results, docCount, byteCount, err = verifier.FetchAndCompareDocuments(ctx, task)
	suite.Require().NoError(err)
	suite.Assert().EqualValues(1, docCount, "should find source docs")
	suite.Assert().NotZero(byteCount, "should tally docs’ size")
	suite.Require().Len(results, 1)
	suite.Assert().Equal(Mismatch, results[0].Details)
	suite.Assert().EqualValues(
		any(id),
		results[0].ID.(bson.RawValue).AsInt64(),
		"mismatch recorded as expeceted",
	)
}

func (suite *IntegrationTestSuite) TestGetNamespaceStatistics_Recheck() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	err := verifier.HandleChangeStreamEvents(
		ctx,
		[]ParsedEvent{{
			OpType: "insert",
			Ns:     &Namespace{DB: "mydb", Coll: "coll2"},
			DocKey: DocKey{
				ID: "heyhey",
			},
		}},
	)
	suite.Require().NoError(err)

	err = verifier.HandleChangeStreamEvents(
		ctx,
		[]ParsedEvent{{
			ID: bson.M{
				"docID": "ID/docID",
			},
			OpType: "insert",
			Ns:     &Namespace{DB: "mydb", Coll: "coll1"},
			DocKey: DocKey{
				ID: "hoohoo",
			},
		}},
	)
	suite.Require().NoError(err)

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

func (suite *IntegrationTestSuite) TestGetNamespaceStatistics_Gen0() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	stats, err := verifier.GetNamespaceStatistics(ctx)
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

	err = verifier.UpdateVerificationTask(ctx, task2)
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(ctx, task1)
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
	err = verifier.UpdateVerificationTask(ctx, task1parts[0])
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

	err = verifier.UpdateVerificationTask(ctx, task2parts[0])
	suite.Require().NoError(err)

	err = verifier.UpdateVerificationTask(ctx, task2parts[1])
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

func (suite *IntegrationTestSuite) TestFailedVerificationTaskInsertions() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()
	err := verifier.InsertFailedCompareRecheckDocs(ctx, "foo.bar", []interface{}{42}, []int{100})
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs(ctx, "foo.bar", []interface{}{43, 44}, []int{100, 100})
	suite.Require().NoError(err)
	err = verifier.InsertFailedCompareRecheckDocs(ctx, "foo.bar2", []interface{}{42}, []int{100})
	suite.Require().NoError(err)
	event := ParsedEvent{
		DocKey: DocKey{ID: int32(55)},
		OpType: "delete",
		Ns: &Namespace{
			DB:   "foo",
			Coll: "bar2",
		},
	}

	err = verifier.HandleChangeStreamEvents(ctx, []ParsedEvent{event})
	suite.Require().NoError(err)
	event.OpType = "insert"
	err = verifier.HandleChangeStreamEvents(ctx, []ParsedEvent{event})
	suite.Require().NoError(err)
	event.OpType = "replace"
	err = verifier.HandleChangeStreamEvents(ctx, []ParsedEvent{event})
	suite.Require().NoError(err)
	event.OpType = "update"
	err = verifier.HandleChangeStreamEvents(ctx, []ParsedEvent{event})
	suite.Require().NoError(err)
	event.OpType = "flibbity"
	err = verifier.HandleChangeStreamEvents(ctx, []ParsedEvent{event})
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

	namespace := "testdb.testns"

	ctx := context.Background()

	makeDocChannel := func(docs []bson.D) <-chan bson.Raw {
		theChan := make(chan bson.Raw, len(docs))

		for _, doc := range docs {
			theChan <- testutil.MustMarshal(doc)
		}

		close(theChan)

		return theChan
	}

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

			dstPermute := permute.Slice(dstDocs)
			for dstPermute.Permute() {
				srcChannel := makeDocChannel(srcDocs)
				dstChannel := makeDocChannel(dstDocs)

				fauxTask := VerificationTask{
					QueryFilter: QueryFilter{
						Namespace: namespace,
						ShardKeys: indexFields,
					},
				}
				results, docCount, byteCount, err := verifier.compareDocsFromChannels(
					ctx,
					&fauxTask,
					srcChannel,
					dstChannel,
				)

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

func (suite *IntegrationTestSuite) TestVerifierCompareViews() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	err := suite.srcMongoClient.Database("testDb").CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "sameView", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.sameView",
			To:        "testDb.sameView"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskCompleted, task.Status)
	suite.Nil(task.FailedDocs)

	// Views must have the same underlying collection
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "wrongColl", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "wrongColl", "testColl2", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.wrongColl",
			To:        "testDb.wrongColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.viewOn")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, "testDb.wrongColl")
	}

	// Views must have the same underlying pipeline
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "wrongPipeline", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}, {"a", 0}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.wrongPipeline",
			To:        "testDb.wrongPipeline"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.pipeline")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, "testDb.wrongPipeline")
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
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.missingOptionsSrc",
			To:        "testDb.missingOptionsSrc"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterSource)
		suite.Equal(task.FailedDocs[0].Details, "Missing")
		suite.Equal(task.FailedDocs[0].NameSpace, "testDb.missingOptionsSrc")
	}

	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "missingOptionsDst", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.missingOptionsDst",
			To:        "testDb.missingOptionsDst"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].Details, "Missing")
		suite.Equal(task.FailedDocs[0].NameSpace, "testDb.missingOptionsDst")
	}

	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation1))
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateView(ctx, "differentOptions", "testColl1", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}, options.CreateView().SetCollation(&collation2))
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.differentOptions",
			To:        "testDb.differentOptions"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(task.FailedDocs[0].Field, "Options.collation")
		suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
		suite.Equal(task.FailedDocs[0].NameSpace, "testDb.differentOptions")
	}
}

func (suite *IntegrationTestSuite) TestVerifierCompareMetadata() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	// Collection exists only on source.
	err := suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.testColl")

	// Make sure "To" is respected.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testCollTo"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.testCollTo")

	// Collection exists only on dest.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "destOnlyColl")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.destOnlyColl",
			To:        "testDb.destOnlyColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterSource)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.destOnlyColl")

	// A view and a collection are different.
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "viewOnSrc", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "viewOnSrc")
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.viewOnSrc",
			To:        "testDb.viewOnSrc"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Field, "Type")
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.viewOnSrc")

	// Capped should not match uncapped
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst")
	suite.Require().NoError(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst", options.CreateCollection().SetCapped(true).SetSizeInBytes(1024*1024*100))
	suite.Require().NoError(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.cappedOnDst",
			To:        "testDb.cappedOnDst"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
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
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskCompleted, task.Status)

	// Neither collection exists success case
	task = &VerificationTask{
		Status: verificationTaskProcessing,
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
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl1",
			To:        "testDb.testColl1",
		},
	}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(srcIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[0].Cluster)
		suite.Equal("testDb.testColl1", task.FailedDocs[0].NameSpace)
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
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl2",
			To:        "testDb.testColl2"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal(dstIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterSource, task.FailedDocs[0].Cluster)
		suite.Equal("testDb.testColl2", task.FailedDocs[0].NameSpace)
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
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl3",
			To:        "testDb.testColl3"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(2, len(task.FailedDocs)) {
		sort.Slice(task.FailedDocs, func(i, j int) bool {
			return task.FailedDocs[i].ID.(string) < task.FailedDocs[j].ID.(string)
		})
		suite.Equal(dstIndexNames[1], task.FailedDocs[0].ID)
		suite.Equal(Missing, task.FailedDocs[0].Details)
		suite.Equal(ClusterSource, task.FailedDocs[0].Cluster)
		suite.Equal("testDb.testColl3", task.FailedDocs[0].NameSpace)
		suite.Equal(srcIndexNames[0], task.FailedDocs[1].ID)
		suite.Equal(Missing, task.FailedDocs[1].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[1].Cluster)
		suite.Equal("testDb.testColl3", task.FailedDocs[1].NameSpace)
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
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl4",
			To:        "testDb.testColl4"}}
	suite.Require().NoError(
		verifier.verifyMetadataAndPartitionCollection(ctx, 1, task),
	)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal("wrong", task.FailedDocs[0].ID)
		suite.Regexp(regexp.MustCompile("^"+Mismatch), task.FailedDocs[0].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[0].Cluster)
		suite.Equal("testDb.testColl4", task.FailedDocs[0].NameSpace)
	}
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

	if suite.GetSrcTopology() != TopologySharded {
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
	if suite.GetSrcTopology() != TopologySharded {
		err = suite.srcMongoClient.Database("local").CreateCollection(ctx, "islocalSrc")
		suite.Require().NoError(err)
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
	_, err := metaColl.InsertMany(ctx, []interface{}{
		bson.M{"generation": 0, "status": "added", "type": "verify"},
		bson.M{"generation": 0, "status": "processing", "type": "verify"},
		bson.M{"generation": 0, "status": "failed", "type": "verify"},
		bson.M{"generation": 0, "status": "mismatch", "type": "verify"},
		bson.M{"generation": 0, "status": "completed", "type": "verify"},
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

	cursor, err := verifier.verificationTaskCollection().Find(
		ctx,
		bson.M{"generation": 0},
		options.Find().SetSort(bson.M{"type": 1}),
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

	cursor, err = verifier.verificationTaskCollection().Find(
		ctx,
		bson.M{"generation": 1},
		options.Find().SetSort(bson.M{"type": 1}),
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

	zerolog.SetGlobalLevel(zerolog.DebugLevel)
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
	// there should be no failures now, since they are are equivalent at this point in time
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

	// there should be no failures now, since they are are equivalent at this point in time
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

	filter := bson.M{"inFilter": bson.M{"$ne": false}}
	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbname1 + ".testColl1"})
	verifier.SetDstNamespaces([]string{dbname2 + ".testColl3"})
	verifier.SetNamespaceMap()
	verifier.SetIgnoreBSONFieldOrder(true)
	// Set this value low to test the verifier with multiple partitions.
	verifier.partitionSizeInBytes = 50

	ctx := suite.Context()

	srcColl := suite.srcMongoClient.Database(dbname1).Collection("testColl1")
	dstColl := suite.dstMongoClient.Database(dbname2).Collection("testColl3")

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
	suite.Require().Equal(status.FailedTasks, 0)

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
	filter := map[string]any{"$expr": map[string]any{"$and": []map[string]any{
		{"$gte": []any{"$n", 100}}, {"$lt": []any{"$n", 200}}}}}

	// Set up the verifier for testing.
	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbname + ".testColl1"})
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
		if _, isMinKey := partition.Key.Lower.(primitive.MinKey); !isMinKey {
			suite.Require().GreaterOrEqual(partition.Key.Lower.(bson.RawValue).AsInt64(), int64(0))
		}
		if _, isMaxKey := partition.Upper.(primitive.MaxKey); !isMaxKey {
			suite.Require().Less(partition.Upper.(bson.RawValue).AsInt64(), int64(30))
		}
	}
}
