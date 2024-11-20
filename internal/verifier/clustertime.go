package verifier

import (
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

const opTimeKeyInServerResponse = "operationTime"

// GetClusterTime returns the remote cluster’s cluster time.
// In so doing it creates a new oplog entry across all shards.
// The “note” will go into the cluster’s oplog, so keep it
// short but meaningful.
func GetClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client, // could change to type=any if we need
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
			optime, err = runAppendOplogNote(ctx, client, true)
			if err != nil {
				err = fmt.Errorf("%w: %w", retry.CustomTransientErr, err)
			}
			return err
		},
	)

	if err != nil {
		return primitive.Timestamp{}, err
	}

	// OPTIMIZATION: We now append an oplog entry--this time for real!--to
	// cause any lagging shards to output their events.
	//
	// Since this is just an optimization, failures here are nonfatal.
	err = retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			optime, err = runAppendOplogNote(ctx, client, false)
			if err != nil {
				err = fmt.Errorf("%w: %w", retry.CustomTransientErr, err)
			}
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

func runAppendOplogNote(
	ctx context.Context,
	client *mongo.Client,
	doomed bool,
) (primitive.Timestamp, error) {
	// We don’t need to write to any shards’ oplogs; we just
	// need to fetch
	cmd := bson.D{
		{"appendOplogNote", 1},
		{"data", bson.D{
			{"migration-verifier", "expect StaleClusterTime error"},
		}},
	}

	if doomed {
		cmd = append(cmd, bson.E{"maxClusterTime", primitive.Timestamp{1, 0}})
	}

	resp := client.Database("admin").RunCommand(ctx, cmd)

	err := resp.Err()

	if doomed {
		// We expect an error here; if we didn't get one then something is
		// amiss on the server.
		if err == nil {
			return primitive.Timestamp{}, errors.Errorf("server request unexpectedly succeeded: %v", cmd)
		}

		if !util.IsStaleClusterTimeError(err) {
			return primitive.Timestamp{}, errors.Errorf(
				"unexpected error (expected StaleClusterTime) from request (%v): %v",
				err,
				cmd,
			)
		}
	} else if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(
			err,
			"command (%v) failed unexpectedly",
			cmd,
		)
	}

	rawResponse, _ := resp.Raw()

	return getOpTimeFromRawResponse(rawResponse)
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
