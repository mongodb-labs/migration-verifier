package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

func (verifier *Verifier) SetSrcURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.srcClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to source %#q", uri)
	}

	clusterInfo, err := util.GetClusterInfo(ctx, verifier.srcClient)
	if err != nil {
		return errors.Wrap(err, "failed to read source cluster info")
	}

	verifier.srcClusterInfo = &clusterInfo

	if clusterInfo.VersionArray[0] < 5 && clusterInfo.Topology == util.TopologySharded {
		err := RefreshAllMongosInstances(
			ctx,
			verifier.logger,
			opts,
		)

		if err != nil {
			return errors.Wrap(
				err,
				"failed to refresh source mongos instances",
			)
		}
	}

	return checkURIAgainstServerVersion(uri, clusterInfo)
}

func (verifier *Verifier) SetDstURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.dstClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to destination %#q", uri)
	}

	clusterInfo, err := util.GetClusterInfo(ctx, verifier.dstClient)
	if err != nil {
		return errors.Wrap(err, "failed to read destination build info")
	}

	verifier.dstClusterInfo = &clusterInfo

	if clusterInfo.VersionArray[0] < 5 && clusterInfo.Topology == util.TopologySharded {
		err := RefreshAllMongosInstances(
			ctx,
			verifier.logger,
			opts,
		)

		if err != nil {
			return errors.Wrap(
				err,
				"failed to refresh destination mongos instances",
			)
		}
	}

	return checkURIAgainstServerVersion(uri, clusterInfo)
}

func checkURIAgainstServerVersion(uri string, bi util.ClusterInfo) error {
	if bi.VersionArray[0] >= 5 {
		return nil
	}

	cs, err := connstring.ParseAndValidate(uri)

	if err != nil {
		return errors.Wrap(err, "failed to parse and validate connection string")
	}
	if cs == nil {
		panic("parsed and validated connection string (" + uri + ") must not be nil")
	}

	// migration-verifier disallows SRV strings for pre-v5 clusters for the
	// same reason as mongosync’s embedded verifier: mongoses can be added
	// dynamically, which means they could avoid the critical router-flush that
	// SERVER-32198 necessitates for pre-v5 clusters.
	if cs.Scheme == connstring.SchemeMongoDBSRV {
		return errors.Errorf(
			"SRV connection string is forbidden for pre-v5 clusters",
		)
	}

	return nil
}
