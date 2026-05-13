package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/sync/errgroup"
)

// CosmosDB change streams do not emit delete events on the source side.
// startCosmosDeleteReconciler launches a goroutine that periodically scans
// destination _ids and, for each one not present on the source, enqueues
// a recheck. The existing recheck flow will detect "missing on src, exists
// on dst" as a mismatch — the same shape it would have if a delete event
// had been observed.
//
// No-op unless srcType is cosmosdb and the configured sweep interval is
// positive.

const deleteReconcilerIDBatchSize = 1000

func (verifier *Verifier) startCosmosDeleteReconciler(
	ctx context.Context,
	eg *errgroup.Group,
) {
	if !verifier.IsSrcCosmosDB() {
		return
	}
	if verifier.cosmosDeleteSweepInterval <= 0 {
		verifier.logger.Info().Msg("CosmosDB delete reconciler disabled (interval is zero).")
		return
	}

	eg.Go(func() error {
		return verifier.runCosmosDeleteReconciler(ctx)
	})
}

func (verifier *Verifier) runCosmosDeleteReconciler(ctx context.Context) error {
	interval := verifier.cosmosDeleteSweepInterval
	verifier.logger.Info().
		Dur("interval", interval).
		Msg("CosmosDB delete reconciler started.")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := verifier.runCosmosDeleteSweep(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				verifier.logger.Error().Err(err).Msg("CosmosDB delete sweep failed; will retry on next tick.")
			}
		}
	}
}

func (verifier *Verifier) runCosmosDeleteSweep(ctx context.Context) error {
	verifier.mux.RLock()
	srcNamespaces := append([]string(nil), verifier.srcNamespaces...)
	verifier.mux.RUnlock()

	started := time.Now()
	var totalMissing int

	for _, srcNs := range srcNamespaces {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		dstNs, ok := verifier.nsMap.GetDstNamespace(srcNs)
		if !ok {
			dstNs = srcNs
		}

		missing, err := verifier.reconcileDeletedIDsForNamespace(ctx, srcNs, dstNs)
		if err != nil {
			return errors.Wrapf(err, "reconciling deletes for %#q", srcNs)
		}
		totalMissing += missing
	}

	verifier.logger.Info().
		Int("namespaces", len(srcNamespaces)).
		Int("missingOnSrc", totalMissing).
		Stringer("elapsed", time.Since(started)).
		Msg("CosmosDB delete sweep finished.")
	return nil
}

func (verifier *Verifier) reconcileDeletedIDsForNamespace(
	ctx context.Context,
	srcNs string,
	dstNs string,
) (int, error) {
	srcDB, srcColl := mmongo.SplitNamespace(srcNs)
	dstDB, dstColl := mmongo.SplitNamespace(dstNs)

	dstCollection := verifier.dstClient.Database(dstDB).Collection(dstColl)
	srcCollection := verifier.srcClient.Database(srcDB).Collection(srcColl)

	cursor, err := dstCollection.Find(
		ctx,
		bson.D{},
		options.Find().
			SetProjection(bson.D{{"_id", 1}}).
			SetSort(bson.D{{"_id", 1}}),
	)
	if err != nil {
		return 0, errors.Wrapf(err, "scanning destination ids for %#q", dstNs)
	}
	defer cursor.Close(ctx)

	var totalMissing int
	batch := make([]bson.RawValue, 0, deleteReconcilerIDBatchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		missing, err := findIDsMissingOnSource(ctx, srcCollection, batch)
		if err != nil {
			return err
		}

		if len(missing) > 0 {
			err := verifier.enqueueDeleteCandidateRechecks(ctx, srcDB, srcColl, missing)
			if err != nil {
				return errors.Wrapf(err, "enqueuing %d delete rechecks for %#q", len(missing), srcNs)
			}
			totalMissing += len(missing)
		}

		batch = batch[:0]
		return nil
	}

	for cursor.Next(ctx) {
		raw, err := cursor.Current.LookupErr("_id")
		if err != nil {
			return totalMissing, errors.Wrap(err, "extracting destination _id")
		}
		// cursor.Current bytes may be reused on the next iteration, so copy.
		copied := make([]byte, len(raw.Value))
		copy(copied, raw.Value)
		batch = append(batch, bson.RawValue{Type: raw.Type, Value: copied})

		if len(batch) >= deleteReconcilerIDBatchSize {
			if err := flush(); err != nil {
				return totalMissing, err
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return totalMissing, errors.Wrap(err, "iterating destination ids")
	}
	if err := flush(); err != nil {
		return totalMissing, err
	}

	return totalMissing, nil
}

// findIDsMissingOnSource queries the source for the given _ids and returns
// the subset that are NOT present.
func findIDsMissingOnSource(
	ctx context.Context,
	srcCollection *mongo.Collection,
	ids []bson.RawValue,
) ([]bson.RawValue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	inList := make(bson.A, len(ids))
	for i, id := range ids {
		inList[i] = id
	}

	srcCursor, err := srcCollection.Find(
		ctx,
		bson.D{{"_id", bson.D{{"$in", inList}}}},
		options.Find().SetProjection(bson.D{{"_id", 1}}),
	)
	if err != nil {
		return nil, errors.Wrap(err, "querying source _ids")
	}
	defer srcCursor.Close(ctx)

	present := make(map[string]struct{}, len(ids))
	for srcCursor.Next(ctx) {
		raw, err := srcCursor.Current.LookupErr("_id")
		if err != nil {
			return nil, errors.Wrap(err, "extracting source _id")
		}
		present[rawValueKey(raw)] = struct{}{}
	}
	if err := srcCursor.Err(); err != nil {
		return nil, errors.Wrap(err, "iterating source _ids")
	}

	missing := make([]bson.RawValue, 0)
	for _, id := range ids {
		if _, ok := present[rawValueKey(id)]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// rawValueKey builds a comparison key from a bson.RawValue that includes
// both the type byte and the encoded bytes. Including the type ensures e.g.
// int32(1) and int64(1) compare as different keys, which matches MongoDB's
// _id equality semantics.
func rawValueKey(rv bson.RawValue) string {
	buf := make([]byte, 0, len(rv.Value)+1)
	buf = append(buf, byte(rv.Type))
	buf = append(buf, rv.Value...)
	return string(buf)
}

func (verifier *Verifier) enqueueDeleteCandidateRechecks(
	ctx context.Context,
	srcDB string,
	srcCollection string,
	ids []bson.RawValue,
) error {
	if len(ids) == 0 {
		return nil
	}

	dbNames := make([]string, len(ids))
	collNames := make([]string, len(ids))
	dataSizes := make([]int32, len(ids))
	nowMs := bson.DateTime(time.Now().UnixMilli())
	firstMismatchTimes := make([]bson.DateTime, len(ids))
	for i := range ids {
		dbNames[i] = srcDB
		collNames[i] = srcCollection
		dataSizes[i] = int32(defaultUserDocumentSize)
		firstMismatchTimes[i] = nowMs
	}
	// Treat these like compare-mismatches (no change-stream origin). The
	// existing recheck infrastructure will re-fetch each _id from both
	// clusters and report any persistent discrepancy.
	return verifier.insertRecheckDocs(
		ctx,
		dbNames,
		collNames,
		ids,
		dataSizes,
		firstMismatchTimes,
		option.None[whichCluster](),
		nil,
	)
}
