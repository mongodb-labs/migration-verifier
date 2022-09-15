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
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MultiDataVersionTestSuite struct {
	WithMongodsTestSuite
}

type MultiMetaVersionTestSuite struct {
	WithMongodsTestSuite
}

func buildVerifier(t *testing.T, srcMongoInstance MongoInstance, dstMongoInstance MongoInstance, metaMongoInstance MongoInstance) *Verifier {
	qfilter := QueryFilter{Namespace: "keyhole.dealers"}
	task := VerificationTask{QueryFilter: qfilter}

	verifier := NewVerifier()
	verifier.SetNumWorkers(3)
	verifier.SetComparisonRetryDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(0)
	require.Nil(t, verifier.SetMetaURI(context.Background(), "mongodb://localhost:"+metaMongoInstance.port))
	require.Nil(t, verifier.SetSrcURI(context.Background(), "mongodb://localhost:"+srcMongoInstance.port))
	require.Nil(t, verifier.SetDstURI(context.Background(), "mongodb://localhost:"+dstMongoInstance.port))
	_, _, err := verifier.SetLogger("stderr")
	require.Nil(t, err)
	verifier.SetMetaDBName("VERIFIER_META")
	require.Nil(t, verifier.verificationTaskCollection().Drop(context.Background()))
	require.Nil(t, verifier.verificationRangeCollection().Drop(context.Background()))
	require.Nil(t, verifier.refetchCollection().Drop(context.Background()))
	require.Nil(t, verifier.srcClientCollection(&task).Drop(context.Background()))
	require.Nil(t, verifier.dstClientCollection(&task).Drop(context.Background()))
	return verifier
}

func TestVerifierMultiversion(t *testing.T) {
	testSuite := new(MultiDataVersionTestSuite)
	srcVersions := []string{"6.0.1", "5.3.2", "5.0.11", "4.4.16", "4.2.22"}
	destVersions := []string{"6.0.1", "5.3.2", "5.0.11", "4.4.16", "4.2.22"}
	metaVersions := []string{"6.0.1"}
	runMultipleVersionTests(t, testSuite, srcVersions, destVersions, metaVersions)
}

func TestVerifierMultiMetaVersion(t *testing.T) {
	srcVersions := []string{"6.0.1"}
	destVersions := []string{"6.0.1"}
	metaVersions := []string{"6.0.1", "5.3.2", "5.0.11", "4.4.16", "4.2.22"}
	testSuite := new(MultiMetaVersionTestSuite)
	runMultipleVersionTests(t, testSuite, srcVersions, destVersions, metaVersions)
}

func runMultipleVersionTests(t *testing.T, testSuite WithMongodsTestingSuite,
	srcVersions, destVersions, metaVersions []string) {
	portOffset := 27001
	testCnt := 0
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
						port:    strconv.Itoa(portOffset + (testCnt * 3)),
						dir:     "meta" + strconv.Itoa(testCnt),
					})
					testSuite.SetSrcInstance(MongoInstance{
						version: srcVersion,
						port:    strconv.Itoa(portOffset + (testCnt * 3) + 1),
						dir:     "source" + strconv.Itoa(testCnt),
					})
					testSuite.SetDstInstance(MongoInstance{
						version: destVersion,
						port:    strconv.Itoa(portOffset + (testCnt * 3) + 2),
						dir:     "dest" + strconv.Itoa(testCnt),
					})
					testCnt++
					suite.Run(t, testSuite)
				})
			}
		}
	}
}

func makeRawDoc(t *testing.T, doc interface{}) bson.Raw {
	raw, err := bson.Marshal(doc)
	require.Nil(t, err, "Unable to marshal test doc -- programming error in test")
	return raw
}

// Using assertions with the values in a call to reflection's MapKeys doesn't work.

func mapKeysAsInterface(myMap interface{}) (result []interface{}) {
	for _, key := range reflect.ValueOf(myMap).MapKeys() {

		result = append(result, key.Interface())
	}
	return
}

