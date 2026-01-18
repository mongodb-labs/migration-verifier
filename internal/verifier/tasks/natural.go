package tasks

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func FetchPriorResumeTokens(
	ctx context.Context,
	namespace string,
	recordID bson.RawValue,
	tasksColl *mongo.Collection,
) ([]bson.Raw, error) {
	cursor, err := tasksColl.Find(
		ctx,
		bson.D{
			{"generation", 0},
			{"type", VerifyDocuments},
			{"query_filter.namespace", namespace},
			{"query_filter.partition._id.lowerBound." + partitions.RecordID, bson.D{
				{"$lt", recordID},
			}},
		},
		options.Find().
			SetProjection(bson.D{
				{"_id", 0},
				{"token", "$query_filter.partition._id.lowerBound"},
			}).
			SetSort(bson.D{
				{"query_filter.partition._id.lowerBound." + partitions.RecordID, -1},
			}),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "opening cursor to read task record IDs before %s", recordID)
	}

	type rec struct {
		Token bson.Raw
	}

	var recs []rec

	if err := cursor.All(ctx, &recs); err != nil {
		return nil, fmt.Errorf("reading task record IDs before %s", recordID)
	}

	return mslices.Map1(
		recs,
		func(r rec) bson.Raw {
			return r.Token
		},
	), nil
}
