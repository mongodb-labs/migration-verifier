package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

const opTimeKeyInServerResponse = "operationTime"

// GetNewClusterTime creates a new cluster time, updates all shards’
// cluster times to meet or exceed that time, then returns it.
func GetNewClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
) (bson.Timestamp, error) {
	var clusterTime bson.Timestamp

	// First we just fetch the latest cluster time among all shards without
	// updating any shards’ oplogs.
	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			var err error
			clusterTime, err = runAppendOplogNote(
				ctx,
				client,
				"new ts",
				option.None[bson.Timestamp](),
			)
			return err
		},
		"appending oplog note to get cluster time",
	).Run(ctx, logger)

	if err != nil {
		return bson.Timestamp{}, err
	}

	// fetchClusterTime() will have taught the mongos about the most current
	// shard’s cluster time. Now we tell that mongos to update all lagging
	// shards to that time.
	err = retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			var err error
			_, err = runAppendOplogNote(
				ctx,
				client,
				"sync ts",
				option.Some(clusterTime),
			)
			return err
		},
		"appending oplog note to synchronize cluster",
	).Run(ctx, logger)
	if err != nil {
		// This isn't serious enough even for info-level.
		logger.Debug().Err(err).
			Msg("Failed to append oplog note; change stream may need extra time to finish.")
	}

	return clusterTime, nil
}

func runAppendOplogNote(
	ctx context.Context,
	client *mongo.Client,
	note string,
	maxClusterTimeOpt option.Option[bson.Timestamp],
) (bson.Timestamp, error) {
	cmd := bson.D{
		{"appendOplogNote", 1},
		{"data", bson.D{
			{"migration-verifier", note},
		}},
	}

	if maxClusterTime, has := maxClusterTimeOpt.Get(); has {
		cmd = append(cmd, bson.E{"maxClusterTime", maxClusterTime})
	}

	resp := client.
		Database(
			"admin",
			options.Database().SetWriteConcern(writeconcern.Majority()),
		).
		RunCommand(ctx, cmd)

	rawResponse, err := resp.Raw()

	// If any shard’s cluster time >= maxTime, the mongos will return a
	// StaleClusterTime error. This particular error doesn’t indicate a
	// failure, so we ignore it.
	if err != nil && !util.IsStaleClusterTimeError(err) {
		return bson.Timestamp{}, errors.Wrap(
			err,
			"failed to append note to oplog",
		)
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func getOpTimeFromRawResponse(rawResponse bson.Raw) (bson.Timestamp, error) {
	// Get the `operationTime` from the response and return it.
	var optime bson.Timestamp

	found, err := mbson.RawLookup(rawResponse, &optime, opTimeKeyInServerResponse)
	if err != nil {
		return bson.Timestamp{}, errors.Errorf("failed to read server response (%s)", rawResponse)
	}
	if !found {
		return bson.Timestamp{}, errors.Errorf("server response (%s) lacks %#q", rawResponse, opTimeKeyInServerResponse)
	}

	return optime, nil
}
