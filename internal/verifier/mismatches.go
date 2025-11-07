package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	mismatchesCollectionName = "mismatches"
)

type MismatchInfo struct {
	Task   bson.ObjectID
	Detail VerificationResult
}

func getMismatchDocMissingAggExpr(docExpr any) bson.D {
	return getResultDocMissingAggExpr(
		bson.D{{"$getField", bson.D{
			{"input", docExpr},
			{"field", "detail"},
		}}},
	)
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

func countMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	filter bson.D,
) (int64, int64, error) {
	cursor, err := db.Collection(mismatchesCollectionName).Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$match", bson.D{
				{"task", bson.D{{"$in", taskIDs}}},
			}}},
			{{"$group", bson.D{
				{"_id", nil},
				{"total", bson.D{{"$sum", 1}}},
				{"match", bson.D{{"$sum", bson.D{
					{"$cond", bson.D{
						{"if", filter},
						{"then", 1},
						{"else", 0},
					}},
				}}}},
			}}},
		},
	)

	if err != nil {
		return 0, 0, errors.Wrap(err, "sending mismatch-counting query")
	}

	var got []bson.Raw
	if err := cursor.All(ctx, &got); err != nil {
		return 0, 0, errors.Wrap(err, "reading mismatch counts")
	}

	if len(got) != 1 {
		return 0, 0, errors.Wrapf(err, "unexpected mismatch count result: %+v")
	}

	totalRV, err := got[0].LookupErr("total")
	if err != nil {
		return 0, 0, errors.Wrapf(err, "getting mismatch count: %+v")
	}

	matchRV, err := got[0].LookupErr("match")
	if err != nil {
		return 0, 0, errors.Wrapf(err, "getting mismatch count: %+v")
	}

	matched := matchRV.AsInt64()

	return matched, totalRV.AsInt64() - matched, nil
}

func getMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	filter option.Option[bson.D],
	limit option.Option[int64],
) (map[bson.ObjectID][]VerificationResult, error) {
	findOpts := options.Find().
		SetSort(
			bson.D{
				{"detail.id", 1},
			},
		)

	if limit, has := limit.Get(); has {
		findOpts.SetLimit(limit)
	}

	query := bson.D{
		{"task", bson.D{{"$in", taskIDs}}},
	}

	if filter, has := filter.Get(); has {
		query = bson.D{
			{"$and", []bson.D{query, filter}},
		}
	}
	cursor, err := db.Collection(mismatchesCollectionName).Find(
		ctx,
		query,
		findOpts,
	)

	if err != nil {
		return nil, errors.Wrapf(err, "fetching %d tasks' discrepancies", len(taskIDs))
	}

	result := map[bson.ObjectID][]VerificationResult{}

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
	taskID bson.ObjectID,
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
