package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

const UnauthorizedErrCode = 13

// RefreshSrcMongosInstances prevents data corruption from SERVER-32198, which can cause reads and writes to be
// accepted by the wrong shard (this is caused by a mongos not knowing the collection is sharded and the shard not
// knowing the collection is sharded). This method relies on the verifier rejecting 4.4 source SRV connection strings
// (see the `auditor` package for more details).
//
// This method must only be called on a sharded cluster, otherwise it returns a "no such command: 'listShards'" error.
//
// This function should only be called on the source cluster because its
// messaging (e.g., errors) say “source”. See RefreshDstMongosInstances for
// equivalent logic for the destination cluster.
//
// Note: this is a reimplementation of MaybeRefreshAllSourceMongosInstances() in mongosync.
func RefreshSrcMongosInstances(
	ctx context.Context,
	l *logger.Logger,
	clientOpts *options.ClientOptions,
) error {
	return refreshMongoses(ctx, l, clientOpts, "source")
}

// RefreshDstMongosInstances is the same as RefreshSrcMongosInstances, but
// logging & error messages say “destination” rather than “source”.
func RefreshDstMongosInstances(
	ctx context.Context,
	l *logger.Logger,
	clientOpts *options.ClientOptions,
) error {
	return refreshMongoses(ctx, l, clientOpts, "destination")
}

func refreshMongoses(
	ctx context.Context,
	l *logger.Logger,
	clientOpts *options.ClientOptions,
	peerName string,
) error {
	hosts := clientOpts.Hosts
	l.Info().
		Strs("hosts", hosts).
		Str("whichCluster", peerName).
		Msg("Refreshing all mongos instances.")

	for _, host := range hosts {
		singleHostClientOpts := *clientOpts

		// Only connect to one host at a time.
		singleHostClientOpts.SetHosts([]string{host})

		// Only open 1 connection to each mongos to reduce the risk of overwhelming the source cluster.
		singleHostClientOpts.SetMaxConnecting(1)

		singleHostClient, err := mongo.Connect(&singleHostClientOpts)
		if err != nil {
			return errors.Wrapf(err, "failed to connect to mongos host %#q", host)
		}

		shardConnStr, err := getAnyExistingShardConnectionStr(
			ctx,
			l,
			singleHostClient,
		)
		if err != nil {
			return err
		}

		err = retry.New().WithCallback(
			func(ctx context.Context, _ *retry.FuncInfo) error {
				// Query a collection on the config server with linearizable read concern to advance the config
				// server primary's majority-committed optime. This populates the $configOpTime.
				opts := options.Database().SetReadConcern(readconcern.Linearizable())
				err := singleHostClient.
					Database("admin", opts).
					Collection("system.version").
					FindOne(
						ctx,
						bson.D{{"_id", "featureCompatibilityVersion"}},
					).
					Err()
				if err != nil {
					return errors.Wrap(err, "failed to query the config server")
				}

				// Run `addShard` on an existing shard to force the mongos' ShardRegistry to refresh. This combined
				// with the previous step guarantees that all shards are known to the mongos.
				err = singleHostClient.
					Database("admin").
					RunCommand(ctx, bson.D{{"addShard", shardConnStr}}).
					Err()
				if err != nil {
					desc := fmt.Sprintf("failed to execute %#q on %s host %#q to force refresh of mongos shard registry", "addShard", peerName, host)

					// TODO (REP-3952): Do this error check using the `shared` package.
					if mmongo.ErrorHasCode(err, UnauthorizedErrCode) {
						desc += fmt.Sprintf(
							"; please restart migration-verifier with a %s URI that includes the `clusterManager` role",
							peerName,
						)
					}

					return errors.Wrapf(err, "%s", desc)
				}

				// We could alternatively run `flushRouterConfig: <dbName>` for each db, but that requires a
				// listDatabases call. We should _never_ run `flushRouterConfig: <dName>.<collName>` because that
				// would cause the mongos to no longer know whether the collection is sharded or not. See this
				// document: https://docs.google.com/document/d/1C0EG2Qx2ECZbUsaNdGDTY-5JK0NISeo5_UT9oMG1dps/edit
				// for more information.
				err = singleHostClient.
					Database("admin").
					RunCommand(ctx, bson.D{{"flushRouterConfig", 1}}).
					Err()
				if err != nil {
					return errors.Wrapf(err, "failed to flush the %s mongos config", peerName)
				}

				return nil
			},
			"refreshing mongos shard cache",
		).Run(ctx, l)

		if err != nil {
			return err
		}

		if err = singleHostClient.Disconnect(ctx); err != nil {
			l.Warn().
				Str("host", host).
				Str("whichCluster", peerName).
				Msg("Failed to disconnect gracefully.")
		}
	}

	l.Info().
		Strs("hosts", hosts).
		Msgf("Successfully refreshed %s mongos instances.", peerName)

	return nil
}

// getAnyExistingShardConnectionStr will return the shard connection string of
// a shard in the current cluster. If the cluster is not sharded,
// an empty string and error will be returned.
//
// Note: this is a reimplementation of a method of the same name in mongosync.
func getAnyExistingShardConnectionStr(
	ctx context.Context,
	l *logger.Logger,
	client *mongo.Client,
) (string, error) {
	res, err := runListShards(ctx, l, client)
	if err != nil {
		return "", err
	}

	doc, err := res.Raw()
	if err != nil {
		return "", err
	}

	rawHost, lookupErr := doc.LookupErr("shards", "0", "host")
	if lookupErr != nil {
		return "", lookupErr
	}

	shardConnStr, ok := rawHost.StringValueOK()
	if !ok {
		return "", errors.New("failed to convert rawHost to string")
	}

	return shardConnStr, nil
}

// runListShards returns the mongo.SingleResult from running the listShards command.
//
// Note: this is a reimplementation of a method of the same name in mongosync.
func runListShards(
	ctx context.Context,
	l *logger.Logger,
	client *mongo.Client,
) (*mongo.SingleResult, error) {
	var res *mongo.SingleResult
	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			res = client.Database("admin").RunCommand(ctx, bson.D{{"listShards", 1}})
			return res.Err()
		},
		"listing shards",
	).Run(ctx, l)
	return res, err
}
