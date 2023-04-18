package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	MongosyncMetaDB = "mongosync_reserved_for_internal_use"
)

var (
	// ExcludedSystemDBs are system databases that are excluded from verification.
	ExcludedSystemDBs = []string{"admin", "config", "local"}

	// ExcludedSystemCollRegex is the regular expression representation of the excluded system collections.
	ExcludedSystemCollRegex = primitive.Regex{Pattern: `^system[.]`, Options: ""}
)

// Lists all the user collections on a cluster.  Unlike mongosync, we don't use the internal $listCatalog, since we need to
// work on old versions without that command.  This means this does not run with read concern majority.
func ListAllUserCollections(ctx context.Context, logger *logger.Logger, client *mongo.Client, includeViews bool,
	additionalExcludedDBs ...string) ([]string, error) {
	excludedDBs := []string{}
	excludedDBs = append(excludedDBs, additionalExcludedDBs...)
	excludedDBs = append(excludedDBs, ExcludedSystemDBs...)
	excludedDBs = append(excludedDBs, MongosyncMetaDB)

	dbNames, err := client.ListDatabaseNames(ctx, bson.D{{"name", bson.D{{"$nin", excludedDBs}}}})
	if err != nil {
		return nil, err
	}
	logger.Debug().Msgf("All user databases: %+v", dbNames)

	collectionNamespaces := []string{}
	for _, dbName := range dbNames {
		db := client.Database(dbName)
		filter := bson.D{{"name", bson.D{{"$nin", bson.A{ExcludedSystemCollRegex}}}}}
		if !includeViews {
			filter = append(filter, bson.E{"type", bson.D{{"$ne", "view"}}})
		}
		specifications, err := db.ListCollectionSpecifications(ctx, filter, options.ListCollections().SetNameOnly(true))
		if err != nil {
			return nil, err
		}
		logger.Debug().Msgf("Collections for database %s: %+v", dbName, specifications)
		for _, spec := range specifications {
			collectionNamespaces = append(collectionNamespaces, dbName+"."+spec.Name)
		}
	}
	return collectionNamespaces, nil
}
