package util

import (
	"context"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

const (
	configDBName  = "config"
	collsCollName = "collections"
)

// GetShardKey returns the collection's shard key, or an empty option
// if the collection is unsharded.
func GetShardKey(
	ctx context.Context,
	coll *mongo.Collection,
) (option.Option[bson.Raw], error) {
	namespace := coll.Database().Name() + "." + coll.Name()

	configCollectionsColl := coll.Database().Client().
		Database(configDBName).
		Collection(collsCollName)

	decoded := struct {
		Key option.Option[bson.Raw]
	}{}

	err := configCollectionsColl.
		FindOne(ctx, bson.D{{"_id", namespace}}).
		Decode(&decoded)

	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[bson.Raw](), nil
	} else if err != nil {
		return option.None[bson.Raw](), errors.Wrapf(
			err,
			"failed to find sharding info for %#q",
			namespace,
		)
	}

	key, hasKey := decoded.Key.Get()

	if !hasKey {
		return option.None[bson.Raw](), nil
	}

	return option.Some(key), nil
}

// Used in tests:
func DisableBalancing(ctx context.Context, coll *mongo.Collection) error {
	client := coll.Database().Client()
	configDB := client.Database("config")

	ns := FullName(coll)

	_, err := configDB.
		Collection(
			"collections",
			options.Collection().SetWriteConcern(writeconcern.Majority()),
		).
		UpdateOne(
			ctx,
			bson.D{{"_id", ns}},
			bson.D{
				{"$set", bson.D{
					{"noBalance", true},
				}},
			},
		)

	if err != nil {
		return errors.Wrapf(err, "disabling %#q’s shard balancing", ns)
	}
	return nil
}
