package verifier

import (
	"bytes"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// This is the Field for a VerificationResult for shard key mismatches.
const ShardKeyField = "Shard Key"

func (verifier *Verifier) verifyShardingIfNeeded(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
) ([]VerificationResult, error) {

	// We only need to compare if both clusters are sharded
	srcSharded := verifier.srcClusterInfo.Topology == util.TopologySharded
	dstSharded := verifier.dstClusterInfo.Topology == util.TopologySharded

	if !srcSharded || !dstSharded {
		return nil, nil
	}

	srcShardOpt, err := util.GetShardKey(ctx, srcColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's shard key on source",
			FullName(srcColl),
		)
	}

	dstShardOpt, err := util.GetShardKey(ctx, dstColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's shard key on destination",
			FullName(dstColl),
		)
	}

	srcKey, srcIsSharded := srcShardOpt.Get()
	dstKey, dstIsSharded := dstShardOpt.Get()

	if !srcIsSharded && !dstIsSharded {
		return nil, nil
	}

	if srcIsSharded != dstIsSharded {
		return []VerificationResult{{
			Field:     ShardKeyField,
			Cluster:   lo.Ternary(srcIsSharded, ClusterTarget, ClusterSource),
			Details:   Missing,
			NameSpace: FullName(srcColl),
		}}, nil
	}

	if bytes.Equal(srcKey, dstKey) {
		return nil, nil
	}

	areEqual, err := util.ServerThinksTheseMatch(
		ctx,
		verifier.metaClient,
		srcKey, dstKey,
		option.None[mongo.Pipeline](),
	)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to ask server if shard keys (src %v; dst: %v) match",
			srcKey,
			dstKey,
		)
	}

	if !areEqual {
		return []VerificationResult{{
			Field:     ShardKeyField,
			Details:   fmt.Sprintf("%s: src=%v; dst=%v", Mismatch, srcKey, dstKey),
			NameSpace: FullName(srcColl),
		}}, nil
	}

	return nil, nil
}
