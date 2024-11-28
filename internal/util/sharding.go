package util

import (
	"context"

	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
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

	rawResult, err := configCollectionsColl.FindOne(ctx, bson.D{{"_id", namespace}}).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[bson.Raw](), nil
	} else if err != nil {
		return option.None[bson.Raw](), errors.Wrapf(
			err,
			"failed to find sharding info for %#q",
			namespace,
		)
	}

	keyAsVal, err := rawResult.LookupErr("key")
	if errors.Is(err, bsoncore.ErrElementNotFound) {
		return option.None[bson.Raw](), nil
	} else if err != nil {
		return option.None[bson.Raw](), errors.Wrapf(
			err,
			"failed to find %#q in %#q's %#q entry",
			"key",
			namespace,
			FullName(configCollectionsColl),
		)
	}

	keyAsRaw, isDoc := keyAsVal.DocumentOK()
	if !isDoc {
		return option.None[bson.Raw](), errors.Errorf(
			"%#q in %#q's %#q entry is of type %#q, not an object",
			"key",
			namespace,
			FullName(configCollectionsColl),
			keyAsVal.Type,
		)
	}

	return option.Some(keyAsRaw), nil
}
