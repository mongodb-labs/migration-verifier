package util

import (
	"cmp"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

type ClusterTopology string

// ClusterFlavor distinguishes real MongoDB from MongoDB-API-compatible
// services like Azure CosmosDB. It is set by callers (the verifier looks
// at its srcType setting) rather than auto-detected, because CosmosDB
// servers don’t reliably advertise themselves through the wire protocol.
type ClusterFlavor string

const (
	// ClusterFlavorMongoDB means a real MongoDB cluster.
	ClusterFlavorMongoDB ClusterFlavor = "mongodb"

	// ClusterFlavorCosmosDB means Azure CosmosDB’s MongoDB-compatible API.
	ClusterFlavorCosmosDB ClusterFlavor = "cosmosdb"
)

type ClusterInfo struct {
	VersionArray []int
	Topology     ClusterTopology

	// Flavor distinguishes MongoDB vs MongoDB-compatible-API services like
	// CosmosDB. Empty is treated equivalent to MongoDB.
	Flavor ClusterFlavor
}

// IsCosmosDB reports whether this cluster is CosmosDB’s MongoDB-compatible API.
func (ci ClusterInfo) IsCosmosDB() bool {
	return ci.Flavor == ClusterFlavorCosmosDB
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
	TopologySharded    ClusterTopology = "sharded"
	TopologyReplset    ClusterTopology = "replset"
	TopologyStandalone ClusterTopology = "standalone"
)

func CmpMinorVersions(a, b [2]int) int {
	return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
}

func GetClusterInfo(ctx context.Context, logger *logger.Logger, client *mongo.Client) (ClusterInfo, error) {
	return GetClusterInfoForFlavor(ctx, logger, client, ClusterFlavorMongoDB)
}

// GetClusterInfoForFlavor is like GetClusterInfo but lets the caller declare
// the cluster’s flavor. For non-MongoDB flavors (e.g. CosmosDB) it relaxes
// the checks that are specific to real MongoDB.
func GetClusterInfoForFlavor(
	ctx context.Context,
	logger *logger.Logger,
	client *mongo.Client,
	flavor ClusterFlavor,
) (ClusterInfo, error) {
	if flavor == "" {
		flavor = ClusterFlavorMongoDB
	}

	va, err := mmongo.GetVersionArray(ctx, client)
	if err != nil {
		if flavor == ClusterFlavorCosmosDB {
			// CosmosDB’s buildInfo isn’t guaranteed to have a versionArray.
			// Treat as a high modern version so feature gates aimed at real
			// MongoDB don’t flag this as ancient.
			logger.Warn().Err(err).Msg("CosmosDB source has no versionArray in buildInfo response; assuming a recent-ish version.")
			va = [3]int{8, 0, 0}
		} else {
			return ClusterInfo{}, errors.Wrap(err, "failed to fetch version array")
		}
	}

	topology, err := getTopology(ctx, client, flavor)
	if err != nil {
		return ClusterInfo{}, errors.Wrapf(err, "failed to learn topology")
	}

	return ClusterInfo{
		VersionArray: va[:],
		Topology:     topology,
		Flavor:       flavor,
	}, nil
}

func getTopology(ctx context.Context, client *mongo.Client, flavor ClusterFlavor) (ClusterTopology, error) {
	// The topology won’t vary amongst the nodes.
	raw, err := getHelloRawForFlavor(ctx, client, option.None[*readpref.ReadPref](), flavor)
	if err != nil {
		return "", errors.Wrapf(err, "failed learn topology")
	}

	hasMsg, err := mbson.RawContains(raw, "msg")
	if err != nil {
		return "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "msg", raw)
	}

	if hasMsg {
		return TopologySharded, nil
	}

	hasMe, err := mbson.RawContains(raw, "me")
	if err != nil {
		return "", errors.Wrapf(err, "failed to check for %#q in hello response (%v)", "me", raw)
	}

	return lo.Ternary(hasMe, TopologyReplset, TopologyStandalone), nil
}

// GetHelloRaw returns the result of a `hello` (or, if needed,
// `isMaster`) command.
func GetHelloRaw(
	ctx context.Context,
	client *mongo.Client,
	readPref option.Option[*readpref.ReadPref],
) (bson.Raw, error) {
	return getHelloRawForFlavor(ctx, client, readPref, ClusterFlavorMongoDB)
}

func getHelloRawForFlavor(
	ctx context.Context,
	client *mongo.Client,
	readPref option.Option[*readpref.ReadPref],
	flavor ClusterFlavor,
) (bson.Raw, error) {
	opts := options.RunCmd()
	if rp, has := readPref.Get(); has {
		opts = opts.SetReadPreference(rp)
	}

	resp := client.Database("admin").RunCommand(
		ctx,
		bson.D{{"hello", 1}},
		opts,
	)

	if resp.Err() != nil {
		resp = client.Database("admin").RunCommand(
			ctx,
			bson.D{{"isMaster", 1}},
			opts,
		)
	}

	raw, err := resp.Raw()

	// The operationTime presence check defends against a specific MongoDB
	// election bug (SERVER-52654). CosmosDB’s hello response may legitimately
	// omit operationTime, so skip the check for non-MongoDB flavors.
	if err == nil && flavor != ClusterFlavorCosmosDB && !raw.Lookup("me").IsZero() {
		const opTimeName = "operationTime"
		_, err := raw.LookupErr(opTimeName)
		if err != nil {
			return nil, fmt.Errorf("server response lacks %#q; force an election, then retry", opTimeName)
		}
	}

	return resp.Raw()
}