func (suite *MultiDataVersionTestSuite) TestVerifierFetchDocuments() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()
	drop := func() {
		err := verifier.srcClient.Database("keyhole").Drop(ctx)
		suite.Require().Nil(err)
		err = verifier.dstClient.Database("keyhole").Drop(ctx)
		suite.Require().Nil(err)
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
	_, err := verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.D{{"_id", id}, {"num", 123}, {"name", "srcTest"}})
	suite.Require().Nil(err)
	_, err = verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.D{{"_id", id}, {"num", 123}, {"name", "dstTest"}})
	suite.Require().Nil(err)
	task := &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	srcDocumentMap, dstDocumentMap, err := verifier.fetchDocuments(task)
	suite.Require().Nil(err)
	rawType, rawIdBytes, err := bson.MarshalValue(id)
	suite.Require().Nil(err)
	rawId := bson.RawValue{Type: rawType, Value: rawIdBytes}
	stringId := RawToString(rawId)

	suite.ElementsMatch(mapKeysAsInterface(srcDocumentMap), []interface{}{stringId})
	suite.ElementsMatch(mapKeysAsInterface(dstDocumentMap), []interface{}{stringId})
}

func (suite *MultiMetaVersionTestSuite) TestRecheckQueue() {
	ctx := context.Background()
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	err := verifier.InsertFailedCompareRecheckDoc("foo", "bar", 42)
	suite.Require().Nil(err)
	err = verifier.InsertFailedCompareRecheckDoc("foo", "bar", 43)
	suite.Require().Nil(err)
	err = verifier.InsertFailedCompareRecheckDoc("foo", "bar", 44)
	suite.Require().Nil(err)
	err = verifier.InsertFailedCompareRecheckDoc("foo", "bar2", 42)
	suite.Require().Nil(err)
	err = verifier.InsertFailedCompareRecheckDoc("foo", "bar", 44)
	suite.Require().Nil(err)
	event := ParsedEvent{
		DocKey: DocKey{ID: int32(55)},
		OpType: "delete",
		Ns: &Namespace{
			DB:   "foo",
			Coll: "bar2",
		},
	}
	err = verifier.HandleChangeStreamEvent(&event)
	suite.Require().Nil(err)
	event.OpType = "insert"
	err = verifier.HandleChangeStreamEvent(&event)
	suite.Require().Nil(err)
	event.OpType = "replace"
	err = verifier.HandleChangeStreamEvent(&event)
	suite.Require().Nil(err)
	event.OpType = "update"
	err = verifier.HandleChangeStreamEvent(&event)
	suite.Require().Nil(err)
	event.OpType = "flibbity"
	err = verifier.HandleChangeStreamEvent(&event)
	suite.Require().Equal(fmt.Errorf(`Not supporting: "flibbity" events`), err)

	cur, err := verifier.GetRecheckDocs(ctx)
	suite.Require().Nil(err)
	var recheck RecheckAggregate
	more := cur.Next(ctx)
	suite.Require().True(more)
	err = cur.Decode(&recheck)
	suite.Require().Nil(err)
	expected := RecheckAggregate{
		ID: Namespace{
			DB:   "foo",
			Coll: "bar",
		},
		Ids: []interface{}{int32(42), int32(43), int32(44)},
	}
	suite.Require().Equal(expected, recheck)

	more = cur.Next(ctx)
	suite.Require().True(more)
	err = cur.Decode(&recheck)
	suite.Require().Nil(err)
	expected = RecheckAggregate{
		ID: Namespace{
			DB:   "foo",
			Coll: "bar2",
		},
		Ids: []interface{}{int32(42), int32(55)},
	}
	suite.Require().Equal(expected, recheck)

	more = cur.Next(ctx)
	suite.Require().False(more)
}

func TestVerifierCompareDocs(t *testing.T) {
	id := rand.Intn(1000)
	verifier := NewVerifier()
	verifier.SetIgnoreBSONFieldOrder(true)

	srcRaw := makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	dstRaw := makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	namespace := "testdb.testns"
	mismatchedIds, err := verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	assert.Equal(t, 0, len(mismatchedIds))

	// Test different field orders
	srcRaw = makeRawDoc(t, bson.D{{"_id", id}, {"name", "foobar"}, {"num", 123}})
	dstRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})

	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	assert.Equal(t, 0, len(mismatchedIds))

	// Test mismatched document
	id = rand.Intn(1000)
	srcRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 1234}, {"name", "foobar"}})
	dstRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	var res int
	if assert.Equal(t, 1, len(mismatchedIds)) {
		require.Nil(t, mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res))
		assert.Equal(t, id, res)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), mismatchedIds[0].Details)
	}

	// // Test document missing on target
	id = rand.Intn(1000)
	srcRaw = makeRawDoc(t, bson.M{"_id": id, "num": 1234, "name": "foobar"})
	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{}, namespace)
	require.Nil(t, err)
	if assert.Equal(t, 1, len(mismatchedIds)) {
		require.Nil(t, mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res))
		assert.Equal(t, id, res)
		assert.Equal(t, mismatchedIds[0].Details, Missing)
		assert.Equal(t, mismatchedIds[0].Cluster, ClusterTarget)
	}

	// Test document missing on source
	id = rand.Intn(1000)
	dstRaw = makeRawDoc(t, bson.M{"_id": id, "num": 1234, "name": "foobar"})
	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	if assert.Equal(t, 1, len(mismatchedIds)) {
		assert.Equal(t, mismatchedIds[0].Details, Missing)
		assert.Equal(t, mismatchedIds[0].Cluster, ClusterSource)
	}
}

