package verifier

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/accum"
	"github.com/10gen/migration-verifier/contextplus"
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

			// This index supports mismatch reports.
			{
				Keys: bson.D{
					{"detail.mismatchHistory.durationMS", -1},
					{"detail.id", 1},
				},
			},
		},
	)

	if err != nil {
		return errors.Wrapf(err, "creating indexes for collection %#q", mismatchesCollectionName)
	}

	return nil
}

type mismatchCountsPerType struct {
	MissingOnDst   int64
	ExtraOnDst     int64
	ContentDiffers int64
}

func (mct mismatchCountsPerType) Total() int64 {
	return mct.MissingOnDst + mct.ExtraOnDst + mct.ContentDiffers
}

type mismatchReportData struct {
	// ContentDiffers shows the most long-lived content-differing mismatches.
	ContentDiffers []MismatchInfo

	// MissingOnDst is like ContentDiffers but for mismatches where the
	// destination lacks the document.
	MissingOnDst []MismatchInfo

	// ExtraOnDst is like ContentDiffers but for mismatches where the
	// document exists on the destination but not the source.
	ExtraOnDst []MismatchInfo

	// Counts tallies up all mismatches, however long-lived.
	Counts mismatchCountsPerType
}

// This is a low-level function used to display metadata mismatches.
// It’s also used in tests.
func getMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
) (map[bson.ObjectID][]VerificationResult, error) {
	cursor, err := db.Collection(mismatchesCollectionName).Find(
		ctx,
		bson.D{
			{"task", bson.D{{"$in", taskIDs}}},
		},
	)
	if err != nil {
		return nil, errors.Wrapf(err, "querying mismatches for %d task(s)", len(taskIDs))
	}

	result := map[bson.ObjectID][]VerificationResult{}

	for cursor.Next(ctx) {
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
		return nil, errors.Wrapf(err, "reading %d tasks’ mismatches", len(taskIDs))
	}

	for _, taskID := range taskIDs {
		if _, ok := result[taskID]; !ok {
			result[taskID] = []VerificationResult{}
		}
	}

	return result, nil
}

var (
	// A filter to identify docs marked “missing” (on either src or dst)
	missingFilter = getMismatchDocMissingAggExpr("$$ROOT")

	missingOnDstFilter = agg.And{
		missingFilter,
		agg.Eq{
			"$detail.cluster",
			ClusterTarget,
		},
	}
	extraOnDstFilter = agg.And{
		missingFilter,
		agg.Eq{
			"$detail.cluster",
			ClusterSource,
		},
	}

	contentDiffersFilter = agg.Not{missingFilter}
)

func countMismatchesForTasks(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
) (mismatchCountsPerType, error) {
	pl := mongo.Pipeline{
		{{"$match", bson.D{
			{"task", bson.D{{"$in", taskIDs}}},
		}}},
		{{"$group", bson.D{
			{"_id", nil},

			{"contentDiffers", accum.Sum{agg.Cond{
				If:   contentDiffersFilter,
				Then: 1,
				Else: 0,
			}}},
			{"missingOnDst", accum.Sum{agg.Cond{
				If:   missingOnDstFilter,
				Then: 1,
				Else: 0,
			}}},
			{"extraOnDst", accum.Sum{agg.Cond{
				If:   extraOnDstFilter,
				Then: 1,
				Else: 0,
			}}},
		}}},
	}

	cursor, err := db.Collection(mismatchesCollectionName).Aggregate(ctx, pl)
	if err != nil {
		return mismatchCountsPerType{}, errors.Wrapf(err, "counting %d tasks’ discrepancies", len(taskIDs))
	}

	var results []mismatchCountsPerType

	if err := cursor.All(ctx, &results); err != nil {
		return mismatchCountsPerType{}, errors.Wrapf(err, "reading mismatch aggregation")
	}

	if len(results) != 1 {
		return mismatchCountsPerType{}, fmt.Errorf("surprise agg results count: %+v", results)
	}

	return results[0], nil
}

func getDocumentMismatchReportData(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	limit int64,
) (mismatchReportData, error) {
	pl := mongo.Pipeline{
		{{"$match", bson.D{
			{"task", bson.D{{"$in", taskIDs}}},
		}}},
		{{"$addFields", bson.D{
			{"_category", agg.Switch{
				Branches: []agg.SwitchCase{
					{Case: contentDiffersFilter, Then: "contentDiffers"},
					{Case: missingOnDstFilter, Then: "missingOnDst"},
					{Case: extraOnDstFilter, Then: "extraOnDst"},
				},
			}},
		}}},
		{{"$setWindowFields", bson.D{
			{"partitionBy", "_category"},
			{"sortBy", bson.D{
				{"detail.mismatchHistory.durationMS", -1},
				{"detail.id", 1},
			}},
			{"output", bson.D{
				{"num", bson.D{{"$denseRank", bson.D{}}}},
			}},
		}}},
		{{"$match", bson.D{
			{"num", bson.D{{"$lte", limit}}},
		}}},
		{{"$addFields", bson.D{
			{"num", "$$REMOVE"},
		}}},
		{{"$group", bson.D{
			{"_id", "$type"},
			{"docs", bson.D{{"$push", "$$ROOT"}}},
		}}},
		{{"$group", bson.D{
			{"_id", nil},
			{"result", bson.D{{"$push", bson.D{
				{"k", "$_id"},
				{"v", "$docs"},
			}}}},
		}}},
		{{"$replaceRoot", bson.D{
			{"newRoot", bson.D{
				{"$arrayToObject", "$result"},
			}},
		}}},
	}

	eg, egCtx := contextplus.ErrGroup(ctx)

	var results []mismatchReportData

	eg.Go(
		func() error {
			cursor, err := db.Collection(mismatchesCollectionName).Aggregate(
				egCtx,
				pl,

				// By default the server will traverse the detail index
				// due to SERVER-12923. It’s much more efficient for this query
				// to use the type index.
				options.Aggregate().SetHint(bson.D{{"task", 1}}),
			)

			if err != nil {
				return errors.Wrapf(err, "fetching %d tasks' discrepancies", len(taskIDs))
			}

			if err := cursor.All(egCtx, &results); err != nil {
				return errors.Wrapf(err, "reading mismatch aggregation")
			}

			if len(results) != 1 {
				panic(fmt.Sprintf("got != 1 result: %+v", results))
			}

			return nil
		},
	)

	var counts mismatchCountsPerType

	eg.Go(
		func() error {
			var err error

			counts, err = countMismatchesForTasks(ctx, db, taskIDs)

			return errors.Wrapf(err, "counting mismatches")
		},
	)

	if err := eg.Wait(); err != nil {
		return mismatchReportData{}, err
	}

	results[0].Counts = counts

	return results[0], nil
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
