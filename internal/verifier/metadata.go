package verifier

import (
	"bytes"
	"context"
	"fmt"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// This is the Field for a VerificationResult for shard key mismatches.
const ShardKeyField = "Shard Key"

// Returns a slice of VerificationResults with the differences, and a boolean indicating whether or
// not the collection data can be safely verified.
func (verifier *Verifier) compareCollectionSpecifications(
	srcNs, dstNs string,
	srcSpecOpt, dstSpecOpt option.Option[util.CollectionSpec],
) ([]VerificationResult, bool, error) {
	srcSpec, hasSrcSpec := srcSpecOpt.Get()
	dstSpec, hasDstSpec := dstSpecOpt.Get()

	if !hasSrcSpec {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterSource,
			Details:   Missing}}, false, nil
	}
	if !hasDstSpec {
		return []VerificationResult{{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Details:   Missing}}, false, nil
	}
	if srcSpec.Type != dstSpec.Type {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterTarget,
			Field:     "Type",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Type, dstSpec.Type)}}, false, nil
		// If the types differ, the rest is not important.
	}
	var results []VerificationResult
	if srcSpec.Info.ReadOnly != dstSpec.Info.ReadOnly {
		results = append(results, VerificationResult{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Field:     "ReadOnly",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Info.ReadOnly, dstSpec.Info.ReadOnly)})
	}
	if !bytes.Equal(srcSpec.Options, dstSpec.Options) {
		mismatchDetails, err := BsonUnorderedCompareRawDocumentWithDetails(srcSpec.Options, dstSpec.Options)
		if err != nil {
			return nil, false, errors.Wrapf(
				err,
				"failed to compare namespace %#q's specifications",
				srcNs,
			)
		}
		if mismatchDetails == nil {
			results = append(results, VerificationResult{
				NameSpace: dstNs,
				Cluster:   ClusterTarget,
				Field:     "Options (Field Order Only)",
				Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Options, dstSpec.Options)})
		} else {
			results = append(results, mismatchResultsToVerificationResults(mismatchDetails, srcSpec.Options, dstSpec.Options, srcNs, nil /* id */, "Options.")...)
		}
	}

	// Don't compare view data; they have no data of their own.
	canCompareData := srcSpec.Type != "view"
	// Do not compare data between capped and uncapped collections because the partitioning is different.
	canCompareData = canCompareData && srcSpec.Options.Lookup("capped").Equal(dstSpec.Options.Lookup("capped"))

	return results, canCompareData, nil
}

func (verifier *Verifier) doIndexSpecsMatch(ctx context.Context, srcSpec, dstSpec bson.Raw) (bool, error) {
	// If the byte buffers match, then we’re done.
	if bytes.Equal(srcSpec, dstSpec) {
		return true, nil
	}

	var fieldsToRemove = []string{
		// v4.4 stopped adding “ns” to index fields.
		"ns",

		// v4.2+ ignores this field.
		"background",
	}

	return util.ServerThinksTheseMatch(
		ctx,
		verifier.metaClient,
		srcSpec,
		dstSpec,
		option.Some(mongo.Pipeline{
			{{"$unset", lo.Reduce(
				fieldsToRemove,
				func(cur []string, field string, _ int) []string {
					return append(cur, "a."+field, "b."+field)
				},
				[]string{},
			)}},
		}),
	)
}

func (verifier *Verifier) verifyShardingIfNeeded(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
) ([]VerificationResult, error) {

	// If one cluster is sharded and the other is unsharded then there's
	// nothing to do here.
	if verifier.srcClusterInfo.Topology != verifier.dstClusterInfo.Topology {
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

func (verifier *Verifier) verifyIndexes(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
	srcIdIndexSpec, dstIdIndexSpec bson.Raw,
) ([]VerificationResult, error) {

	srcMap, err := getIndexesMap(ctx, srcColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on source",
			FullName(srcColl),
		)
	}

	if srcIdIndexSpec != nil {
		srcMap["_id"] = srcIdIndexSpec
	}

	dstMap, err := getIndexesMap(ctx, dstColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on destination",
			FullName(dstColl),
		)
	}

	if dstIdIndexSpec != nil {
		dstMap["_id"] = dstIdIndexSpec
	}

	var results []VerificationResult
	srcMapUsed := map[string]bool{}

	for indexName, dstSpec := range dstMap {
		srcSpec, exists := srcMap[indexName]
		if exists {
			srcMapUsed[indexName] = true
			theyMatch, err := verifier.doIndexSpecsMatch(ctx, srcSpec, dstSpec)
			if err != nil {
				return nil, errors.Wrapf(
					err,
					"failed to check whether %#q's source & desstination %#q indexes match",
					FullName(srcColl),
					indexName,
				)
			}

			if !theyMatch {
				results = append(results, VerificationResult{
					NameSpace: FullName(dstColl),
					Cluster:   ClusterTarget,
					ID:        indexName,
					Details:   Mismatch + fmt.Sprintf(": src: %v, dst: %v", srcSpec, dstSpec),
				})
			}
		} else {
			results = append(results, VerificationResult{
				ID:        indexName,
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: FullName(srcColl),
			})
		}
	}

	// Find any index specs which existed in the source cluster but not the target cluster.
	for indexName := range srcMap {
		if !srcMapUsed[indexName] {
			results = append(results, VerificationResult{
				ID:        indexName,
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: FullName(dstColl)})
		}
	}
	return results, nil
}
