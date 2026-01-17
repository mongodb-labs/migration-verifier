package tasks

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func FetchPriorRecordIDs(
	ctx context.Context,
	namespace string,
	recordID bson.RawValue,
	tasksColl *mongo.Collection,
) ([]bson.RawValue, error) {
	cursor, err := tasksColl.Find(
		ctx,
		bson.D{
			{"generation", 0},
			{"type", VerifyDocuments},
			{"query_filter.namespace", namespace},
			{"query_filter.partition._id.lowerBound.$recordId", bson.D{
				{"$lt", recordID},
			}},
		},
		options.Find().
			SetProjection(bson.D{
				{"_id", 0},
				{"rid", "$query_filter.partition._id.lowerBound.$recordId"},
			}).
			SetSort(bson.D{
				{"query_filter.partition._id.lowerBound.$recordId", -1},
			}),
	)
	if err != nil {
		return nil, fmt.Errorf("opening cursor to read task record IDs before %s", recordID)
	}

	type rec struct {
		RID bson.RawValue
	}

	var recs []rec

	if err := cursor.All(ctx, &recs); err != nil {
		return nil, fmt.Errorf("reading task record IDs before %s", recordID)
	}

	return mslices.Map1(
		recs,
		func(r rec) bson.RawValue {
			return r.RID
		},
	), nil
}
