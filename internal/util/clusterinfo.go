package util

import (
	"context"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type ClusterTopology string

type ClusterInfo struct {
	VersionArray []int
	Topology     ClusterTopology
}

const (
	TopologySharded ClusterTopology = "sharded"
	TopologyReplset ClusterTopology = "replset"
)

func GetClusterInfo(ctx context.Context, client *mongo.Client) (ClusterInfo, error) {
	va, err := getVersionArray(ctx, client)
	if err != nil {
		return ClusterInfo{}, errors.Wrap(err, "failed to fetch version array")
	}

	topology, err := getTopology(ctx, client)
	if err != nil {
		return ClusterInfo{}, errors.Wrap(err, "failed to determine topology")
	}

	return ClusterInfo{
		VersionArray: va,
		Topology:     topology,
	}, nil
}

func getVersionArray(ctx context.Context, client *mongo.Client) ([]int, error) {
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

func getTopology(ctx context.Context, client *mongo.Client) (ClusterTopology, error) {
	resp := client.Database("admin").RunCommand(
		ctx,
		bson.D{{"hello", 1}},
	)

	hello := struct {
		Msg string
	}{}

	if err := resp.Decode(&hello); err != nil {
		return "", errors.Wrapf(
			err,
			"failed to decode %#q response",
			"hello",
		)
	}

	return lo.Ternary(hello.Msg == "isdbgrid", TopologySharded, TopologyReplset), nil
}
