package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const opTimeKeyInServerResponse = "operationTime"

// GetClusterTime returns the remote cluster’s cluster time.
// In so doing it creates a new oplog entry across all shards.
// The “note” will go into the cluster’s oplog, so keep it
// short but meaningful.
func GetClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	retryer := retry.New(retry.DefaultDurationLimit)

	var optime primitive.Timestamp

	// To get the cluster time, we submit a request to append an oplog note
	// but set an unreasonably-early maxClusterTime. All the shards will fail
	// the request but still send their current clusterTime to the mongos,
	// which will return the most recent of those. Thus we can fetch the
	// most recent cluster time without altering the oplog.
	err := retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			optime, err = fetchClusterTime(ctx, client)
			return err
		},
	)

	if err != nil {
		return primitive.Timestamp{}, err
	}

	// OPTIMIZATION FOR SHARDED CLUSTERS: We append another oplog entry to
	// bring all shards to a cluster time that’s *after* the optime that we’ll
	// return. That way any events *at*
	//
	// Since this is just an optimization, failures here are nonfatal.
	err = retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			optime, err = syncClusterTimeAcrossShards(ctx, client)
			return err
		},
	)
	if err != nil {
		// This isn't serious enough even to warn on, so leave it at info-level.
		logger.Info().Err(err).
			Msg("Failed to append oplog note; change stream may need extra time to finish.")
	}

	return optime, nil
}

// Use this when we just need the correct cluster time without
// actually changing any shards’ oplogs.
func fetchClusterTime(
	ctx context.Context,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	cmd, rawResponse, err := runAppendOplogNote(
		ctx,
		client,
		"expect StaleClusterTime error",
		bson.E{"maxClusterTime", primitive.Timestamp{1, 0}},
	)

	// We expect an error here; if we didn't get one then something is
	// amiss on the server.
	if err == nil {
		return primitive.Timestamp{}, errors.Errorf("server request unexpectedly succeeded: %v", cmd)
	}

	if !util.IsStaleClusterTimeError(err) {
		return primitive.Timestamp{}, errors.Wrap(
			err,
			"unexpected error (expected StaleClusterTime) from request",
		)
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func syncClusterTimeAcrossShards(
	ctx context.Context,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	_, rawResponse, err := runAppendOplogNote(ctx, client, "syncing cluster time")

	if err != nil {
		return primitive.Timestamp{}, err
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func runAppendOplogNote(
	ctx context.Context,
	client *mongo.Client,
	note string,
	extraPieces ...bson.E,
) (bson.D, bson.Raw, error) {
	cmd := append(
		bson.D{
			{"appendOplogNote", 1},
			{"data", bson.D{
				{"migration-verifier", note},
			}},
		},
		extraPieces...,
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
