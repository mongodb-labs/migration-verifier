package testutil

import (
	"context"
	"testing"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
)

// Marshal wraps `bsonMarshal` with a panic on failure.
func MustMarshal(doc any) bson.Raw {
	raw, err := bson.Marshal(doc)
	if err != nil {
		panic("bson.Marshal (error in test): " + err.Error())
	}

	return raw
}

// DocsToCursor returns an in-memory cursor created from the given
// documents with a panic on failure.
func DocsToCursor(docs []bson.D) *mongo.Cursor {
	cursor, err := mongo.NewCursorFromDocuments(convertDocsToAnys(docs), nil, nil)
	if err != nil {
		panic("NewCursorFromDocuments (error in test): " + err.Error())
	}

	return cursor
}

func convertDocsToAnys(docs []bson.D) []any {
	anys := make([]any, len(docs))
	for i, doc := range docs {
		anys[i] = doc
	}

	return anys
}

func KillApplicationChangeStreams(
	ctx context.Context,
	t *testing.T,
	client *mongo.Client,
	appName string,
) error {
	// Kill verifier’s change stream.
	cursor, err := client.Database(
		"admin",
		options.Database().SetReadConcern(readconcern.Local()),
	).Aggregate(
		ctx,
		mongo.Pipeline{
			{
				{"$currentOp", bson.D{
					{"idleCursors", true},
				}},
			},
			{
				{"$match", bson.D{
					{"clientMetadata.application.name", appName},
					{"command.collection", "$cmd.aggregate"},
					{"cursor.originatingCommand.pipeline.0.$_internalChangeStreamOplogMatch",
						bson.D{{"$type", "object"}},
					},
				}},
			},
		},
	)
	if err != nil {
		return errors.Wrapf(err, "failed to list %#q's change streams", appName)
	}

	ops := []struct {
		Opid any
	}{}
	err = cursor.All(ctx, &ops)
	if err != nil {
		return errors.Wrapf(err, "failed to decode %#q's change streams", appName)
	}

	for _, op := range ops {
		t.Logf("Killing change stream op %+v", op.Opid)

		err :=
			client.Database("admin").RunCommand(
				ctx,
				bson.D{
					{"killOp", 1},
					{"op", op.Opid},
				},
			).Err()

		if err != nil {
			return errors.Wrapf(err, "failed to kill change stream with opId %#q", op.Opid)
		}
	}

	return nil
}
