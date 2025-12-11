package verifier

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/agg"
	"github.com/10gen/migration-verifier/agg/accum"
	"github.com/10gen/migration-verifier/agg/helpers"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
			{
				Keys: bson.D{
					{"detail.mismatchTimes.durationMS", 1},
				},
			},
		},
	)

	if err != nil {
		return errors.Wrapf(err, "creating indexes for collection %#q", mismatchesCollectionName)
	}

	return nil
}

type recheckCounts struct {
	// FromMismatch are rechecks in the given generation from mismatches
	// in the prior generation.
	FromMismatch int64

	// FromChange are rechecks from changes seen in the prior generation.
	FromChange int64

	// Total adds up all of the given generation’s rechecks. This will be less
	// than FromMismatch + FromChange by however many documents both changed
	// and were seen to mismatch.
	Total int64

	// NewMismatches are mismatches seen thus far in the current generation
	// that will be rechecked in the next generation.
	NewMismatches int64

	// MaxMismatchDuration indicates the longest-lived mismatch, among either
	// the current or the prior generation.
	MaxMismatchDuration option.Option[time.Duration]
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

				// NB: We don’t filter by task status because we need to count
				// all rechecks, including those in tasks that turned up no
				// mismatches.
			}}},
			{{"$lookup", bson.D{
				{"from", mismatchesCollectionName},
				{"localField", "_id"},
				{"foreignField", "task"},
				{"as", "mismatches"},
				{"pipeline", mongo.Pipeline{
					{{"$group", bson.D{
						{"_id", nil},

						{"count", accum.Sum{1}},
						{"maxDurationMS", accum.Max{"$detail.mismatchTimes.durationMS"}},
					}}},
				}},
			}}},

			{{"$addFields", bson.D{
				// We avoid $unwind here because that’ll erase any tasks that
				// don’t match up to >=1 mismatch.
				{"mismatches", agg.ArrayElemAt{
					Array: "$mismatches",
					Index: 0,
				}},
				{"_ids", agg.Cond{
					If:   agg.Eq{0, "$generation"},
					Then: 0,
					Else: agg.Size{"$_ids"},
				}},
				{"rechecksFromChange", agg.Cond{
					If: agg.Or{
						agg.Eq{0, "$generation"},
						agg.Eq{generation - 1, "$generation"},
					},
					Then: 0,

					// _ids is the array of document IDs to recheck.
					// mismatch_first_seen_at maps indexes of that array to
					// the document’s first mismatch time. It only contains
					// entries for documents that mismatched without a change
					// event. Thus, any _ids member whose index is *not* in
					// mismatch_first_seen_at was enqueued from a change event.
					Else: agg.Size{agg.Filter{
						// This gives us all the array indices.
						Input: agg.Range{End: agg.Size{"$_ids"}},
						As:    "idx",
						Cond: agg.Not{helpers.Exists{
							agg.GetField{
								Input: "$mismatch_first_seen_at",
								Field: agg.ToString{"$$idx"},
							},
						}},
					}},
				}},
			}}},
			{{"$group", bson.D{
				{"_id", nil},
				{"allRechecks", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"$generation", generation},
						Then: "$_ids",
						Else: 0,
					},
				}},
				{"rechecksFromMismatch", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"$generation", generation - 1},
						Then: "$mismatches.count",
						Else: 0,
					},
				}},
				{"rechecksFromChange", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"$generation", generation},
						Then: "$rechecksFromChange",
						Else: 0,
					},
				}},
				{"newMismatches", accum.Sum{
					agg.Cond{
						If:   agg.Eq{"$generation", generation},
						Then: "$mismatches.count",
						Else: 0,
					},
				}},
				{"maxMismatchDurationMS", accum.Max{"$mismatches.maxDurationMS"}},
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

		// This happens if there were no failed or in-progress tasks in the queried generations.
		return recheckCounts{}, nil
	}

	result := struct {
		AllRechecks           int64
		RechecksFromMismatch  int64
		RechecksFromChange    int64
		NewMismatches         int64
		MaxMismatchDurationMS option.Option[int64]
	}{}

	err = cursor.Decode(&result)
	if err != nil {
		return recheckCounts{}, errors.Wrapf(err, "reading mismatches from result (%v)", cursor.Current)
	}

	/*
		if result.RechecksFromMismatch > result.AllRechecks {
			// TODO: fix
			slog.Warn(
				fmt.Sprintf(
					"Mismatches found in generation %d outnumber generation %d’s total docs to recheck. This should be rare.",
					generation-1,
					generation,
				),
				"priorGenMismatches", result.RechecksFromMismatch,
				"curGenRechecks", result.AllRechecks,
			)
		}
	*/

	return recheckCounts{
		Total:         result.AllRechecks,
		FromMismatch:  result.RechecksFromMismatch,
		FromChange:    result.RechecksFromChange,
		NewMismatches: result.NewMismatches,
		MaxMismatchDuration: option.Map(
			result.MaxMismatchDurationMS,
			func(ms int64) time.Duration {
				return time.Duration(ms) * time.Millisecond
			},
		),
	}, nil
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
	ContentDiffers []MismatchInfo
	MissingOnDst   []MismatchInfo
	ExtraOnDst     []MismatchInfo

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

func getDocumentMismatchReportData(
	ctx context.Context,
	db *mongo.Database,
	taskIDs []bson.ObjectID,
	limit int64,
) (mismatchReportData, error) {
	// A filter to identify docs marked “missing” (on either src or dst)
	missingFilter := getMismatchDocMissingAggExpr("$$ROOT")

	missingOnDstFilter := agg.And{
		missingFilter,
		agg.Eq{
			"$detail.cluster",
			ClusterTarget,
		},
	}
	extraOnDstFilter := agg.And{
		missingFilter,
		agg.Eq{
			"$detail.cluster",
			ClusterSource,
		},
	}

	contentDiffersFilter := agg.Not{missingFilter}

	pl := mongo.Pipeline{
		{{"$match", bson.D{
			{"task", bson.D{{"$in", taskIDs}}},
		}}},
		{{"$sort", bson.D{
			{"detail.mismatchTimes.duration", -1},
			{"detail.id", 1},
		}}},
		{{"$facet", bson.D{
			{"counts", mongo.Pipeline{
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
			}},
			{"contentDiffers", mongo.Pipeline{
				{{"$match", bson.D{{"$expr", contentDiffersFilter}}}},
				{{"$limit", limit}},
			}},
			{"missingOnDst", mongo.Pipeline{
				{{"$match", bson.D{{"$expr", missingOnDstFilter}}}},
				{{"$limit", limit}},
			}},
			{"extraOnDst", mongo.Pipeline{
				{{"$match", bson.D{{"$expr", extraOnDstFilter}}}},
				{{"$limit", limit}},
			}},
		}}},
		{{"$addFields", bson.D{
			{"counts", agg.ArrayElemAt{
				Array: "$counts",
				Index: 0,
			}},
		}}},
	}

	cursor, err := db.Collection(mismatchesCollectionName).Aggregate(ctx, pl)

	if err != nil {
		return mismatchReportData{}, errors.Wrapf(err, "fetching %d tasks' discrepancies", len(taskIDs))
	}

	var results []mismatchReportData

	if err := cursor.All(ctx, &results); err != nil {
		return mismatchReportData{}, errors.Wrapf(err, "reading mismatch aggregation")
	}

	if len(results) != 1 {
		panic(fmt.Sprintf("got != 1 result: %+v", results))
	}

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
