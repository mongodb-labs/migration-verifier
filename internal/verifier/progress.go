package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/agg/accum"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/history"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/ccoveille/go-safecast/v2"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (verifier *Verifier) GetProgress(ctx context.Context) (api.Progress, error) {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	generation := verifier.generation

	var srcLastRecheckedTS, dstLastRecheckedTS option.Option[bson.Timestamp]

	verifier.srcLastRecheckedTS.Load(func(t bson.Timestamp) {
		srcLastRecheckedTS = option.IfNotZero(t)
	})
	verifier.dstLastRecheckedTS.Load(func(t bson.Timestamp) {
		dstLastRecheckedTS = option.IfNotZero(t)
	})

	var mismatch option.Option[MismatchInfo]
	var status *api.VerificationStatus

	eg, egCtx := contextplus.ErrGroup(ctx)

	// There can only be long-lived mismatches in gen1+.
	if generation > 0 {
		eg.Go(func() error {
			var err error

			mismatch, err = getLongestLivedDocumentMismatch(
				egCtx,
				verifier.metaClient.Database(verifier.metaDBName),
				generation,
			)

			return err
		})
	}

	eg.Go(func() error {
		var err error

		status, err = verifier.getVerificationStatusForGeneration(egCtx, generation)

		return err
	})

	var compareStats comparisonStatistics

	eg.Go(func() error {
		var err error

		compareStats, err = verifier.getComparisonStatistics(
			egCtx,
			generation,
			true,
		)

		return err
	})

	// Fetch and cache generation-0 stats on the first /progress call after
	// generation 0 completes. The cache is set at most once (CompareAndSwap),
	// so concurrent callers racing here are harmless.
	if generation > 0 && verifier.cachedGen0Stats.Load() == nil {
		eg.Go(func() error {
			s, err := verifier.getComparisonStatistics(egCtx, 0, false)
			if err != nil {
				return err
			}
			computed := api.ProgressGenerationStats{
				DocsCompared:     s.comparedDocs,
				TotalDocs:        s.totalDocs,
				SrcBytesCompared: s.comparedBytes,
				TotalSrcBytes:    s.totalBytes,
				TotalNamespaces:  s.totalNss,
			}
			verifier.cachedGen0Stats.CompareAndSwap(nil, &computed)
			return nil
		})
	}

	var totalRechecks types.DocumentCount
	if generation > 0 {
		eg.Go(func() error {
			var err error

			totalRechecks, err = verifier.countAllRechecks(egCtx)

			return err
		})
	}

	if err := eg.Wait(); err != nil {
		return api.Progress{Error: err}, err
	}

	var srcLag, dstLag option.Option[time.Duration]

	if times, has := verifier.srcChangeReader.getCurrentTimestamps().Get(); has {
		srcLag = option.Some(times.Lag())
	}

	if times, has := verifier.dstChangeReader.getCurrentTimestamps().Get(); has {
		dstLag = option.Some(times.Lag())
	}

	progress := api.Progress{
		Phase:      verifier.getPhaseWhileLocked(),
		Generation: verifier.generation,

		RecentRecheckSecs: mslices.Map1(
			verifier.recheckDurations.Get(),
			time.Duration.Seconds,
		),

		GenerationStats: api.ProgressGenerationStats{
			DocsCompared:     compareStats.comparedDocs,
			TotalDocs:        compareStats.totalDocs,
			SrcBytesCompared: compareStats.comparedBytes,
			TotalSrcBytes:    compareStats.totalBytes,
			TotalNamespaces:  compareStats.totalNss,
		},

		Gen0Stats: option.FromPointer(verifier.cachedGen0Stats.Load()),

		SrcLastRecheckedTS: srcLastRecheckedTS,
		DstLastRecheckedTS: dstLastRecheckedTS,

		SrcChangeStats: api.ProgressChangeStats{
			EventsPerSecond: verifier.srcChangeReader.getEventsPerSecond(),
			LagSecs: option.Map(
				srcLag,
				chain(
					time.Duration.Seconds,
					safecast.MustConvert[int, float64],
				),
			),
			BufferSaturation: verifier.srcChangeReader.getBufferSaturation(),
		},
		DstChangeStats: api.ProgressChangeStats{
			EventsPerSecond: verifier.dstChangeReader.getEventsPerSecond(),
			LagSecs: option.Map(
				dstLag,
				chain(
					time.Duration.Seconds,
					safecast.MustConvert[int, float64],
				),
			),
			BufferSaturation: verifier.dstChangeReader.getBufferSaturation(),
		},

		DocsComparedPerSecond: history.RatePer(
			verifier.docsComparedHistory.Get(),
			time.Second,
		),

		SrcBytesComparedPerSecond: history.RatePer(
			verifier.bytesComparedHistory.Get(),
			time.Second,
		),

		TotalRechecksDone: totalRechecks,

		Status: status,
	}

	if mm, has := mismatch.Get(); has {
		progress.LongestDocMismatch = option.Some(mm.Detail.APIDocMismatchInfo())
	}

	return progress, nil
}

func chain[T1, T2, T3 any](f1 func(a T1) T2, f2 func(b T2) T3) func(T1) T3 {
	return func(in T1) T3 {
		return f2(f1(in))
	}
}

func (verifier *Verifier) getPhaseWhileLocked() string {
	verifier.assertLocked()

	if !verifier.running {
		return Idle
	}

	if verifier.generation > 0 {
		return Recheck
	}

	return Check
}

func (verifier *Verifier) countAllRechecks(ctx context.Context) (types.DocumentCount, error) {
	metaDB := verifier.verificationDatabase()

	cursor, err := metaDB.Collection(verificationTasksCollection).Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$match", bson.D{
				{"generation", bson.D{{"$gt", 0}}},
				{"type", tasks.VerifyDocuments},
				{"status", bson.D{{"$in", mslices.Of(
					tasks.Completed,
					tasks.Failed,
				)}}},
			}}},
			{{"$group", bson.D{
				{"_id", nil},
				{"rechecks", accum.Sum{"$documents_count"}},
			}}},
		},
	)
	if err != nil {
		return 0, errors.Wrap(err, "counting rechecks")
	}

	var results []struct {
		Rechecks int64 `bson:"rechecks"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return 0, errors.Wrap(err, "reading count of rechecks")
	}

	switch len(results) {
	case 0:
		return 0, nil
	case 1:
	default:
		return 0, errors.Errorf("expected at most one aggregation result, got %d", len(results))
	}

	return safecast.Convert[types.DocumentCount](results[0].Rechecks)
}
