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
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	verificationTasksCollection = "verification_tasks"
)

func (verifier *Verifier) insertCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
	generation int) (*tasks.Task, error) {

	dstNamespace := srcNamespace
	if verifier.nsMap.Len() != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap.GetDstNamespace(srcNamespace)
		if !ok {
			return nil, fmt.Errorf("could not find Namespace %s", srcNamespace)
		}
	}

	verificationTask := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Generation: generation,
		Status:     tasks.Added,
		Type:       tasks.VerifyCollection,
		QueryFilter: tasks.QueryFilter{
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
	}

	srcColl := verifier.srcClientCollection(&verificationTask)

	size, docs, _, err := partitions.GetSizeAndDocumentCount(
		ctx,
		verifier.logger,
		srcColl,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"getting %#q’s size & doc count: %w",
			srcNamespace,
			err,
		)
	}

	verificationTask.SourceByteCount = size
	verificationTask.SourceDocumentCount = docs

	logEvent := verifier.logger.Debug().
		Any("task", verificationTask.PrimaryKey)

	if srcNamespace == dstNamespace {
		logEvent.
			Str("namespace", srcNamespace)
	} else {
		logEvent.
			Str("srcNamespace", srcNamespace).
			Str("dstNamespace", dstNamespace)
	}

	logEvent.Msg("Adding metadata task.")

	err = retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			_, err := verifier.verificationTaskCollection().InsertOne(ctx, verificationTask)
			return err
		},
		"persisting namespace %#q's verification task",
		srcNamespace,
	).Run(ctx, verifier.logger)

	return &verificationTask, err
}

func (verifier *Verifier) InsertCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
) (*tasks.Task, error) {
	return verifier.insertCollectionVerificationTask(ctx, srcNamespace, verifier.generation)
}

func (verifier *Verifier) InsertFailedCollectionVerificationTask(
	ctx context.Context,
	srcNamespace string,
) (*tasks.Task, error) {
	return verifier.insertCollectionVerificationTask(ctx, srcNamespace, verifier.generation+1)
}

func (verifier *Verifier) InsertPartitionVerificationTask(
	ctx context.Context,
	partition *partitions.Partition,
	shardKeys []string,
	dstNamespace string,
) (*tasks.Task, error) {
	srcNamespace := strings.Join([]string{partition.Ns.DB, partition.Ns.Coll}, ".")

	task := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Generation: verifier.generation,
		Status:     tasks.Added,
		Type:       tasks.VerifyDocuments,
		QueryFilter: tasks.QueryFilter{
			Partition: partition,
			ShardKeys: shardKeys,
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
	}

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			_, err := verifier.verificationTaskCollection().InsertOne(ctx, &task)

			return err
		},
		"persisting partition verification task for %#q (%v to %v)",
		task.QueryFilter.Namespace,
		task.QueryFilter.Partition.Key.Lower,
		task.QueryFilter.Partition.Upper,
	).Run(ctx, verifier.logger)

	return &task, err
}

func (verifier *Verifier) createDocumentRecheckTask(
	ids []bson.RawValue,
	firstMismatchTime map[int32]bson.DateTime,
	srcTimestamp option.Option[bson.Timestamp],
	dstTimestamp option.Option[bson.Timestamp],
	dataSize types.ByteCount,
	srcNamespace string,
) (*tasks.Task, error) {
	dstNamespace := srcNamespace
	if verifier.nsMap.Len() != 0 {
		var ok bool
		dstNamespace, ok = verifier.nsMap.GetDstNamespace(srcNamespace)
		if !ok {
			return nil, fmt.Errorf("could not find namespace %s", srcNamespace)
		}
	}

	return &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Generation: verifier.generation,
		Ids:        ids,
		Status:     tasks.Added,
		Type:       tasks.VerifyDocuments,
		QueryFilter: tasks.QueryFilter{
			Namespace: srcNamespace,
			To:        dstNamespace,
		},
		SourceDocumentCount: types.DocumentCount(len(ids)),
		SourceByteCount:     dataSize,
		FirstMismatchTime:   firstMismatchTime,
		SrcTimestamp:        srcTimestamp,
		DstTimestamp:        dstTimestamp,
	}, nil
}

func (verifier *Verifier) insertDocumentRecheckTasks(
	ctx context.Context,
	tasks []bson.Raw,
) error {
	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			_, err := verifier.verificationTaskCollection().InsertMany(ctx, tasks)

			return err
		},
		"persisting %d recheck tasks",
		len(tasks),
	).Run(ctx, verifier.logger)

	return err
}

func (verifier *Verifier) FindNextVerifyTaskAndUpdate(
	ctx context.Context,
) (option.Option[tasks.Task], error) {
	task := &tasks.Task{}

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {

			err := verifier.verificationTaskCollection().FindOneAndUpdate(
				ctx,
				bson.M{
					"$and": []bson.M{
						{"generation": verifier.generation},
						{"type": bson.M{
							"$in": mslices.Of(
								tasks.VerifyDocuments,
								tasks.VerifyCollection,
							),
						}},
						{"status": tasks.Added},
					},
				},
				bson.M{
					"$set": bson.M{
						"status":     tasks.Processing,
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
		"finding next task to do in generation %d",
		verifier.generation,
	).Run(ctx, verifier.logger)

	return option.FromPointer(task), err
}

func (verifier *Verifier) UpdateVerificationTask(ctx context.Context, task *tasks.Task) error {
	return retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateOne(
				ctx,
				bson.M{"_id": task.PrimaryKey},
				bson.M{
					"$set": bson.M{
						"status":                 task.Status,
						"source_documents_count": task.SourceDocumentCount,
						"source_bytes_count":     task.SourceByteCount,
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
		"updating task %v (namespace %#q)",
		task.PrimaryKey,
		task.QueryFilter.Namespace,
	).Run(ctx, verifier.logger)
}

func (verifier *Verifier) CreatePrimaryTaskIfNeeded(ctx context.Context) (bool, error) {
	ownerSetId := bson.NewObjectID()

	var created bool

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateOne(
				ctx,
				bson.M{"type": tasks.Primary},
				bson.M{
					"$setOnInsert": bson.M{
						"_id":    ownerSetId,
						"type":   tasks.Primary,
						"status": tasks.Added,
					},
				},
				options.UpdateOne().SetUpsert(true),
			)
			if err != nil {
				return err
			}

			created = result.UpsertedID == ownerSetId

			return nil
		},
		"ensuring primary task's existence",
	).Run(ctx, verifier.logger)

	return created, err
}

func (verifier *Verifier) UpdatePrimaryTaskComplete(ctx context.Context) error {
	return retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			result, err := verifier.verificationTaskCollection().UpdateMany(
				ctx,
				bson.M{"type": tasks.Primary},
				bson.M{
					"$set": bson.M{
						"status": tasks.Completed,
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
		"noting completion of primary task",
	).Run(ctx, verifier.logger)
}
