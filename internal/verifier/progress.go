package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
)

func (verifier *Verifier) GetProgress(ctx context.Context) (Progress, error) {
	verifier.mux.RLock()
	defer verifier.mux.RUnlock()

	var vStatus *VerificationStatus

	generation := verifier.generation
	genStats := ProgressGenerationStats{}

	if !verifier.generationStartTime.IsZero() {
		progressTime := time.Now()
		genElapsed := progressTime.Sub(verifier.generationStartTime)

		genStats.TimeElapsed = option.Some(genElapsed.Round(time.Millisecond).String())
	}

	eg, egCtx := contextplus.ErrGroup(ctx)
	eg.Go(
		func() error {
			var err error
			vStatus, err = verifier.getVerificationStatusForGeneration(egCtx, generation)

			return errors.Wrapf(err, "fetching generation %d’s tasks’ status", generation)
		},
	)

	eg.Go(
		func() error {
			recheckStats, err := countRechecksForGeneration(
				egCtx,
				verifier.metaClient.Database(verifier.metaDBName),
				generation,
			)

			if err != nil {
				return errors.Wrapf(err, "counting mismatches seen during generation %d", generation)
			}

			if generation > 0 {
				genStats.CurrentGenerationRechecks = option.Some(ProgressRechecks{
					Changes:    recheckStats.FromChange,
					Mismatches: recheckStats.FromMismatch,
					Total:      recheckStats.Total,
				})
			}

			genStats.MismatchesFound = recheckStats.NewMismatches
			genStats.MaxMismatchDuration = option.Map(
				recheckStats.MaxMismatchDuration,
				time.Duration.String,
			)

			return nil
		},
	)

	eg.Go(
		func() error {
			enqueuedRecheckCounts, err := verifier.countEnqueuedRechecksWhileLocked(ctx)

			if err != nil {
				return errors.Wrap(err, "analyzing rechecks enqueued for next generation")
			}

			genStats.NextGenerationRechecks = ProgressRechecks{
				Changes:    enqueuedRecheckCounts.Changed,
				Mismatches: enqueuedRecheckCounts.Mismatched,
				Total:      enqueuedRecheckCounts.Changed + enqueuedRecheckCounts.Mismatched - enqueuedRecheckCounts.ChangedAndMismatched,
			}

			return nil
		},
	)

	eg.Go(
		func() error {
			var err error
			nsStats, err := verifier.GetPersistedNamespaceStatisticsForGeneration(ctx, generation)

			if err != nil {
				return errors.Wrapf(err, "fetching generation %d’s persisted namespace stats", generation)
			}

			var totalDocs, comparedDocs types.DocumentCount
			var totalBytes, comparedBytes types.ByteCount
			var totalNss, completedNss types.NamespaceCount

			for _, result := range nsStats {
				totalDocs += result.TotalDocs
				comparedDocs += result.DocsCompared
				totalBytes += result.TotalBytes
				comparedBytes += result.BytesCompared

				totalNss++
				if result.PartitionsDone > 0 {
					partitionsPending := result.PartitionsAdded + result.PartitionsProcessing
					if partitionsPending == 0 {
						completedNss++
					}
				}
			}

			var activeWorkers int
			perNamespaceWorkerStats := verifier.getPerNamespaceWorkerStats()
			for _, nsWorkerStats := range perNamespaceWorkerStats {
				for _, workerStats := range nsWorkerStats {
					activeWorkers++
					comparedDocs += workerStats.SrcDocCount
					comparedBytes += workerStats.SrcByteCount
				}
			}

			genStats.DocsCompared = comparedDocs
			genStats.TotalDocs = totalDocs

			genStats.SrcBytesCompared = comparedBytes
			genStats.TotalSrcBytes = totalBytes

			genStats.ActiveWorkers = activeWorkers

			return nil
		},
	)

	if err := eg.Wait(); err != nil {
		return Progress{Error: err}, err
	}

	return Progress{
		Phase: lo.Ternary(
			verifier.running,
			lo.Ternary(generation > 0, Recheck, Check),
			Idle,
		),
		Generation:      verifier.generation,
		GenerationStats: genStats,
		SrcChangeStats: ProgressChangeStats{
			EventsPerSecond:  verifier.srcChangeReader.getEventsPerSecond(),
			CurrentTimes:     verifier.srcChangeReader.getCurrentTimes(),
			BufferSaturation: verifier.srcChangeReader.getBufferSaturation(),
		},
		DstChangeStats: ProgressChangeStats{
			EventsPerSecond:  verifier.dstChangeReader.getEventsPerSecond(),
			CurrentTimes:     verifier.dstChangeReader.getCurrentTimes(),
			BufferSaturation: verifier.dstChangeReader.getBufferSaturation(),
		},
		Status: vStatus,
	}, nil

}
