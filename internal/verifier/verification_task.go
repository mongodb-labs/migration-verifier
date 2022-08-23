package verifier

// Copyright (C) MongoDB, Inc. 2020-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/partitions"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	verificationTaskAdded       = "added"
	verificationTaskCompleted   = "completed"
	verificationTaskFailed      = "failed"
	verificationTaskProcessing  = "processing"
	verificationTasksCollection = "verification_tasks"
	// TODO: This may actually be necessary in the final product
	//verificationRangeCollection = "verification_ranges"
	verificationTasksRetry     = "retry"
	verificationTaskMaxRetries = 5

	verificationTaskVerify = "verify"
	// A verifyCollection task verifies collection metadata, and inserts tasks to verify data ranges.
	verificationTaskVerifyCollection = "verifyCollection"
	verificationTaskPrimary          = "primary"
)

// VerificationTask stores source cluster info
type VerificationTask struct {
	PrimaryKey  interface{}          `bson:"_id"`
	Ids         []interface{}        `bson:"_ids"`
	ID          int                  `bson:"id"`
	FailedIDs   []interface{}        `bson:"failed_ids"`
	Attempts    int                  `bson:"attempts"`
	RetryAfter  time.Time            `bson:"retry_after"`
	Status      string               `bson:"status"`
	Type        string               `bson:"type"`
	ParentID    interface{}          `bson:"parent_id"`
	FailedDocs  []VerificationResult `bson:"failed_docs,omitempty"`
	QueryFilter QueryFilter          `json:"query_filter" bson:"query_filter"`
}

// VerificationRange stores ID ranges for tasks that can be re-used between runs
type VerificationRange struct {
	PrimaryKey primitive.ObjectID `bson:"_id"`
	StartID    primitive.ObjectID `bson:"start_id"`
	EndID      primitive.ObjectID `bson:"end_id"`
}

func InsertRetryVerificationTask(failedId interface{}, collection *mongo.Collection) (*VerificationTask, error) {
	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerify,
		FailedIDs:  []interface{}{failedId},
		Attempts:   0,
	}
	_, err := collection.InsertOne(context.Background(), verificationTask)
	return &verificationTask, err
}

func InsertPartitionVerificationTask(partition *partitions.Partition, dstNamespace string, collection *mongo.Collection) (*VerificationTask, error) {
	srcNamespace := strings.Join([]string{partition.Ns.DB, partition.Ns.Coll}, ".")
	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerify,
		QueryFilter: QueryFilter{
			Partition: *partition,
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
		Attempts: 0,
	}
	_, err := collection.InsertOne(context.Background(), verificationTask)
	return &verificationTask, err
}

func InsertCollectionVerificationTask(srcNamespace string, dstNamespace string, collection *mongo.Collection) (*VerificationTask, error) {
	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyCollection,
		QueryFilter: QueryFilter{
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
	}
	_, err := collection.InsertOne(context.Background(), verificationTask)
	return &verificationTask, err
}

func (verifier *Verifier) FindNextVerifyTaskAndUpdate() (*VerificationTask, error) {
	var verificationTask = VerificationTask{}
	filter := bson.M{
		"$and": bson.A{
			bson.M{"type": bson.M{"$in": bson.A{verificationTaskVerify, verificationTaskVerifyCollection}}},
			bson.M{"$or": bson.A{
				bson.M{"status": verificationTaskAdded},
				bson.M{"$and": bson.A{
					bson.M{"status": verificationTasksRetry},
					bson.M{"retry_after": bson.M{"$lte": primitive.NewDateTimeFromTime(time.Now())}},
				}},
			}},
		},
	}

	coll := verifier.verificationTaskCollection()
	opts := options.FindOneAndUpdate()
	opts.SetReturnDocument(options.After)
	updates := bson.M{"$set": bson.M{"status": verificationTaskProcessing, "begin_time": time.Now()}}
	err := coll.FindOneAndUpdate(context.Background(), filter, updates, opts).Decode(&verificationTask)
	return &verificationTask, err
}

func (verifier *Verifier) UpdateVerificationTask(task *VerificationTask) error {
	var ctx = context.Background()
	updateFields := bson.M{
		"$set": bson.M{
			"status":      task.Status,
			"attempts":    task.Attempts,
			"failed_ids":  task.FailedIDs,
			"retry_after": task.RetryAfter,
			"failed_docs": task.FailedDocs,
		},
	}
	result, err := verifier.verificationTaskCollection().UpdateOne(ctx, bson.M{"_id": task.PrimaryKey}, updateFields)
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return TaskError{Code: ErrorUpdateTask, Message: fmt.Sprintf(`no matched task updated: "%v"`, task.PrimaryKey)}
	}

	return err
}

func (verifier *Verifier) CheckIsPrimary() (bool, error) {
	ownerSetId := primitive.NewObjectID()
	filter := bson.M{"type": verificationTaskPrimary}
	opts := options.Update()
	opts.SetUpsert(true)
	update := bson.M{
		"$setOnInsert": bson.M{
			"_id":    ownerSetId,
			"type":   verificationTaskPrimary,
			"status": verificationTaskAdded,
		},
	}
	result, err := verifier.verificationTaskCollection().UpdateOne(context.Background(), filter, update, opts)
	if err != nil {
		return false, err
	}

	isPrimary := result.UpsertedID == ownerSetId
	return isPrimary, nil
}

func (verifier *Verifier) UpdatePrimaryTaskComplete() error {
	var ctx = context.Background()
	updateFields := bson.M{
		"$set": bson.M{
			"status": verificationTaskCompleted,
		},
	}
	result, err := verifier.verificationTaskCollection().UpdateMany(ctx, bson.M{"type": verificationTaskPrimary}, updateFields)
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return TaskError{Code: ErrorUpdateTask, Message: `no primary task found`}
	}

	return err
}
