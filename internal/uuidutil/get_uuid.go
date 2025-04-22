package uuidutil

import (
	"context"
	"fmt"

	"github.com/mongodb-labs/migration-verifier/internal/logger"
	"github.com/mongodb-labs/migration-verifier/internal/retry"
	"github.com/mongodb-labs/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// NamespaceAndUUID represents a collection and database name and its corresponding UUID.  It is
// used as a bridge between code which expects collection UUIDs and code which uses namespaces only.
type NamespaceAndUUID struct {
	// The collection's UUID
	UUID util.UUID

	// The collection's database name
	DBName string

	// The collection's name.
	CollName string
}

func GetCollectionNamespaceAndUUID(ctx context.Context, logger *logger.Logger, db *mongo.Database, collName string) (*NamespaceAndUUID, error) {
	binaryUUID, uuidErr := GetCollectionUUID(ctx, logger, db, collName)
	if uuidErr != nil {
		return nil, uuidErr
	}
	return &NamespaceAndUUID{
		UUID:     util.ParseBinary(binaryUUID),
		DBName:   db.Name(),
		CollName: collName,
	}, nil
}

func GetCollectionUUID(ctx context.Context, logger *logger.Logger, db *mongo.Database, collName string) (*primitive.Binary, error) {
	filter := bson.D{{"name", collName}}
	opts := options.ListCollections().SetNameOnly(false)

	var collSpecs []*mongo.CollectionSpecification
	err := retry.New().WithCallback(
		func(_ context.Context, ri *retry.FuncInfo) error {
			ri.Log(logger.Logger, "ListCollectionSpecifications", db.Name(), collName, "Getting collection UUID.", "")
			var driverErr error
			collSpecs, driverErr = db.ListCollectionSpecifications(ctx, filter, opts)
			return driverErr
		},
		"getting namespace %#q's specification",
		db.Name()+"."+collName,
	).Run(ctx, logger)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list collections specification")
	}

	if len(collSpecs) != 1 {
		fmt.Println("!!!")
		fmt.Println(collName)
		return nil, errors.Errorf("number of matching collections should be 1")
	}

	util.Invariant(logger, collSpecs[0].UUID != nil, "Collection has nil UUID (most probably is a view): %v", collSpecs[0])

	return collSpecs[0].UUID, nil
}
