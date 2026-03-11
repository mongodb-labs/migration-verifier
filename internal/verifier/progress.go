package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/history"
	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/10gen/migration-verifier/internal/verifier/constants"
	"github.com/10gen/migration-verifier/option"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
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

		compareStats, err = verifier.getComparisonStatistics(egCtx, generation)

		return err
	})

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

		GenerationStats: api.ProgressGenerationStats{
			DocsCompared:     compareStats.comparedDocs,
			TotalDocs:        compareStats.totalDocs,
			SrcBytesCompared: compareStats.comparedBytes,
			TotalSrcBytes:    compareStats.totalBytes,
		},

		SrcLastRecheckedTS: srcLastRecheckedTS,
		DstLastRecheckedTS: dstLastRecheckedTS,

		SrcChangeStats: api.ProgressChangeStats{
			EventsPerSecond:  verifier.srcChangeReader.getEventsPerSecond(),
			LagSecs:          option.Map(srcLag, time.Duration.Seconds),
			BufferSaturation: verifier.srcChangeReader.getBufferSaturation(),
		},
		DstChangeStats: api.ProgressChangeStats{
			EventsPerSecond:  verifier.dstChangeReader.getEventsPerSecond(),
			LagSecs:          option.Map(dstLag, time.Duration.Seconds),
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

		RecentRecheckDurations: verifier.recheckDurations.Get(),

		Status: status,
	}

	if mm, has := mismatch.Get(); has {
		mismatchType := lo.Ternary(
			mm.Detail.DocumentIsMissing(),
			lo.Ternary(
				mm.Detail.Cluster == constants.ClusterSource,
				"extraOnDst",
				"missingOnDst",
			),
			"contentMismatch",
		)

		progress.LongestMismatch = option.Some(api.ProgressMismatch{
			ID:              mm.Detail.ID,
			Namespace:       mm.Detail.NameSpace,
			DurationSeconds: mm.Detail.MismatchDuration().Seconds(),
			Detail:          mm.Detail.Details,
			Type:            mismatchType,
		})
	}

	return progress, nil
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