func TestVerifierCompareDocsOrdered(t *testing.T) {
	id := rand.Intn(1000)
	verifier := NewVerifier()
	verifier.SetIgnoreBSONFieldOrder(false)

	srcRaw := makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	dstRaw := makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	namespace := "testdb.testns"
	mismatchedIds, err := verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	assert.Equal(t, 0, len(mismatchedIds))

	// Test different field orders
	srcRaw = makeRawDoc(t, bson.D{{"_id", id}, {"name", "foobar"}, {"num", 123}})
	dstRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})

	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	if assert.Equal(t, 1, len(mismatchedIds)) {
		var res int
		require.Nil(t, mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res))
		assert.Equal(t, id, res)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), mismatchedIds[0].Details)
	}

	// Test mismatched document
	id = rand.Intn(1000)
	srcRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 1234}, {"name", "foobar"}})
	dstRaw = makeRawDoc(t, bson.D{{"_id", id}, {"num", 123}, {"name", "foobar"}})
	mismatchedIds, err = verifier.compareDocuments(map[interface{}]bson.Raw{id: srcRaw}, map[interface{}]bson.Raw{id: dstRaw}, namespace)
	require.Nil(t, err)
	var res int
	if assert.Equal(t, 1, len(mismatchedIds)) {
		require.Nil(t, mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res))
		assert.Equal(t, id, res)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), mismatchedIds[0].Details)
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierCompareMetadata() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	// Collection exists only on source.
	err := suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().Nil(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.testColl")

	// Make sure "To" is respected.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl")
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl",
			To:        "testDb.testCollTo"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.testCollTo")

	// Collection exists only on dest.
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "destOnlyColl")
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.destOnlyColl",
			To:        "testDb.destOnlyColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Details, Missing)
	suite.Equal(task.FailedDocs[0].Cluster, ClusterSource)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.destOnlyColl")

	// A view and a collection are different.
	err = suite.srcMongoClient.Database("testDb").CreateView(ctx, "viewOnSrc", "testColl", bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}})
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "viewOnSrc")
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.viewOnSrc",
			To:        "testDb.viewOnSrc"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskFailed, task.Status)
	suite.Equal(1, len(task.FailedDocs))
	suite.Equal(task.FailedDocs[0].Field, "Type")
	suite.Equal(task.FailedDocs[0].Cluster, ClusterTarget)
	suite.Equal(task.FailedDocs[0].NameSpace, "testDb.viewOnSrc")

	// Capped should not match uncapped
	err = suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "cappedOnDst", options.CreateCollection().SetCapped(true).SetSizeInBytes(1024*1024*100))
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.cappedOnDst",
			To:        "testDb.cappedOnDst"}}
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
			Namespace: "testDb.testColl",
			To:        "testDb.testColl"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskCompleted, task.Status)

	// Neither collection exists success case
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testCollDNE",
			To:        "testDb.testCollDNE"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskCompleted, task.Status)
}

