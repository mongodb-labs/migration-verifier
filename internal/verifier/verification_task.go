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
	"github.com/10gen/migration-verifier/internal/types"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type verificationTaskType string
type verificationTaskStatus string

const (
	verificationTasksCollection = "verification_tasks"

	// --------------------------------------------------
	// Task statuses:
	// --------------------------------------------------

	verificationTaskAdded     verificationTaskStatus = "added"
	verificationTaskCompleted verificationTaskStatus = "completed"
	verificationTaskFailed    verificationTaskStatus = "failed"
	// This is used for collection verification, and means the task successfully created the data tasks,
	// but there were mismatches in the metadata/indexes
	verificationTaskMetadataMismatch verificationTaskStatus = "mismatch"
	verificationTaskProcessing       verificationTaskStatus = "processing"

	// --------------------------------------------------
	// Task types:
	// --------------------------------------------------

	// The “workhorse” task type: verify a partition of documents.
	verificationTaskVerifyDocuments verificationTaskType = "verify"

	// A verifyCollection task verifies collection metadata
	// and inserts verify-documents tasks to verify data ranges.
	verificationTaskVerifyCollection verificationTaskType = "verifyCollection"

	// The primary task creates a verifyCollection task for each
	// namespace.
	verificationTaskPrimary verificationTaskType = "primary"
)

// VerificationTask stores source cluster info
type VerificationTask struct {
	PrimaryKey interface{}            `bson:"_id"`
	Type       verificationTaskType   `bson:"type"`
	Status     verificationTaskStatus `bson:"status"`
	Generation int                    `bson:"generation"`
	Ids        []interface{}          `bson:"_ids"`
	// Deprecated: VerificationTask ID field is ignored by the verifier.
	ID          int                  `bson:"id"`
	FailedDocs  []VerificationResult `bson:"failed_docs,omitempty"`
	QueryFilter QueryFilter          `bson:"query_filter"          json:"query_filter"`

	// DocumentCount is set when the verifier is done with the task
	// (whether we found mismatches or not).
	SourceDocumentCount types.DocumentCount `bson:"source_documents_count"`

	// ByteCount is like DocumentCount: set when the verifier is done
	// with the task.
	SourceByteCount types.ByteCount `bson:"source_bytes_count"`
}

// VerificationRange stores ID ranges for tasks that can be re-used between runs
type VerificationRange struct {
	PrimaryKey primitive.ObjectID `bson:"_id"`
	StartID    primitive.ObjectID `bson:"start_id"`
	EndID      primitive.ObjectID `bson:"end_id"`
}

func (verifier *Verifier) insertCollectionVerificationTask(
	srcNamespace string,
	generation int) (*VerificationTask, error) {

	dstNamespace := srcNamespace
	if len(verifier.nsMap) != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap[srcNamespace]
		if !ok {
			return nil, fmt.Errorf("Could not find Namespace %s", srcNamespace)
		}
	}

	var msg string
	if srcNamespace == dstNamespace {
		msg = fmt.Sprintf("Adding task for %s", srcNamespace)
	} else {
		msg = fmt.Sprintf("Adding task for %s → %s", srcNamespace, dstNamespace)
	}
	verifier.logger.Debug().Msg(msg)

	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Generation: generation,
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyCollection,
		QueryFilter: QueryFilter{
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
	}
	_, err := verifier.verificationTaskCollection().InsertOne(context.Background(), verificationTask)
	return &verificationTask, err
}

func (verifier *Verifier) InsertCollectionVerificationTask(
	srcNamespace string) (*VerificationTask, error) {
	return verifier.insertCollectionVerificationTask(srcNamespace, verifier.generation)
}

func (verifier *Verifier) InsertFailedCollectionVerificationTask(
	srcNamespace string) (*VerificationTask, error) {
	return verifier.insertCollectionVerificationTask(srcNamespace, verifier.generation+1)
}

func (verifier *Verifier) InsertPartitionVerificationTask(partition *partitions.Partition, shardKeys []string,
	dstNamespace string) (*VerificationTask, error) {
	srcNamespace := strings.Join([]string{partition.Ns.DB, partition.Ns.Coll}, ".")
	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Generation: verifier.generation,
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyDocuments,
		QueryFilter: QueryFilter{
			Partition: partition,
			ShardKeys: shardKeys,
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
	}
	_, err := verifier.verificationTaskCollection().InsertOne(context.Background(), verificationTask)
	return &verificationTask, err
}

func (verifier *Verifier) InsertFailedIdsVerificationTask(ids []interface{}, dataSize types.ByteCount, srcNamespace string) error {
	dstNamespace := srcNamespace
	if len(verifier.nsMap) != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap[srcNamespace]
		if !ok {
			return fmt.Errorf("Could not find Namespace %s", srcNamespace)
		}
	}

	verificationTask := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Generation: verifier.generation,
		Ids:        ids,
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyDocuments,
		QueryFilter: QueryFilter{
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
		SourceDocumentCount: types.DocumentCount(len(ids)),
		SourceByteCount:     dataSize,
	}
	var ctx = context.Background()
	_, err := verifier.verificationTaskCollection().InsertOne(ctx, &verificationTask)
	return err
}

func (verifier *Verifier) FindNextVerifyTaskAndUpdate() (*VerificationTask, error) {
	var verificationTask = VerificationTask{}
	filter := bson.M{
		"$and": bson.A{
			bson.M{"generation": verifier.generation},
			bson.M{"type": bson.M{
				"$in": bson.A{
					verificationTaskVerifyDocuments,
					verificationTaskVerifyCollection,
				},
			}},
			bson.M{"status": verificationTaskAdded},
		},
	}

	coll := verifier.verificationTaskCollection()
	opts := options.FindOneAndUpdate()
	opts.SetReturnDocument(options.After)
	updates := bson.M{
		"$set": bson.M{
			"status":     verificationTaskProcessing,
			"begin_time": time.Now(),
		},
	}

	// We want “verifyCollection” tasks before “verify”(-document) ones.
	opts.SetSort(bson.M{"type": -1})

	err := coll.FindOneAndUpdate(context.Background(), filter, updates, opts).Decode(&verificationTask)
	return &verificationTask, err
}

func (verifier *Verifier) UpdateVerificationTask(task *VerificationTask) error {
	var ctx = context.Background()
	updateFields := bson.M{
		"$set": bson.M{
			"status":                 task.Status,
			"failed_docs":            task.FailedDocs,
			"source_documents_count": task.SourceDocumentCount,
			"source_bytes_count":     task.SourceByteCount,
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
