package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const opTimeKeyInServerResponse = "operationTime"

// GetNewClusterTime advances the cluster time and returns that time.
// All shards’ cluster times will meet or exceed the returned time.
func GetNewClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	retryer := retry.New(retry.DefaultDurationLimit)

	var clusterTime primitive.Timestamp

	// First we just fetch the latest cluster time among all shards without
	// updating any shards’ oplogs.
	err := retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			clusterTime, err = fetchNewClusterTime(ctx, client)
			return err
		},
	)

	if err != nil {
		return primitive.Timestamp{}, err
	}

	// fetchClusterTime() will have taught the mongos about the most current
	// shard’s cluster time. Now we tell that mongos to update all lagging
	// shards to that time.
	err = retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			clusterTime, err = syncClusterTimeAcrossShards(ctx, client, clusterTime)
			return err
		},
	)
	if err != nil {
		// This isn't serious enough even to warn on, so leave it at info-level.
		logger.Info().Err(err).
			Msg("Failed to append oplog note; change stream may need extra time to finish.")
	}

	return clusterTime, nil
}

// Use this when we just need the correct cluster time without
// actually changing any shards’ oplogs.
func fetchNewClusterTime(
	ctx context.Context,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	_, rawResponse, err := runAppendOplogNote(
		ctx,
		client,
		"expect StaleClusterTime error",
	)
	if err != nil {
		return primitive.Timestamp{}, errors.Wrap(
			err,
			"failed to append note to oplog",
		)
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func syncClusterTimeAcrossShards(
	ctx context.Context,
	client *mongo.Client,
	maxTime primitive.Timestamp,
) (primitive.Timestamp, error) {
	_, rawResponse, err := runAppendOplogNote(
		ctx,
		client,
		"syncing cluster time",
		bson.E{"maxClusterTime", maxTime},
	)

	if err != nil {
		return primitive.Timestamp{}, errors.Wrap(
			err,
			"failed to append note to oplog",
		)
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func runAppendOplogNote(
	ctx context.Context,
	client *mongo.Client,
	note string,
	extra ...bson.E,
) (bson.D, bson.Raw, error) {
	cmd := append(
		bson.D{
			{"appendOplogNote", 1},
			{"data", bson.D{
				{"migration-verifier", note},
			}},
		},
		extra...,
	)

	resp := client.
		Database(
			"admin",
			options.Database().SetWriteConcern(writeconcern.Majority()),
		).
		RunCommand(ctx, cmd)

	raw, err := resp.Raw()

	return cmd, raw, errors.Wrapf(
		err,
		"command (%v) failed unexpectedly",
		cmd,
	)
}

func getOpTimeFromRawResponse(rawResponse bson.Raw) (primitive.Timestamp, error) {
	// Get the `operationTime` from the response and return it.
	var optime primitive.Timestamp

	found, err := mbson.RawLookup(rawResponse, &optime, opTimeKeyInServerResponse)
	if err != nil {
		return primitive.Timestamp{}, errors.Errorf("failed to read server response (%s)", rawResponse)
	}
	if !found {
		return primitive.Timestamp{}, errors.Errorf("server response (%s) lacks %#q", rawResponse, opTimeKeyInServerResponse)
	}

	return optime, nil
}