func (suite *MultiDataVersionTestSuite) TestVerifierCompareIndexes() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	// Missing index on destination.
	err := suite.srcMongoClient.Database("testDb").CreateCollection(ctx, "testColl1")
	srcColl := suite.srcMongoClient.Database("testDb").Collection("testColl1")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl1")
	suite.Require().Nil(err)
	dstColl := suite.dstMongoClient.Database("testDb").Collection("testColl1")
	suite.Require().Nil(err)
	srcIndexNames, err := srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().Nil(err)
	_, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().Nil(err)
	task := &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl1",
			To:        "testDb.testColl1"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
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
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl2")
	suite.Require().Nil(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl2")
	suite.Require().Nil(err)
	_, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().Nil(err)
	dstIndexNames, err := dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl2",
			To:        "testDb.testColl2"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
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
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl3")
	suite.Require().Nil(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl3")
	suite.Require().Nil(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().Nil(err)
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}}})
	suite.Require().Nil(err)
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl3",
			To:        "testDb.testColl3"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
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
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb").CreateCollection(ctx, "testColl4")
	suite.Require().Nil(err)
	dstColl = suite.dstMongoClient.Database("testDb").Collection("testColl4")
	suite.Require().Nil(err)
	srcIndexNames, err = srcColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"z", 1}, {"q", -1}}, Options: options.Index().SetName("wrong")}, {Keys: bson.D{{"a", 1}, {"b", -1}}}})
	suite.Require().Nil(err)
	suite.Require().Equal("wrong", srcIndexNames[0])
	dstIndexNames, err = dstColl.Indexes().CreateMany(ctx, []mongo.IndexModel{{Keys: bson.D{{"a", 1}, {"b", -1}}}, {Keys: bson.D{{"x", 1}}, Options: options.Index().SetName("wrong")}})
	suite.Require().Nil(err)
	suite.Require().Equal("wrong", dstIndexNames[1])
	task = &VerificationTask{
		Status: verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: "testDb.testColl4",
			To:        "testDb.testColl4"}}
	verifier.verifyMetadataAndPartitionCollection(ctx, 1, task)
	suite.Equal(verificationTaskMetadataMismatch, task.Status)
	if suite.Equal(1, len(task.FailedDocs)) {
		suite.Equal("wrong", task.FailedDocs[0].ID)
		suite.Regexp(regexp.MustCompile("^"+Mismatch), task.FailedDocs[0].Details)
		suite.Equal(ClusterTarget, task.FailedDocs[0].Cluster)
		suite.Equal("testDb.testColl4", task.FailedDocs[0].NameSpace)
	}
}

func TestVerifierCompareIndexSpecs(t *testing.T) {
	// Index specification
	keysDoc1 := bson.D{{"a", 1}, {"b", -1}}
	// We marshal the key document twice so they are physically separate memory.
	keysRaw1, err := bson.Marshal(keysDoc1)
	require.Nil(t, err)
	keysRaw2, err := bson.Marshal(keysDoc1)
	require.Nil(t, err)
	simpleIndexSpec1 := mongo.IndexSpecification{
		Name:         "testIndex",
		Namespace:    "testDB.testIndex",
		KeysDocument: keysRaw1,
		Version:      1}

	simpleIndexSpec2 := mongo.IndexSpecification{
		Name:         "testIndex",
		Namespace:    "testDB.testIndex",
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
	require.Nil(t, err)
	simpleIndexSpec3 = simpleIndexSpec2
	simpleIndexSpec3.KeysDocument = keysRaw3
	results = compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "KeysDocument", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
	}

	// Shortening the key mattes
	keysDoc3 = bson.D{{"a", 1}}
	keysRaw3, err = bson.Marshal(keysDoc3)
	require.Nil(t, err)
	simpleIndexSpec3 = simpleIndexSpec2
	simpleIndexSpec3.KeysDocument = keysRaw3
	results = compareIndexSpecifications(&simpleIndexSpec1, &simpleIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "KeysDocument", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
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
		Namespace:          "testDB.testIndex",
		KeysDocument:       keysRaw1,
		Version:            1,
		ExpireAfterSeconds: &expireAfterSeconds0_1,
		Sparse:             &sparseFalse_1,
		Unique:             &uniqueFalse_1,
		Clustered:          &clusteredFalse_1}

	fullIndexSpec2 := mongo.IndexSpecification{
		Name:               "testIndex",
		Namespace:          "testDB.testIndex",
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
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
		diffFields = append(diffFields, result.Field)
	}
	assert.ElementsMatch(t, []string{"Sparse", "Unique", "ExpireAfterSeconds", "Clustered"}, diffFields)

	fullIndexSpec3 := fullIndexSpec2
	fullIndexSpec3.ExpireAfterSeconds = &expireAfterSeconds30
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "ExpireAfterSeconds", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Sparse = &sparseTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "Sparse", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Unique = &uniqueTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "Unique", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
	}

	fullIndexSpec3 = fullIndexSpec2
	fullIndexSpec3.Clustered = &clusteredTrue
	results = compareIndexSpecifications(&fullIndexSpec1, &fullIndexSpec3)
	if assert.Equalf(t, 1, len(results), "Actual mismatches: %+v", results) {
		result := results[0]
		assert.Equal(t, "testIndex", result.ID)
		assert.Equal(t, "testDB.testIndex", result.NameSpace)
		assert.Equal(t, "Clustered", result.Field)
		assert.Regexp(t, regexp.MustCompile("^"+Mismatch), result.Details)
	}
}

