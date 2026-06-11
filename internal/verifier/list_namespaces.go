package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/namespaces"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/timeseries"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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

	var dbNames []string
	collectionNamespaces := []string{}

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {

			var err error
			dbNames, err = client.ListDatabaseNames(ctx, bson.D{
				{"$and", []bson.D{
					{{"name", bson.D{{"$nin", excluded}}}},
					util.ExcludePrefixesQuery("name", namespaces.ExcludedDBPrefixes),
				}},
			})

			if err != nil {
				return err
			}

			return nil
		},
		"list DB names",
	).Run(ctx, logger)

	if err != nil {
		return nil, err
	}

	logger.Debug().
		Strs("databases", dbNames).
		Msg("All user databases.")

	for _, dbName := range dbNames {
		db := client.Database(dbName)

		filter := bson.D{
			{"$or", []bson.D{
				util.ExcludePrefixesQuery(
					"name",
					mslices.Of(namespaces.ExcludedSystemCollPrefix),
				),
				{
					{"$expr", mmongo.StartsWithAgg("$name", timeseries.BucketPrefix)},
				},
			}},
		}

		var collNames []string

		err := retry.New().WithCallback(
			func(ctx context.Context, _ *retry.FuncInfo) error {
				var err error
				collNames, err = db.ListCollectionNames(ctx, filter)
				return err
			},
			"list DB %#q’s collection names",
			dbName,
		).Run(ctx, logger)

		if err != nil {
			return nil, err
		}

		logger.Debug().
			Str("database", dbName).
			Strs("collections", collNames).
			Msg("All collection names.")

		for _, collName := range collNames {
			collectionNamespaces = append(
				collectionNamespaces,
				dbName+"."+collName,
			)
		}
	}
	return collectionNamespaces, nil
}
