package verifier

import (
	"bytes"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// This is the Field for a VerificationResult for shard key mismatches.
const ShardKeyField = "shard key"

func (verifier *Verifier) verifyShardingIfNeeded(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
) ([]compare.Result, error) {
	// We only need to compare if both clusters are sharded
	srcSharded := verifier.srcClusterInfo.Topology == util.TopologySharded
	dstSharded := verifier.dstClusterInfo.Topology == util.TopologySharded

	if !srcSharded || !dstSharded {
		return nil, nil
	}

	srcShardOpt, err := GetShardKey(ctx, verifier.logger, srcColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's shard key on source",
			FullName(srcColl),
		)
	}

	dstShardOpt, err := GetShardKey(ctx, verifier.logger, dstColl)
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
		return []compare.Result{{
			Field:     ShardKeyField,
			Cluster:   lo.Ternary(srcIsSharded, constants.ClusterTarget, constants.ClusterSource),
			Details:   compare.Missing,
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
		return []compare.Result{{
			Field:     ShardKeyField,
			Details:   fmt.Sprintf("%s: src=%v; dst=%v", Mismatch, srcKey, dstKey),
			NameSpace: FullName(srcColl),
		}}, nil
	}

	return nil, nil
}

// GetShardKey returns the collection's shard key, or an empty option
// if the collection is unsharded.
func GetShardKey(
	ctx context.Context,
	logger *logger.Logger,
	coll *mongo.Collection,
) (option.Option[bson.Raw], error) {
	namespace := coll.Database().Name() + "." + coll.Name()

	configCollectionsColl := coll.Database().Client().
		Database("config").
		Collection("collections")

	decoded := struct {
		Key option.Option[bson.Raw]
	}{}

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			err := configCollectionsColl.
				FindOne(ctx, bson.D{{"_id", namespace}}).
				Decode(&decoded)

			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil
			}

			return err
		},
		"fetching %#q's shard key",
		namespace,
	).Run(ctx, logger)

	if errors.Is(err, mongo.ErrNoDocuments) {
		return option.None[bson.Raw](), nil
	} else if err != nil {
		return option.None[bson.Raw](), errors.Wrapf(
			err,
			"failed to find sharding info for %#q",
			namespace,
		)
	}

	key, hasKey := decoded.Key.Get()

	if !hasKey {
		return option.None[bson.Raw](), nil
	}

	return option.Some(key), nil
}

// DisableBalancing is used in tests.
func DisableBalancing(ctx context.Context, coll *mongo.Collection) error {
	client := coll.Database().Client()
	configDB := client.Database("config")

	ns := FullName(coll)

	_, err := configDB.
		Collection(
			"collections",
			options.Collection().SetWriteConcern(writeconcern.Majority()),
		).
		UpdateOne(
			ctx,
			bson.D{{"_id", ns}},
			bson.D{
				{"$set", bson.D{
					{"noBalance", true},
				}},
			},
		)
	if err != nil {
		return errors.Wrapf(err, "disabling %#q's shard balancing", ns)
	}
	return nil
}
