package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
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

	// ExcludedSystemCollPrefix is the prefix of system collections,
	// which we ignore.
	ExcludedSystemCollPrefix = "system."
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

		filter := bson.D{
			{"$or", []bson.D{
				util.ExcludePrefixesQuery(
					"name",
					mslices.Of(ExcludedSystemCollPrefix),
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
