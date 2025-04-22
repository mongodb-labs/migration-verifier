package util

import (
	"context"

	"github.com/mongodb-labs/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// CollectionSpec is like mongo.CollectionSpecification except:
// - IDIndex is a bson.Raw rather than mongo.IndexSpecification.
// - It can detect unexpected fields.
type CollectionSpec struct {
	Name    string
	Type    string
	Options bson.Raw
	Info    struct {
		ReadOnly bool `bson:"readOnly"`
		UUID     primitive.Binary

		Extra map[string]any
	}
	IDIndex bson.Raw `bson:"idIndex"`

	Extra map[string]any
}

// FullName returns the collection's full namespace.
func FullName(collection *mongo.Collection) string {
	return collection.Database().Name() + "." + collection.Name()
}

// GetCollectionSpecIfExists returns the given collection’s specification,
// or empty if the collection doesn’t exist. If any unexpected properties
// exist in the collection specification then an error is returned.
func GetCollectionSpecIfExists(
	ctx context.Context,
	coll *mongo.Collection,
) (option.Option[CollectionSpec], error) {
	cursor, err := coll.Database().ListCollections(ctx, bson.M{"name": coll.Name()})
	if err != nil {
		return option.None[CollectionSpec](), errors.Wrapf(
			err,
			"failed to fetch %#q's specification",
			FullName(coll),
		)
	}

	var specs []CollectionSpec
	err = cursor.All(ctx, &specs)
	if err != nil {
		return option.None[CollectionSpec](), errors.Wrapf(
			err,
			"failed to parse %#q's specification",
			FullName(coll),
		)
	}

	switch len(specs) {
	case 0:
		return option.None[CollectionSpec](), nil
	case 1:
		if len(specs[0].Extra) > 0 || len(specs[0].Info.Extra) > 0 {
			return option.None[CollectionSpec](), errors.Wrapf(
				err,
				"%#q's specification (%v) contains unrecognized fields",
				FullName(coll),
				specs[0],
			)
		}

		return option.Some(specs[0]), nil
	}

	return option.None[CollectionSpec](), errors.Wrapf(
		err,
		"received multiple results (%v) when fetching %#q's specification",
		specs,
		FullName(coll),
	)
}
