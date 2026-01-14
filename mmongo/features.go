package mmongo

import (
	"context"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func GetVersionArray(ctx context.Context, client *mongo.Client) ([]int, error) {
	commandResult := client.Database("admin").RunCommand(ctx, bson.D{{"buildinfo", 1}})

	rawResp, err := commandResult.Raw()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to run %#q", "buildinfo")
	}

	var va []int
	_, err = mbson.RawLookup(rawResp, &va, "versionArray")
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode build info version array")
	}

	return va, nil
}
