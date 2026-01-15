package util

import (
	"cmp"
	"context"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
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

// ClusterHasBSONSize indicates whether a cluster with the given
// major & minor version numbers supports the $bsonSize aggregation operator.
func ClusterHasBSONSize(va [2]int) bool {
	major := va[0]

	if major == 4 {
		return va[1] >= 4
	}

	return major > 4
}

func ClusterHasCurrentOpIdleCursors(va [2]int) bool {
	major := va[0]

	if major == 4 {
		return va[1] >= 2
	}

	return major > 4
}

var ClusterHasChangeStreamStartAfter = ClusterHasCurrentOpIdleCursors

const (
	TopologySharded ClusterTopology = "sharded"
	TopologyReplset ClusterTopology = "replset"
)

func CmpMinorVersions(a, b [2]int) int {
	return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
}

func GetClusterInfo(ctx context.Context, logger *logger.Logger, client *mongo.Client) (ClusterInfo, error) {
	va, err := mmongo.GetVersionArray(ctx, client)
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
