// Package collspec provides types and helpers for fetching MongoDB collection
// specifications.
package collspec

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
		UUID     bson.Binary

		Extra map[string]any
	}
	IDIndex bson.Raw `bson:"idIndex"`

	Extra map[string]any
}

func fullName(collection *mongo.Collection) string {
	return collection.Database().Name() + "." + collection.Name()
}

// GetCollectionSpecIfExists returns the given collection's specification,
// or empty if the collection doesn't exist. If any unexpected properties
// exist in the collection specification then an error is returned.
func GetCollectionSpecIfExists(
	ctx context.Context,
	logger *logger.Logger,
	coll *mongo.Collection,
) (option.Option[CollectionSpec], error) {
	var specs []CollectionSpec

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			cursor, err := coll.Database().ListCollections(ctx, bson.M{"name": coll.Name()})
			if err != nil {
				return errors.Wrapf(
					err,
					"fetch %#q's specification",
					fullName(coll),
				)
			}

			err = cursor.All(ctx, &specs)
			if err != nil {
				return errors.Wrapf(
					err,
					"parse %#q's specification",
					fullName(coll),
				)
			}

			return nil
		},
		"fetching namespace %#q's specification",
		fullName(coll),
	).Run(ctx, logger)

	switch len(specs) {
	case 0:
		return option.None[CollectionSpec](), nil
	case 1:
		if len(specs[0].Extra) > 0 || len(specs[0].Info.Extra) > 0 {
			return option.None[CollectionSpec](), errors.Wrapf(
				err,
				"%#q's specification (%v) contains unrecognized fields",
				fullName(coll),
				specs[0],
			)
		}

		return option.Some(specs[0]), nil
	}

	return option.None[CollectionSpec](), errors.Wrapf(
		err,
		"received multiple results (%v) when fetching %#q's specification",
		specs,
		fullName(coll),
	)
}
