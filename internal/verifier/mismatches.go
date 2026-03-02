package verifier

import (
	"context"
	"encoding/binary"
	"fmt"
	"iter"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/accum"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	mismatchesCollectionName = "mismatches"

	maxMismatchBatchBytes = 5 << 20 // somewhat arbitrary
	maxMismatchCount      = 10_000
)

type MismatchInfo struct {
	Task   bson.ObjectID
	Detail VerificationResult
}

// Returns an aggregation that indicates whether the MismatchInfo refers to
// a missing document.
func getMismatchDocMissingAggExpr(docExpr any) any {
	return getResultDocMissingAggExpr(
		agg.GetField{
			Input: docExpr,
			Field: "detail",
		},
	)
}

var _ bson.Marshaler = MismatchInfo{}

func (mi MismatchInfo) MarshalBSON() ([]byte, error) {
	panic("Use MarshalToBSON().")
}

// AppendBSON appends the struct’s BSON representation to the
// given slice & returns the new slice.
func (mi MismatchInfo) AppendBSON(buf []byte) []byte {
	bsonStart := len(buf)
	buf = binary.LittleEndian.AppendUint32(buf, 0)

	buf = bsoncore.AppendObjectIDElement(buf, "task", mi.Task)
	buf = bsoncore.AppendDocumentElement(buf, "detail", nil)
	buf = mi.Detail.AppendBSON(buf)

	buf = append(buf, 0)

	bsonLen := len(buf) - bsonStart

	binary.LittleEndian.PutUint32(buf[bsonStart:], uint32(bsonLen))

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
	eg, egCtx := contextplus.ErrGroup(ctx)

	var reportData mismatchReportData

	mismatchesColl := db.Collection(mismatchesCollectionName)

	for _, categoryParts := range []struct {
		label  string
		ptr    *[]MismatchInfo
		filter any
	}{
		{"mismatched content", &reportData.ContentDiffers, contentDiffersFilter},
		{"missing on dst", &reportData.MissingOnDst, missingOnDstFilter},
		{"extra on dst", &reportData.ExtraOnDst, extraOnDstFilter},
	} {
		eg.Go(
			func() error {
				cursor, err := mismatchesColl.Find(
					egCtx,
					bson.D{{"$expr", agg.And{
						categoryParts.filter,
						agg.In("$task", taskIDs),
					}}},
					options.Find().
						SetSort(
							bson.D{
								{"detail.mismatchHistory.durationMS", -1},
								{"detail.id", 1},
							},
						).
						SetLimit(limit),
				)

				if err != nil {
					return errors.Wrapf(err, "fetching %#q", categoryParts.label)
				}

				if err := cursor.All(egCtx, categoryParts.ptr); err != nil {
					return errors.Wrapf(err, "reading %#q", categoryParts.label)
				}

				return nil
			},
		)
	}

	eg.Go(
		func() error {
			counts, err := countMismatchesForTasks(ctx, db, taskIDs)

			reportData.Counts = counts

			return errors.Wrapf(err, "counting mismatches")
		},
	)

	if err := eg.Wait(); err != nil {
		return mismatchReportData{}, err
	}

	return reportData, nil
}

func recordMismatches(
	ctx context.Context,
	db *mongo.Database,
	taskID bson.ObjectID,
	problems iter.Seq[VerificationResult],
) error {
	if option.IfNotZero(taskID).IsNone() {
		panic("empty task ID given")
	}

	totalBytes := 0
	var curProblems []bson.Raw
	var arena []byte

	eg, egCtx := contextplus.ErrGroup(ctx)

	var flush = func() {
		batchArena := make([]byte, 0, len(arena))
		batch := make([]bson.Raw, 0, len(curProblems))

		for _, prob := range curProblems {
			start := len(batchArena)
			batchArena = append(batchArena, prob...)
			batch = append(batch, bson.Raw(batchArena[start:]))
		}

		curProblems = curProblems[:0]
		arena = arena[:0]

		eg.Go(func() error {
			_, err := db.Collection(mismatchesCollectionName).InsertMany(
				egCtx,
				batch,
				options.InsertMany().SetOrdered(false),
			)

			return errors.Wrapf(err, "persisting batch of %d mismatches for task %v", len(batch), taskID)
		})
	}

	var nextMismatch bson.Raw

	for prob := range problems {
		nextMismatch = MismatchInfo{
			Task:   taskID,
			Detail: prob,
		}.AppendBSON(nextMismatch[:0])

		if totalBytes+len(nextMismatch) > maxMismatchBatchBytes {
			flush()
		}

		start := len(arena)
		arena = append(arena, nextMismatch...)
		curProblems = append(curProblems, arena[start:])

		if len(curProblems) == maxMismatchCount {
			flush()
		}
	}

	if len(curProblems) > 0 {
		flush()
	}

	return eg.Wait()
}
