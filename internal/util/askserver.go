package util

import (
	"context"
	"slices"

	"github.com/mongodb-labs/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ServerThinksTheseMatch runs an aggregation on the server that determines
// whether the server thinks a & b are equal. This allows you, e.g., to
// ignore BSON type differences for equivalent numbers.
//
// tinker is an optional pipeline that operates on the documents in the
// pipeline (`a` and `b`, respectively) before they're compared.
//
// In migration-verifier the client is generally expected to be for
// the metadata cluster.
func ServerThinksTheseMatch(
	ctx context.Context,
	client *mongo.Client,
	a, b any,
	tinker option.Option[mongo.Pipeline],
) (bool, error) {
	pipeline := mongo.Pipeline{
		{{"$documents", []bson.D{
			{
				{"a", bson.D{{"$literal", a}}},
				{"b", bson.D{{"$literal", b}}},
			},
		}}},

		// Now check to be sure that those specs match.
		{{"$match", bson.D{
			{"$expr", bson.D{
				{"$eq", mslices.Of("$a", "$b")},
			}},
		}}},
	}

	if extra, hasExtra := tinker.Get(); hasExtra {
		pipeline = slices.Insert(
			pipeline,
			1,
			extra...,
		)
	}

	cursor, err := client.Database("admin").Aggregate(ctx, pipeline)

	if err == nil {
		defer cursor.Close(ctx)

		if cursor.Next(ctx) {
			return true, nil
		}

		err = cursor.Err()
	}

	return false, errors.Wrapf(err, "failed to ask server if a (%v) matches b (%v)", a, b)
}