func (suite *MultiDataVersionTestSuite) TestVerifierNamespaceList() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	// Collections on source only
	err := suite.srcMongoClient.Database("testDb1").CreateCollection(ctx, "testColl1")
	suite.Require().Nil(err)
	err = suite.srcMongoClient.Database("testDb1").CreateCollection(ctx, "testColl2")
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.dstNamespaces)

	// Multiple DBs on source
	err = suite.srcMongoClient.Database("testDb2").CreateCollection(ctx, "testColl3")
	suite.Require().Nil(err)
	err = suite.srcMongoClient.Database("testDb2").CreateCollection(ctx, "testColl4")
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.dstNamespaces)

	// Same namespaces on dest
	err = suite.dstMongoClient.Database("testDb1").CreateCollection(ctx, "testColl1")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb1").CreateCollection(ctx, "testColl2")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb2").CreateCollection(ctx, "testColl3")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb2").CreateCollection(ctx, "testColl4")
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4"},
		verifier.dstNamespaces)

	// Additional namespaces on dest
	err = suite.dstMongoClient.Database("testDb3").CreateCollection(ctx, "testColl5")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb4").CreateCollection(ctx, "testColl6")
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4",
		"testDb3.testColl5", "testDb4.testColl6"},
		verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2", "testDb2.testColl3", "testDb2.testColl4",
		"testDb3.testColl5", "testDb4.testColl6"},
		verifier.dstNamespaces)

	err = suite.srcMongoClient.Database("testDb2").Drop(ctx)
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb2").Drop(ctx)
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb3").Drop(ctx)
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb4").Drop(ctx)
	suite.Require().Nil(err)

	// Views should not be found
	pipeline := bson.A{bson.D{{"$project", bson.D{{"_id", 1}}}}}
	err = suite.srcMongoClient.Database("testDb1").CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("testDb1").CreateView(ctx, "testView1", "testColl1", pipeline)
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.dstNamespaces)

	// Collections in admin, config, and local should not be found
	err = suite.srcMongoClient.Database("local").CreateCollection(ctx, "islocalSrc")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("local").CreateCollection(ctx, "islocalDest")
	suite.Require().Nil(err)
	err = suite.srcMongoClient.Database("admin").CreateCollection(ctx, "isAdminSrc")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("admin").CreateCollection(ctx, "isAdminDest")
	suite.Require().Nil(err)
	err = suite.srcMongoClient.Database("config").CreateCollection(ctx, "isConfigSrc")
	suite.Require().Nil(err)
	err = suite.dstMongoClient.Database("config").CreateCollection(ctx, "isConfigDest")
	suite.Require().Nil(err)
	err = verifier.setupAllNamespaceList(ctx)
	suite.Require().Nil(err)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.srcNamespaces)
	suite.ElementsMatch([]string{"testDb1.testColl1", "testDb1.testColl2"}, verifier.dstNamespaces)
}

