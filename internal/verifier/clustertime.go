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

// GetNewClusterTime advances the remote cluster’s cluster time an returns
// that time. In sharded clusters this advancement happens across all shards,
// which (usefully!) equalizes the shards’ cluster time and triggers them to
// output all events before then.
func GetNewClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	retryer := retry.New(retry.DefaultDurationLimit)

	var optime primitive.Timestamp

	err := retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(_ *retry.Info) error {
			var err error
			optime, err = syncClusterTimeAcrossShards(ctx, client)
			return err
		},
	)

	return optime, err
}

func syncClusterTimeAcrossShards(
	ctx context.Context,
	client *mongo.Client,
) (primitive.Timestamp, error) {
	cmd := bson.D{
		{"appendOplogNote", 1},
		{"data", bson.D{
			{"migration-verifier", "syncing cluster time"},
		}},
	}

	resp := client.
		Database(
			"admin",
			options.Database().SetWriteConcern(writeconcern.Majority()),
		).
		RunCommand(ctx, cmd)

	rawResponse, err := resp.Raw()

	if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(
			err,
			"command (%v) failed unexpectedly",
			cmd,
		)
	}

	return getOpTimeFromRawResponse(rawResponse)
}

func getOpTimeFromRawResponse(rawResponse bson.Raw) (primitive.Timestamp, error) {
	// Return the response’s `operationTime`.
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
