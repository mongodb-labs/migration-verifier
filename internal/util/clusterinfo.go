package util

import (
	"cmp"
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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

func CmpMinorVersions(a, b [2]int) int {
	return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
}

func GetClusterInfo(ctx context.Context, logger *logger.Logger, client *mongo.Client) (ClusterInfo, error) {
	va, err := getVersionArray(ctx, client)
	if err != nil {
		return ClusterInfo{}, errors.Wrap(err, "failed to fetch version array")
	}

	topology, err := getTopology(ctx, "hello", client)
	if err != nil {
		logger.Info().
			Err(err).
			Msgf("Failed to learn topology via %#q; falling back to %#q.", "hello", "isMaster")

		topology, err = getTopology(ctx, "isMaster", client)
		if err != nil {
			return ClusterInfo{}, errors.Wrapf(err, "failed to learn topology via %#q", "isMaster")
		}
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

func getTopology(ctx context.Context, cmdName string, client *mongo.Client) (ClusterTopology, error) {

	resp := client.Database("admin").RunCommand(
		ctx,
		bson.D{{cmdName, 1}},
	)

	raw, err := resp.Raw()
	if err != nil {
		return "", errors.Wrapf(err, "failed learn topology via %#q", cmdName)
	}

	hasMsg, err := mbson.RawContains(raw, "msg")
	if err != nil {
		return "", errors.Wrapf(err, "failed to check for %#q in %#q response (%v)", "msg", cmdName, raw)
	}

	return lo.Ternary(hasMsg, TopologySharded, TopologyReplset), nil
}
