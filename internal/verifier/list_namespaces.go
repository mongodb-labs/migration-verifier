package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	MongosyncMetaDBPrefixes = mslices.Of(
		"mongosync_internal_",
		"mongosync_reserved_",
	)
)

var (
	// ExcludedSystemDBs are system databases that are excluded from verification.
	ExcludedSystemDBs = []string{"admin", "config", "local"}

	// ExcludedSystemCollRegex is the regular expression representation of the excluded system collections.
	ExcludedSystemCollPrefix = "system."
)

// Lists all the user collections on a cluster.  Unlike mongosync, we don't use the internal $listCatalog, since we need to
// work on old versions without that command.  This means this does not run with read concern majority.
func ListAllUserCollections(ctx context.Context, logger *logger.Logger, client *mongo.Client, includeViews bool,
	additionalExcludedDBs ...string) ([]string, error) {
	excludedDBs := []string{}
	excludedDBs = append(excludedDBs, additionalExcludedDBs...)
	excludedDBs = append(excludedDBs, ExcludedSystemDBs...)

	var excluded []any
	for _, e := range excludedDBs {
		excluded = append(excluded, e)
	}

	dbNames, err := client.ListDatabaseNames(ctx, bson.D{
		{"$and", []bson.D{
			{{"name", bson.D{{"$nin", excluded}}}},
			util.ExcludePrefixesQuery("name", MongosyncMetaDBPrefixes),
		}},
	})

	if err != nil {
		return nil, err
	}
	logger.Debug().
		Strs("databases", dbNames).
		Msg("All user databases.")

	collectionNamespaces := []string{}
	for _, dbName := range dbNames {
		db := client.Database(dbName)
		//filter := bson.D{{"name", bson.D{{"$nin", bson.A{ExcludedSystemCollRegex}}}}}
		filter := bson.D{{"$and", []bson.D{
			util.ExcludePrefixesQuery("name", mslices.Of(ExcludedSystemCollPrefix)),
		}}}
		if !includeViews {
			filter = append(filter, bson.E{"type", bson.D{{"$ne", "view"}}})
		}
		specifications, err := db.ListCollectionSpecifications(ctx, filter, options.ListCollections().SetNameOnly(true))
		if err != nil {
			return nil, err
		}
		logger.Debug().
			Str("database", dbName).
			Interface("specifications", specifications).
			Msg("Found database members.")

		for _, spec := range specifications {
			collectionNamespaces = append(collectionNamespaces, dbName+"."+spec.Name)
		}
	}
	return collectionNamespaces, nil
}
