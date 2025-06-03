package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	mismatchesCollectionName = "mismatches"
)

type MismatchInfo struct {
	Task   primitive.ObjectID
	Detail VerificationResult
}

func createMismatchesCollection(ctx context.Context, db *mongo.Database) error {
	_, err := db.Collection(mismatchesCollectionName).Indexes().CreateMany(
		ctx,
		[]mongo.IndexModel{
			{
				Keys: bson.D{
					{"task", 1},
				},
			},
		},
	)

	if err != nil {
		return errors.Wrapf(err, "creating indexes for collection %#q", mismatchesCollectionName)
	}

	return nil
}

func getMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []primitive.ObjectID,
) (map[primitive.ObjectID][]VerificationResult, error) {
	cursor, err := db.Collection(mismatchesCollectionName).Find(
		ctx,
		bson.D{
			{"task", bson.D{{"$in", taskIDs}}},
		},
	)

	if err != nil {
		return nil, errors.Wrapf(err, "fetching %d tasks' discrepancies", len(taskIDs))
	}

	result := map[primitive.ObjectID][]VerificationResult{}

	for cursor.Next(ctx) {
		if cursor.Err() != nil {
			break
		}

		var d MismatchInfo
		if err := cursor.Decode(&d); err != nil {
			return nil, errors.Wrapf(err, "parsing discrepancy %+v", cursor.Current)
		}

		result[d.Task] = append(
			result[d.Task],
			d.Detail,
		)
	}

	if cursor.Err() != nil {
		return nil, errors.Wrapf(err, "reading %d tasks' discrepancies", len(taskIDs))
	}

	for _, taskID := range taskIDs {
		if _, ok := result[taskID]; !ok {
			result[taskID] = []VerificationResult{}
		}
	}

	return result, nil
}

func recordMismatches(
	ctx context.Context,
	db *mongo.Database,
	taskID primitive.ObjectID,
	problems []VerificationResult,
) error {
	if option.IfNotZero(taskID).IsNone() {
		panic("empty task ID given")
	}

	models := lo.Map(
		problems,
		func(r VerificationResult, _ int) mongo.WriteModel {
			return &mongo.InsertOneModel{
				Document: MismatchInfo{
					Task:   taskID,
					Detail: r,
				},
			}
		},
	)

	_, err := db.Collection(mismatchesCollectionName).BulkWrite(
		ctx,
		models,
	)

	return errors.Wrapf(err, "recording %d mismatches", len(models))
}
