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
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
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

	// For failed tasks, this stores the document IDs missing on
	// one cluster or the other.
	Ids []interface{} `bson:"_ids"`

	// Deprecated: VerificationTask ID field is ignored by the verifier.
	ID int `bson:"id"`

	// For failed tasks, this stores details on documents that exist on
	// both clusters but don’t match.
	FailedDocs []VerificationResult `bson:"failed_docs,omitempty"`

	QueryFilter QueryFilter `bson:"query_filter" json:"query_filter"`

	// DocumentCount is set when the verifier is done with the task
	// (whether we found mismatches or not).
	SourceDocumentCount types.DocumentCount `bson:"source_documents_count"`

	// ByteCount is like DocumentCount: set when the verifier is done
	// with the task.
	SourceByteCount types.ByteCount `bson:"source_bytes_count"`
}

func (t *VerificationTask) augmentLogWithDetails(evt *zerolog.Event) {
	if len(t.Ids) > 0 {
		evt.Int("documentCount", len(t.Ids))
	} else {
		evt.
			Interface("minDocID", t.QueryFilter.Partition.Key.Lower).
			Interface("maxDocID", t.QueryFilter.Partition.Upper)
	}
}

// VerificationRange stores ID ranges for tasks that can be re-used between runs
type VerificationRange struct {
	PrimaryKey primitive.ObjectID `bson:"_id"`
	StartID    primitive.ObjectID `bson:"start_id"`
	EndID      primitive.ObjectID `bson:"end_id"`
}

func (verifier *Verifier) insertCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
	generation int) (*VerificationTask, error) {

	dstNamespace := srcNamespace
	if verifier.nsMap.Len() != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap.GetDstNamespace(srcNamespace)
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

	err := retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {
			_, err := verifier.verificationTaskCollection().InsertOne(ctx, verificationTask)
			return err
		},
	)

	return &verificationTask, err
}

func (verifier *Verifier) InsertCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
) (*VerificationTask, error) {
	return verifier.insertCollectionVerificationTask(ctx, srcNamespace, verifier.generation)
}

func (verifier *Verifier) InsertFailedCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
) (*VerificationTask, error) {
	return verifier.insertCollectionVerificationTask(ctx, srcNamespace, verifier.generation+1)
}

func (verifier *Verifier) InsertPartitionVerificationTask(
	ctx context.Context,
	partition *partitions.Partition,
	shardKeys []string,
	dstNamespace string,
) (*VerificationTask, error) {
	srcNamespace := strings.Join([]string{partition.Ns.DB, partition.Ns.Coll}, ".")

	task := VerificationTask{
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

	err := retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {
			_, err := verifier.verificationTaskCollection().InsertOne(ctx, &task)

			return err
		},
	)

	return &task, err
}

func (verifier *Verifier) InsertDocumentRecheckTask(
	ctx context.Context,
	ids []interface{},
	dataSize types.ByteCount,
	srcNamespace string,
) (*VerificationTask, error) {
	dstNamespace := srcNamespace
	if verifier.nsMap.Len() != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap.GetDstNamespace(srcNamespace)
		if !ok {
			return nil, fmt.Errorf("Could not find Namespace %s", srcNamespace)
		}
	}

	task := VerificationTask{
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

	err := retry.Retry(ctx, verifier.logger, func(ctx context.Context, _ *retry.FuncInfo) error {
		_, err := verifier.verificationTaskCollection().InsertOne(ctx, &task)

		return err
	})

	return &task, err
}

func (verifier *Verifier) FindNextVerifyTaskAndUpdate(
	ctx context.Context,
) (option.Option[VerificationTask], error) {
	task := &VerificationTask{}

	err := retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {

			err := verifier.verificationTaskCollection().FindOneAndUpdate(
				ctx,
				bson.M{
					"$and": []bson.M{
						{"generation": verifier.generation},
						{"type": bson.M{
							"$in": mslices.Of(
								verificationTaskVerifyDocuments,
								verificationTaskVerifyCollection,
							),
						}},
						{"status": verificationTaskAdded},
					},
				},
				bson.M{
					"$set": bson.M{
						"status":     verificationTaskProcessing,
						"begin_time": time.Now(),
					},
				},
				options.FindOneAndUpdate().
					SetReturnDocument(options.After).

					// We want “verifyCollection” tasks before “verify”(-document) ones.
					SetSort(bson.M{"type": -1}),
			).Decode(task)

			if err != nil {
				task = nil
				if errors.Is(err, mongo.ErrNoDocuments) {
					err = nil
				}
			}

			return err
		},
	)

	return option.FromPointer(task), err
}

func (verifier *Verifier) UpdateVerificationTask(ctx context.Context, task *VerificationTask) error {
	return retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateOne(
				ctx,
				bson.M{"_id": task.PrimaryKey},
				bson.M{
					"$set": bson.M{
						"status":                 task.Status,
						"failed_docs":            task.FailedDocs,
						"source_documents_count": task.SourceDocumentCount,
						"source_bytes_count":     task.SourceByteCount,
						"_ids":                   task.Ids,
					},
				},
			)
			if err != nil {
				return err
			}
			if result.MatchedCount == 0 {
				return TaskError{
					Code:    ErrorUpdateTask,
					Message: fmt.Sprintf(`no matched task updated: "%v"`, task.PrimaryKey),
				}
			}

			return err
		},
	)
}

func (verifier *Verifier) CreatePrimaryTaskIfNeeded(ctx context.Context) (bool, error) {
	ownerSetId := primitive.NewObjectID()

	var created bool

	err := retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateOne(
				ctx,
				bson.M{"type": verificationTaskPrimary},
				bson.M{
					"$setOnInsert": bson.M{
						"_id":    ownerSetId,
						"type":   verificationTaskPrimary,
						"status": verificationTaskAdded,
					},
				},
				options.Update().SetUpsert(true),
			)
			if err != nil {
				return err
			}

			created = result.UpsertedID == ownerSetId

			return nil
		},
	)

	return created, err
}

func (verifier *Verifier) UpdatePrimaryTaskComplete(ctx context.Context) error {
	return retry.Retry(
		ctx,
		verifier.logger,
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateMany(
				ctx,
				bson.M{"type": verificationTaskPrimary},
				bson.M{
					"$set": bson.M{
						"status": verificationTaskCompleted,
					},
				},
			)
			if err != nil {
				return err
			}
			if result.MatchedCount == 0 {
				return TaskError{
					Code:    ErrorUpdateTask,
					Message: `no primary task found`,
				}
			}

			return nil
		},
	)
}
