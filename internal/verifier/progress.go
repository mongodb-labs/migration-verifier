package verifier

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
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

	if generation > 0 {
		eg.Go(
			func() error {
				mismatches, changes, err := countRechecksForPriorGeneration(
					egCtx,
					verifier.metaClient.Database(verifier.metaDBName),
					generation-1,
				)

				if err != nil {
					return errors.Wrapf(err, "counting mismatches seen during generation %d", generation-1)
				}

				genStats.PriorRechecks = option.Some(ProgressRechecks{
					Changes:    changes,
					Mismatches: mismatches,
				})

				return nil
			},
		)
	}

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
	eg.Go(
		func() error {
			failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
				ctx,
				verifier.logger,
				verifier.verificationTaskCollection(),
				verificationTaskVerifyDocuments,
				generation,
			)
			if err != nil {
				return errors.Wrapf(err, "fetching generation %d’s failed & incomplete tasks", generation)
			}

			taskIDsToQuery := lo.Map(
				lo.Flatten(mslices.Of(failedTasks, incompleteTasks)),
				func(ft VerificationTask, _ int) bson.ObjectID {
					return ft.PrimaryKey
				},
			)

			mismatchCount, _, err := countMismatchesForTasks(
				egCtx,
				verifier.verificationDatabase(),
				taskIDsToQuery,
				true,
			)
			if err != nil {
				return errors.Wrapf(err, "counting mismatches seen during generation %d", generation)
			}

			genStats.MismatchesFound = mismatchCount

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
