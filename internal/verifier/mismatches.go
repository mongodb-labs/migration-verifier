package verifier

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	mismatchesCollectionName = "mismatches"
)

type MismatchInfo struct {
	Task   bson.ObjectID
	Detail VerificationResult
}

// Returns an aggregation that indicates whether the MismatchInfo refers to
// a missing document.
func getMismatchDocMissingAggExpr(docExpr any) bson.D {
	return getResultDocMissingAggExpr(
		bson.D{{"$getField", bson.D{
			{"input", docExpr},
			{"field", "detail"},
		}}},
	)
}

var _ bson.Marshaler = MismatchInfo{}

func (mi MismatchInfo) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON().")
}

func (mi MismatchInfo) MarshalToBSON() []byte {
	detail := mi.Detail.MarshalToBSON()

	bsonLen := 4 + // header
		1 + 4 + 1 + len(bson.ObjectID{}) + // Task
		1 + 6 + 1 + len(detail) + // Detail
		1 // NUL

	buf := make(bson.Raw, 4, bsonLen)

	binary.LittleEndian.PutUint32(buf, uint32(bsonLen))

	buf = bsoncore.AppendObjectIDElement(buf, "task", mi.Task)
	buf = bsoncore.AppendDocumentElement(buf, "detail", detail)

	buf = append(buf, 0)

	if len(buf) != bsonLen {
		panic(fmt.Sprintf("%T BSON length is %d but expected %d", mi, len(buf), bsonLen))
	}

	return buf
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

type mismatchCounts struct {
	// Total counts all of the mismatches.
	Total int

	// Match counts all mismatches that satisfy the filter.
	Match int

	// Multi counts mismatches that have been seen more than once without
	// a change event. These are of concern because they indicate a mismatch
	// that the migrator tool (e.g., mongosync) may never rectify.
	Multi int

	// MultiMatch counts mismatches that have been seen more than once *and*
	// satisfy the filter.
	MultiMatch int
}

func countMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	filter bson.D,
) (mismatchCounts, error) {
	multiExpr := agg.Gt{"detail.mismatches", 1}

	cursor, err := db.Collection(mismatchesCollectionName).Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$match", bson.D{
				{"task", bson.D{{"$in", taskIDs}}},
			}}},
			{{"$group", bson.D{
				{"_id", nil},
				{"total", agg.Sum{1}},
				{"match", agg.Sum{agg.Cond{
					If:   filter,
					Then: 1,
					Else: 0,
				}}},
				{"multi", agg.Sum{agg.Cond{
					If:   multiExpr,
					Then: 1,
					Else: 0,
				}}},
				{"multiMatch", agg.Sum{agg.Cond{
					If: agg.And{
						filter,
						multiExpr,
					},
					Then: 1,
					Else: 0,
				}}},
			}}},
		},
	)

	if err != nil {
		return mismatchCounts{}, errors.Wrap(err, "sending mismatch-counting query")
	}

	var got []mismatchCounts
	if err := cursor.All(ctx, &got); err != nil {
		return mismatchCounts{}, errors.Wrap(err, "reading mismatch counts")
	}

	if len(got) != 1 {
		return mismatchCounts{}, fmt.Errorf("unexpected mismatch count result: %+v", got)
	}

	return got[0], nil
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
				}.MarshalToBSON(),
			}
		},
	)

	_, err := db.Collection(mismatchesCollectionName).BulkWrite(
		ctx,
		models,
	)

	return errors.Wrapf(err, "recording %d mismatches", len(models))
}
