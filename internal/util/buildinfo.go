package util

import (
	"context"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type BuildInfo struct {
	VersionArray []int
	IsSharded    bool
}

func GetBuildInfo(ctx context.Context, client *mongo.Client) (BuildInfo, error) {
	commandResult := client.Database("admin").RunCommand(ctx, bson.D{{"buildinfo", 1}})

	rawResp, err := commandResult.Raw()
	if err != nil {
		return BuildInfo{}, errors.Wrap(err, "failed to fetch build info")
	}

	bi := BuildInfo{}
	_, err = mbson.RawLookup(rawResp, &bi.VersionArray, "versionArray")
	if err != nil {
		return BuildInfo{}, errors.Wrap(err, "failed to decode build info version array")
	}

	var msg string
	_, err = mbson.RawLookup(rawResp, &msg, "msg")
	bi.IsSharded = msg == "isdbgrid"

	return bi, nil
}
