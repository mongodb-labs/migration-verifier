package util

import (
	"context"

	"github.com/mongodb-labs/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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
