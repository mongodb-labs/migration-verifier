package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"context"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func buildVerifier(t *testing.T) *Verifier {
	qfilter := QueryFilter{Namespace: "keyhole.dealers"}
	task := VerificationTask{QueryFilter: qfilter}

	verifier := NewVerifier()
	verifier.SetNumWorkers(3)
	verifier.SetComparisonRetryDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(0)
	err := verifier.SetMetaURI(context.Background(), "mongodb://localhost:"+metaPort)
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.SetSrcURI(context.Background(), "mongodb://localhost:"+sourcePort)
	if err != nil {
		t.Fatal(err)
	}
	err = verifier.SetDstURI(context.Background(), "mongodb://localhost:"+destPort)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = verifier.SetLogger("stderr")
	if err != nil {
		t.Fatal(err)
	}
	verifier.SetMetaDBName("VERIFIER_META")
	verifier.verificationTaskCollection().Drop(context.Background())
	verifier.verificationRangeCollection().Drop(context.Background())
	verifier.refetchCollection().Drop(context.Background())
	verifier.srcClientCollection(&task).Drop(context.Background())
	verifier.dstClientCollection(&task).Drop(context.Background())
	return verifier
}

func TestVerifierCompareDocs(t *testing.T) {
	err := startTestMongods()
	if err != nil {
		t.Fatal(err)
	}
	defer stopTestMongods()
	verifier := buildVerifier(t)
	ctx := context.Background()
	drop := func() {
		verifier.srcClient.Database("keyhole").Drop(ctx)
		verifier.dstClient.Database("keyhole").Drop(ctx)
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
	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 123, "name": "foobar"})
	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 123, "name": "foobar"})

	task := &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	mismatchedIds, err := verifier.FetchAndCompareDocuments(task)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 0, len(mismatchedIds))
	drop()

	// Test different field orders
	task = &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "name": "foobar", "num": 123})
	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 123, "name": "foobar"})

	task = &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	mismatchedIds, err = verifier.FetchAndCompareDocuments(task)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 0, len(mismatchedIds))

	drop()
	id = rand.Intn(1000)
	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 1234, "name": "foobar"})
	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 123, "name": "foobar"})

	task = &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	mismatchedIds, err = verifier.FetchAndCompareDocuments(task)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, len(mismatchedIds))
	var res int
	mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res)
	assert.Equal(t, id, res)

	drop()
	// Test document missing on target
	id = rand.Intn(1000)
	verifier.srcClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 1234, "name": "foobar"})

	task = &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	mismatchedIds, err = verifier.FetchAndCompareDocuments(task)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, len(mismatchedIds))
	mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res)
	assert.Equal(t, id, res)

	drop()
	// Test document missing on source
	id = rand.Intn(1000)
	verifier.dstClient.Database("keyhole").Collection("dealers").InsertOne(ctx, bson.M{"_id": id, "num": 1234, "name": "foobar"})

	task = &VerificationTask{ID: id, QueryFilter: basicQueryFilter("keyhole.dealers")}
	mismatchedIds, err = verifier.FetchAndCompareDocuments(task)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, len(mismatchedIds), 1)
	mismatchedIds[0].ID.(bson.RawValue).Unmarshal(&res)
	assert.Equal(t, id, res)
}

func getVerificationTasks(t *testing.T, verifier *Verifier) []VerificationTask {
	ctx := context.Background()
	cursor, err := verifier.verificationTaskCollection().Find(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []VerificationTask{}
	for cursor.Next(ctx) {
		var task VerificationTask
		err := cursor.Decode(&task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func countDocs(t *testing.T, col *mongo.Collection) int64 {
	count, err := col.CountDocuments(context.Background(), bson.D{})
	if err != nil {
		t.Fatal(err)
	}

	return count
}

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
