package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/namespaces"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ListAllUserNamespaces lists all the user collections on a cluster,
// in addition to time-series “system.buckets.*” collections.
//
// Unlike mongosync, we don't use the internal $listCatalog, since we need to
// work on old versions without that command. Thus, this does *NOT* run with
// majority read concern.
func ListAllUserNamespaces(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
	additionalExcludedDBs ...string,
) ([]string, error) {
	excludedDBs := []string{}
	excludedDBs = append(excludedDBs, additionalExcludedDBs...)
	excludedDBs = append(excludedDBs, namespaces.ExcludedSystemDBs...)

	var excluded []any
	for _, e := range excludedDBs {
		excluded = append(excluded, e)
	}

	dbNames, err := client.ListDatabaseNames(ctx, bson.D{
		{"$and", []bson.D{
			{{"name", bson.D{{"$nin", excluded}}}},
			util.ExcludePrefixesQuery("name", namespaces.ExcludedDBPrefixes),
		}},
	})

	if err != nil {
		return nil, err
	}
	logger.Debug().
		Strs("databases", dbNames).
		Msg("All user databases.")

	return ListUserCollectionsForDBs(ctx, logger, client, includeViews, dbNames)
}

func ListUserCollectionsForDBs(ctx context.Context, logger *logger.Logger, client *mongo.Client, includeViews bool,
	databases []string) ([]string, error) {

	collectionNamespaces := []string{}
	for _, dbName := range databases {
		db := client.Database(dbName)

		filter := bson.D{
			{"$or", []bson.D{
				util.ExcludePrefixesQuery(
					"name",
					mslices.Of(namespaces.ExcludedSystemCollPrefix),
				),
				{
					{"$expr", mmongo.StartsWithAgg("$name", timeseriesBucketsPrefix)},
				},
			}},
		}

		specifications, err := db.ListCollectionSpecifications(ctx, filter, options.ListCollections().SetNameOnly(true))
		if err != nil {
			return nil, err
		}
		logger.Debug().
			Str("database", dbName).
			Any("specifications", specifications).
			Msg("Found database members.")

		for _, spec := range specifications {
			collectionNamespaces = append(collectionNamespaces, dbName+"."+spec.Name)
		}
	}
	return collectionNamespaces, nil
}