// func getVerificationTasks(t *testing.T, verifier *Verifier) []VerificationTask {
// 	ctx := context.Background()
// 	cursor, err := verifier.verificationTaskCollection().Find(ctx, bson.D{})
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	tasks := []VerificationTask{}
// 	for cursor.Next(ctx) {
// 		var task VerificationTask
// 		err := cursor.Decode(&task)
// 		if err != nil {
// 			t.Fatal(err)
// 		}
// 		tasks = append(tasks, task)
// 	}
// 	return tasks
// }
//
// func countDocs(t *testing.T, col *mongo.Collection) int64 {
// 	count, err := col.CountDocuments(context.Background(), bson.D{})
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	return count
// }
//
//func TestVerifierAddAllTasks(t *testing.T) {
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//
//	// No tasks created yet
//	_, err := verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// Insert 2 full batches of documents, and one partial
//	batchSizes := []int{10, 10, 3}
//	coll := verifier.srcClient.Database("keyhole").Collection("dealers")
//	for _, batchSize := range batchSizes {
//		var docs []interface{}
//		for i := 0; i < batchSize; i++ {
//			docs = append(docs, bson.M{"serial": i, "test": true, "name": "foo"})
//		}
//		coll.InsertMany(ctx, docs)
//	}
//
//	// Create batches for verification
//	//err = verifier.CreateAllVerifyTasks()
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//
//	batchesCreated := countDocs(t, verifier.verificationTaskCollection())
//	assert.Equal(t, int(batchesCreated), 3)
//
//	rangesCreated := countDocs(t, verifier.verificationRangeCollection())
//	assert.Equal(t, int(rangesCreated), 3)
//
//	allDocs := map[interface{}]interface{}{}
//	batches := 0
//	for {
//		nextTask, err := verifier.FindNextVerifyTaskAndUpdate()
//		if err != nil {
//			break
//		}
//		documents, err := getDocuments(verifier.srcClientCollection(nextTask), nextTask)
//		if err != nil {
//			t.Fatal(err)
//		}
//		for docID, doc := range documents {
//			allDocs[docID] = doc
//		}
//		batches++
//	}
//	assert.Equal(t, len(allDocs), 23)
//	assert.Equal(t, batches, 3)
//
//	// drop the task collection and rebuild tasks, this time using the existing ranges
//	verifier.verificationTaskCollection().Drop(ctx)
//	_, err = verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// Ensure range creation covers all docs
//	for {
//		nextTask, err := verifier.FindNextVerifyTaskAndUpdate()
//		if err != nil {
//			break
//		}
//		documents, err := getDocuments(verifier.srcClientCollection(nextTask), nextTask)
//		if err != nil {
//			t.Fatal(err)
//		}
//		for docID, doc := range documents {
//			allDocs[docID] = doc
//		}
//		batches++
//	}
//	assert.Equal(t, len(allDocs), 23)
//	assert.Equal(t, batches, 3)
//
//	// drop the task collection
//	verifier.verificationTaskCollection().Drop(ctx)
//	_, err = verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// delete one of our ranges to ensure that tasks are build from ranges rather than
//	// from scratch
//	_, err = verifier.verificationRangeCollection().DeleteOne(ctx, bson.D{})
//	if err != nil {
//		t.Fatal(err)
//	}
//
//	//err = verifier.CreateAllVerifyTasks()
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//
//	batchesCreated = countDocs(t, verifier.verificationTaskCollection())
//	assert.Equal(t, int(batchesCreated), 2)
//}

