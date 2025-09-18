package verifier

import (
	"context"
	"fmt"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/exp/slices"
)

func (verifier *Verifier) addTimeseriesBucketsToNamespaces(ctx context.Context) error {
	srcTimeseriesNamespaces, err := whichNamespacesAreTimeseries(
		ctx,
		verifier.srcClient,
		mapset.NewSet(verifier.srcNamespaces...),
	)
	if err != nil {
		return errors.Wrap(err, "fetching timeseries namespaces")
	}

	for _, srcNS := range slices.Clone(verifier.srcNamespaces) {
		if !srcTimeseriesNamespaces.Contains(srcNS) {
			continue
		}

		dstNS, ok := verifier.nsMap.GetDstNamespace(srcNS)
		if !ok {
			return fmt.Errorf("found no dst namespace for %#q", srcNS)
		}

		srcBuckets := "system.buckets." + srcNS
		dstBuckets := "system.buckets." + dstNS

		verifier.srcNamespaces = append(
			verifier.srcNamespaces,
			srcBuckets,
		)

		verifier.dstNamespaces = append(
			verifier.dstNamespaces,
			dstBuckets,
		)

		if err := verifier.nsMap.Augment(srcBuckets, dstBuckets); err != nil {
			return errors.Wrapf(
				err,
				"adding %#q -> %#q to internal namespace map",
				srcBuckets,
				dstBuckets,
			)
		}
	}

	return nil
}

func whichNamespacesAreTimeseries(
	ctx context.Context,
	client *mongo.Client,
	namespaces mapset.Set[string],
) (mapset.Set[string], error) {
	tsNamespaces := mapset.NewSet[string]()

	dbNamespaces := map[string]mapset.Set[string]{}

	for ns := range namespaces.Iter() {
		db, coll := SplitNamespace(ns)
		if set, exists := dbNamespaces[db]; exists {
			set.Add(coll)
		} else {
			dbNamespaces[db] = mapset.NewSet(coll)
		}
	}

	for db, colls := range dbNamespaces {
		specs, err := client.Database(db).ListCollectionSpecifications(
			ctx,
			bson.D{
				{"type", "timeseries"},
				{"name", bson.D{{"$in", colls.ToSlice()}}},
			},
		)

		if err != nil {
			return nil, errors.Wrapf(err, "listing %#q’s timeseries namespaces", db)
		}

		for _, spec := range specs {
			tsNamespaces.Add(db + "." + spec.Name)
		}
	}

	return tsNamespaces, nil
}
