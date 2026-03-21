package verifier

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/accum"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/option"
	"github.com/ccoveille/go-safecast/v2"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
	"golang.org/x/exp/slices"
)

const (
	mismatchesCollectionName = "mismatches"
)

type MismatchInfo struct {
	TaskID     bson.ObjectID
	TaskType   tasks.Type
	Generation int
	Detail     compare.Result
}

var _ bson.Marshaler = MismatchInfo{}

func (mi MismatchInfo) MarshalBSON() ([]byte, error) {
	return mi.MarshalToBSON(), nil
}

var (
	generationBSONLen   = len(bsoncore.AppendInt32Element(nil, "generation", 0))
	taskTypeBSONLenBase = len(bsoncore.AppendStringElement(nil, "taskType", ""))
	taskIDBSONLen       = len(bsoncore.AppendObjectIDElement(nil, "taskID", bson.ObjectID{}))
	detailBSONLenBase   = len(bsoncore.AppendDocumentElement(nil, "detail", nil))
)

func (mi MismatchInfo) MarshalToBSON() []byte {
	detail := mi.Detail.MarshalToBSON()

	bsonLen := 4 + // header
		generationBSONLen +
		taskTypeBSONLenBase + len(mi.TaskType) +
		taskIDBSONLen +
		detailBSONLenBase + len(detail) +
		1 // NUL

	buf := make(bson.Raw, 4, bsonLen)

	binary.LittleEndian.PutUint32(buf, safecast.MustConvert[uint32](bsonLen))

	buf = bsoncore.AppendInt32Element(buf, "generation", safecast.MustConvert[int32](mi.Generation))
	buf = bsoncore.AppendStringElement(buf, "taskType", string(mi.TaskType))
	buf = bsoncore.AppendObjectIDElement(buf, "taskID", mi.TaskID)
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
					// We query on generation & task type …
					{"generation", 1},
					{"taskType", 1},

					// … then sort descending.
					{"detail.mismatchHistory.durationMS", -1},
					{"detail.id", 1},
				},
			},

			// These are here so we can quickly recall last-rechecked
			// optimes on restart.
			{
				Keys: bson.D{
					{compare.SrcTimestampField, -1},
				},
			},
			{
				Keys: bson.D{
					{compare.DstTimestampField, -1},
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
) (map[bson.ObjectID][]compare.Result, error) {
	cursor, err := db.Collection(mismatchesCollectionName).Find(
		ctx,
		bson.D{
			{"taskID", bson.D{{"$in", taskIDs}}},
		},
	)
	if err != nil {
		return nil, errors.Wrapf(err, "querying mismatches for %d task(s)", len(taskIDs))
	}

	result := map[bson.ObjectID][]compare.Result{}

	for cursor.Next(ctx) {
		var d MismatchInfo
		if err := cursor.Decode(&d); err != nil {
			return nil, errors.Wrapf(err, "parsing discrepancy %+v", cursor.Current)
		}

		result[d.TaskID] = append(
			result[d.TaskID],
			d.Detail,
		)
	}

	if cursor.Err() != nil {
		return nil, errors.Wrapf(err, "reading %d tasks’ mismatches", len(taskIDs))
	}

	for _, taskID := range taskIDs {
		if _, ok := result[taskID]; !ok {
			result[taskID] = []compare.Result{}
		}
	}

	return result, nil
}

var (
	// A filter to identify docs marked “missing” (on either src or dst)
	missingFilter = agg.And{
		agg.Eq{"$detail.details", compare.Missing},
		agg.Eq{"$detail.field", ""},
	}

	missingOnDstFilter = append(
		slices.Clone(missingFilter),
		agg.Eq{
			"$detail.cluster",
			constants.ClusterTarget,
		},
	)

	extraOnDstFilter = append(
		slices.Clone(missingFilter),
		agg.Eq{
			"$detail.cluster",
			constants.ClusterSource,
		},
	)

	contentDiffersFilter = agg.Not{missingFilter}
)

func countMismatchesForGeneration(
	ctx context.Context,
	db *mongo.Database,
	generation int,
) (mismatchCountsPerType, error) {
	pl := mongo.Pipeline{
		{{"$match", bson.D{
			{"generation", generation},
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
		return mismatchCountsPerType{}, errors.Wrapf(err, "counting generation %d’s discrepancies", generation)
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
	generation int,
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
					bson.D{
						{"$and", []bson.D{
							{
								{"generation", generation},
								{"taskType", tasks.VerifyDocuments},
							},
							{{"$expr", categoryParts.filter}},
						}},
					},
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
			counts, err := countMismatchesForGeneration(ctx, db, generation)

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
	task *tasks.Task,
	problems []compare.Result,
) error {
	models := lo.Map(
		problems,
		func(r compare.Result, _ int) mongo.WriteModel {
			return &mongo.InsertOneModel{
				Document: MismatchInfo{
					TaskID:     task.PrimaryKey,
					TaskType:   task.Type,
					Generation: task.Generation,
					Detail:     r,
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

func getLongestLivedDocumentMismatch(
	ctx context.Context,
	db *mongo.Database,
	generation int,
) (option.Option[MismatchInfo], error) {
	raw, err := db.Collection(mismatchesCollectionName).FindOne(
		ctx,
		bson.D{
			{"generation", generation},
			{"taskType", tasks.VerifyDocuments},
		},
		options.FindOne().SetSort(
			bson.D{
				{"detail.mismatchHistory.durationMS", -1},
				{"detail.id", 1},
			},
		),
	).Raw()

	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[MismatchInfo](), nil
	} else if err != nil {
		return option.None[MismatchInfo](), errors.Wrap(err, "querying mismatches")
	}

	var mmInfo MismatchInfo

	if err := bson.Unmarshal(raw, &mmInfo); err != nil {
		return option.None[MismatchInfo](), errors.Wrap(err, "parsing mismatch")
	}

	return option.Some(mmInfo), nil
}

// SendDocumentMismatches outputs all document mismatches found in the prior
// generation (or, in generation 0, the current generation).
func (verifier *Verifier) SendDocumentMismatches(
	ctx context.Context,
	minDurationSecs uint32,
	out chan<- api.MismatchInfo,
) error {
	defer close(out)

	generation, _ := verifier.getGeneration()

	// A quick shortcut since a significant use case here is discovering
	// mismatches of nonzero duration.
	if generation == 0 && minDurationSecs > 0 {
		return nil
	}

	if generation > 0 {
		generation--
	}

	filter := bson.D{
		{"generation", generation},
		{"taskType", tasks.VerifyDocuments},
	}

	if minDurationSecs > 0 {
		filter = append(
			filter,
			bson.E{
				"detail.mismatchHistory.durationMS", bson.D{
					{"$gte", minDurationSecs},
				},
			},
		)
	}

	db := verifier.metaClient.Database(verifier.metaDBName)

	cursor, err := db.Collection(mismatchesCollectionName).Find(
		ctx,
		filter,
		options.Find().SetSort(
			bson.D{
				{"detail.mismatchHistory.durationMS", -1},
				{"detail.id", 1},
			},
		),
	)
	if err != nil {
		return fmt.Errorf("fetching generation %d’s mismatches: %w", generation, err)
	}

	for cursor.Next(ctx) {
		var mm MismatchInfo

		if err := cursor.Decode(&mm); err != nil {
			return fmt.Errorf("parsing generation %d’s mismatches: %w", generation, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- mm.Detail.APIMismatchInfo():
		}
	}

	if err := cursor.Err(); err != nil {
		return fmt.Errorf("reading generation %d’s mismatches: %w", generation, err)
	}

	return nil
}