//
//func TestVerifierMultipleMachines(t *testing.T) {
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//
//	// No tasks created yet
//	_, err := verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// Insert 2 full batches of documents, and one partial
//	batchSizes := []int{10, 10, 3}
//	coll := verifier.srcClient.Database("keyhole").Collection("dealers")
//	for _, batchSize := range batchSizes {
//		var docs []interface{}
//		for i := 0; i < batchSize; i++ {
//			docs = append(docs, bson.M{"serial": i, "test": true, "name": "foo"})
//		}
//		coll.InsertMany(ctx, docs)
//	}
//
//	// Run the initial verifier
//	if err = verifier.Verify(); err != nil {
//		t.Fatal(err)
//	}
//
//	status, err := verifier.GetVerificationStatus()
//	if err != nil {
//		t.Fatal(err)
//	}
//	assert.Equal(t, status.totalTasks, 4)
//
//	// Run another verifier which will see existing verifier and not create new jobs
//	if err = verifier.Verify(); err != nil {
//		t.Fatal(err)
//	}
//	status, err = verifier.GetVerificationStatus()
//	if err != nil {
//		t.Fatal(err)
//	}
//	assert.Equal(t, status.totalTasks, 4)
//}
//
//func TestVerifierAllMatch(t *testing.T) {
//	var err error
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//	task := &VerificationTask{}
//	batchSizes := []int{10, 10, 3}
//	sourceColl := verifier.srcClientCollection(task)
//	targetColl := verifier.dstClientCollection(task)
//	for _, batchSize := range batchSizes {
//		var docs []interface{}
//		for i := 0; i < batchSize; i++ {
//			id := primitive.NewObjectID()
//			docs = append(docs, bson.M{"_id": id, "serial": i, "test": true, "name": "foo"})
//		}
//		_, err := sourceColl.InsertMany(ctx, docs)
//		if err != nil {
//			t.Fatal(err)
//		}
//		_, err = targetColl.InsertMany(ctx, docs)
//		if err != nil {
//			t.Fatal(err)
//		}
//	}
//
//	if err = verifier.Verify(); err != nil {
//		t.Fatal(err)
//	}
//
//	tasks := []VerificationTask{}
//	result, err := verifier.verificationTaskCollection().Find(ctx, bson.D{})
//	if err != nil {
//		t.Fatal(err)
//	}
//	err = result.All(ctx, &tasks)
//	if err != nil {
//		t.Fatal(err)
//	}
//
//	for _, task := range tasks {
//		assert.Equal(t, task.Status, "completed")
//	}
//}
//
//func TestVerifierMismatch(t *testing.T) {
//	var err error
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//	task := &VerificationTask{}
//	batchSizes := []int{10}
//	sourceColl := verifier.srcClientCollection(task)
//	targetColl := verifier.dstClientCollection(task)
//	mismatches := []primitive.ObjectID{}
//	for _, batchSize := range batchSizes {
//		var docs []interface{}
//		var id primitive.ObjectID
//		for i := 0; i < batchSize; i++ {
//			id = primitive.NewObjectID()
//			docs = append(docs, bson.M{"_id": id, "serial": i, "test": true, "name": "foo"})
//		}
//		_, err := sourceColl.InsertMany(ctx, docs)
//		if err != nil {
//			t.Fatal(err)
//		}
//
//		// Change contents of the last doc before inserting to target cluster
//		mismatches = append(mismatches, id)
//		docs[len(docs)-1] = bson.M{"_id": id, "test": "false", "name": "foo"}
//		_, err = targetColl.InsertMany(ctx, docs)
//		if err != nil {
//			t.Fatal(err)
//		}
//	}
//
//	if err = verifier.Verify(); err != nil {
//		t.Fatal(err)
//	}
//
//	tasks := []VerificationTask{}
//	result, err := verifier.verificationTaskCollection().Find(ctx, bson.M{"type": verificationTaskVerify})
//	if err != nil {
//		t.Fatal(err)
//	}
//	err = result.All(ctx, &tasks)
//	if err != nil {
//		t.Fatal(err)
//	}
//
//	for _, task := range tasks {
//		assert.Equal(t, task.Status, verificationTaskFailed)
//	}
//}
//
//func TestVerifierRetries(t *testing.T) {
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//
//	goodId := primitive.NewObjectID()
//	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": goodId, "name": "foo"})
//	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": goodId, "name": "foo"})
//	badId := primitive.NewObjectID()
//	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": badId, "name": "bar"})
//	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": badId, "name": "baz"})
//
//	//firstId, err := primitive.ObjectIDFromHex("000000000000000000000000")
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//	//lastId, err := primitive.ObjectIDFromHex("FFFFFFFFFFFFFFFFFFFFFFFF")
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//	//_, err = verifier.InsertVerifyTask(firstId, lastId)
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//	for i := 0; i < verificationTaskMaxRetries; i++ {
//		task, err := verifier.FindNextVerifyTaskAndUpdate()
//		if err != nil {
//			t.Fatal(err)
//		}
//		verifier.ProcessVerifyTask(0, task)
//
//		if i == verificationTaskMaxRetries-1 {
//			assert.Equal(t, task.Status, verificationTaskFailed)
//		} else {
//			assert.Equal(t, task.Status, verificationTasksRetry)
//		}
//		assert.Equal(t, len(task.FailedIDs), 1)
//		assert.Equal(t, task.FailedIDs[0], badId)
//		assert.Equal(t, task.Attempts, i+1)
//	}
//}
//
//func TestVerifierRefetchQueue(t *testing.T) {
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//
//	badId := primitive.NewObjectID()
//	//verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": badId, "name": "bar"})
//	//verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": badId, "name": "baz"})
//	//
//	//_, err := verifier.InsertVerifyTask(badId, badId)
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//	for i := 0; i < verificationTaskMaxRetries; i++ {
//		task, err := verifier.FindNextVerifyTaskAndUpdate()
//		if err != nil {
//			t.Fatal(err)
//		}
//		verifier.ProcessVerifyTask(0, task)
//
//		cur, err := verifier.refetchCollection().Find(ctx, bson.M{"status": Unprocessed})
//		if err != nil {
//			t.Fatal(err)
//		}
//		var docs []Refetch
//		err = cur.All(ctx, &docs)
//		if err != nil {
//			t.Fatal(err)
//		}
//
//		if i == verificationTaskMaxRetries-1 {
//			assert.Equal(t, task.Status, verificationTaskFailed)
//			assert.Equal(t, len(docs), 1)
//			refetchDoc := docs[0]
//			assert.Equal(t, refetchDoc.ID, badId)
//			//assert.Equal(t, refetchDoc.DB, "keyhole")
//			//assert.Equal(t, refetchDoc.Collection, "dealers")
//		} else {
//			assert.Equal(t, task.Status, verificationTasksRetry)
//			assert.Equal(t, len(docs), 0)
//		}
//		assert.Equal(t, len(task.FailedIDs), 1)
//		assert.Equal(t, task.FailedIDs[0], badId)
//		assert.Equal(t, task.Attempts, i+1)
//	}
//	numberTasks := countDocs(t, verifier.verificationTaskCollection())
//
//	// Ensure refetch will add task to reverify
//	refetchData(ctx, verifier.mongopush)
//	newNumberTasks := countDocs(t, verifier.verificationTaskCollection())
//	assert.Equal(t, newNumberTasks, numberTasks+1)
//
//	tasks := []VerificationTask{}
//	cur, err := verifier.verificationTaskCollection().Find(ctx, bson.M{"status": verificationTaskAdded})
//	if err != nil {
//		t.Fatal(err)
//	}
//	err = cur.All(ctx, &tasks)
//	if err != nil {
//		t.Fatal(err)
//	}
//	assert.Equal(t, len(tasks), 1)
//	task := tasks[0]
//	assert.Equal(t, len(task.FailedIDs), 1)
//	assert.Equal(t, task.FailedIDs[0], badId)
//}
//
//func TestVerifierRangeResplit(t *testing.T) {
//	verifier := buildVerifier(t)
//	ctx := context.Background()
//
//	// No tasks created yet
//	_, err := verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// Insert 2 full batches of documents, and one partial
//	batchSizes := []int{10, 10, 3}
//	coll := verifier.srcClient.Database("keyhole").Collection("dealers")
//	for _, batchSize := range batchSizes {
//		var docs []interface{}
//		for i := 0; i < batchSize; i++ {
//			docs = append(docs, bson.M{"serial": i, "test": true, "name": "foo"})
//		}
//		coll.InsertMany(ctx, docs)
//	}
//
//	// Create batches for verification
//	//err = verifier.CreateAllVerifyTasks()
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//
//	batchesCreated := countDocs(t, verifier.verificationTaskCollection())
//	assert.Equal(t, int(batchesCreated), 3)
//	rangesCreated := countDocs(t, verifier.verificationRangeCollection())
//	assert.Equal(t, int(rangesCreated), 3)
//
//	// Make final range larger
//	var docs []interface{}
//	for i := 0; i < 20; i++ {
//		docs = append(docs, bson.M{"serial": i, "test": true, "name": "foo"})
//	}
//	coll.InsertMany(ctx, docs)
//
//	// drop the task collection and rebuild tasks, this time using the existing ranges
//	verifier.verificationTaskCollection().Drop(ctx)
//	_, err = verifier.FindNextVerifyTaskAndUpdate()
//	assert.NotNil(t, err)
//
//	// Create tasks from ranges
//	//err = verifier.CreateAllVerifyTasks()
//	//if err != nil {
//	//	t.Fatal(err)
//	//}
//
//	// Ensure more batches are created this time
//	batchesCreated = countDocs(t, verifier.verificationTaskCollection())
//	assert.Equal(t, int(batchesCreated), 5)
//	rangesCreated = countDocs(t, verifier.verificationRangeCollection())
//	assert.Equal(t, int(rangesCreated), 5)
//
//	allDocs := map[interface{}]interface{}{}
//	allBatchSizes := []int{}
//	batches := 0
//	for {
//		nextTask, err := verifier.FindNextVerifyTaskAndUpdate()
//		if err != nil {
//			break
//		}
//		documents, err := getDocuments(verifier.srcClientCollection(nextTask), nextTask)
//		if err != nil {
//			t.Fatal(err)
//		}
//		batchSize := 0
//		for docID, doc := range documents {
//			allDocs[docID] = doc
//			batchSize++
//		}
//		allBatchSizes = append(allBatchSizes, batchSize)
//		batches++
//	}
//	assert.Equal(t, len(allDocs), 43)
//	assert.Equal(t, batches, 5)
//	sort.Ints(allBatchSizes)
//	expectedBatchSizes := []int{3, 11, 11, 11, 11} // batches are size 11 since we use greater/less than or EQUAL to
//	if !reflect.DeepEqual(allBatchSizes, expectedBatchSizes) {
//		t.Fatalf("Batch sizes not equal: %+v - %+v", allBatchSizes, expectedBatchSizes)
//	}
//}
