package verifier

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/accum"
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

func countMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	filter any,
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

	switch len(got) {
	case 0:
		return 0, 0, nil
	case 1:
	default:
		return 0, 0, fmt.Errorf("unexpected mismatch count (%d) result: %+v", len(got), got)
	}

	totalRV, err := got[0].LookupErr("total")
	if err != nil {
		return 0, 0, errors.Wrap(err, "getting mismatch count’s total")
	}

	matchRV, err := got[0].LookupErr("match")
	if err != nil {
		return 0, 0, errors.Wrap(err, "getting mismatch count’s filter-match count")
	}

	matched := matchRV.AsInt64()

	return matched, totalRV.AsInt64() - matched, nil
}

type recheckCounts struct {
	// FromMismatch are rechecks from previously-seen mismatches.
	FromMismatch int64

	// FromChange are rechecks from previously-seen changes.
	FromChange int64

	// NewMismatches are newly-seen mismatches that *will* require rechecks.
	NewMismatches int64
}

// NB: This is OK to call for generation==0. In this case it will only
// add up newly-seen document mismatches.
func countRechecksForGeneration(
	ctx context.Context,
	metaDB *mongo.Database,
	generation int,
) (recheckCounts, error) {

	// The numbers we need are:
	// - the given generation’s total # of docs to recheck
	// - the # of mismatches found in the prior generation
	cursor, err := metaDB.Collection(verificationTasksCollection).Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$match", bson.D{
				{"generation", bson.D{{"$in", []any{generation, generation - 1}}}},
				{"type", verificationTaskVerifyDocuments},
			}}},
			{{"$lookup", bson.D{
				{"from", mismatchesCollectionName},
				{"localField", "_id"},
				{"foreignField", "task"},
				{"as", "mismatches"},
			}}},
			{{"$group", bson.D{
				{"_id", nil},
				{"allRechecks", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"generation", generation},
						Then: agg.Size{"$_ids"},
						Else: 0,
					},
				}},
				{"rechecksFromMismatch", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"generation", generation - 1},
						Then: agg.Size{"$mismatches"},
						Else: 0,
					},
				}},
				{"newMismatches", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"generation", generation},
						Then: agg.Size{"$mismatches"},
						Else: 0,
					},
				}},
			}}},
		},
	)
	if err != nil {
		return recheckCounts{}, errors.Wrap(err, "sending query to count last generation’s found mismatches")
	}

	defer cursor.Close(ctx)

	if !cursor.Next(ctx) {
		if cursor.Err() != nil {
			return recheckCounts{}, errors.Wrap(err, "reading count of last generation’s found mismatches")
		}

		// This happens if there were no tasks in the queried generations.
		return recheckCounts{}, nil
	}

	result := struct {
		AllRechecks          int64
		RechecksFromMismatch int64
		NewMismatches        int64
	}{}

	err = cursor.Decode(&result)
	if err != nil {
		return recheckCounts{}, errors.Wrapf(err, "reading mismatches from result (%v)", cursor.Current)
	}

	if result.RechecksFromMismatch > result.AllRechecks {
		return recheckCounts{}, fmt.Errorf(
			"INTERNAL ERROR: generation %d’s found mismatches (%d) outnumber generation %d’s total documents to recheck (%d); this is nonsensical",
			generation-1,
			result.RechecksFromMismatch,
			generation,
			result.AllRechecks,
		)
	}

	return recheckCounts{
		FromMismatch:  result.RechecksFromMismatch,
		FromChange:    result.AllRechecks - result.RechecksFromMismatch,
		NewMismatches: result.NewMismatches,
	}, nil
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
