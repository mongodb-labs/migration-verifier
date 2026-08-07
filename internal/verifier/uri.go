package verifier

import (
	"context"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

func (verifier *Verifier) SetSrcURI(ctx context.Context, uri string) error {
	client, clientOpts, err := verifier.getClient(ctx, uri, verifier.readPreference)
	if err != nil {
		return errors.Wrapf(err, "connect to source")
	}

	verifier.srcClient = client

	verifier.srcURI = uri

	verifier.logger.Debug().
		Msg("Reading source’s cluster info.")

	clusterInfo, err := util.GetClusterInfo(ctx, verifier.logger, verifier.srcClient)
	if err != nil {
		return errors.Wrap(err, "failed to read source cluster info")
	}

	verifier.logger.Info().
		Any("clusterInfo", clusterInfo).
		Msg("Found source’s cluster info.")

	verifier.srcClusterInfo = &clusterInfo

	if clusterInfo.VersionArray[0] < 5 && clusterInfo.Topology == util.TopologySharded {
		err := RefreshSrcMongosInstances(
			ctx,
			verifier.logger,
			clientOpts,
		)

		if err != nil {
			return errors.Wrap(
				err,
				"failed to refresh source mongos instances",
			)
		}
	}

	if !isVersionSupported(clusterInfo.VersionArray) {
		return errors.Errorf("unsupported source version: %v", clusterInfo.VersionArray)
	}

	return checkURIAgainstServerVersion(uri, clusterInfo)
}

func isVersionSupported(version []int) bool {
	return version[0] >= 4
}

func (verifier *Verifier) SetDstURI(ctx context.Context, uri string) error {
	client, clientOpts, err := verifier.getClient(ctx, uri, verifier.readPreference)
	if err != nil {
		return errors.Wrapf(err, "connect to destination")
	}

	verifier.dstClient = client

	verifier.logger.Debug().
		Msg("Reading destination’s cluster info.")

	clusterInfo, err := util.GetClusterInfo(ctx, verifier.logger, verifier.dstClient)
	if err != nil {
		return errors.Wrap(err, "failed to read destination cluster info")
	}

	verifier.logger.Info().
		Any("clusterInfo", clusterInfo).
		Msg("Found destination’s cluster info.")

	if !isVersionSupported(clusterInfo.VersionArray) {
		return errors.Errorf("unsupported destination version: %v", clusterInfo.VersionArray)
	}

	verifier.dstClusterInfo = &clusterInfo

	if clusterInfo.VersionArray[0] < 5 && clusterInfo.Topology == util.TopologySharded {
		err := RefreshDstMongosInstances(
			ctx,
			verifier.logger,
			clientOpts,
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
