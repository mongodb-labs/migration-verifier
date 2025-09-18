package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type GenerationStatus string

const (
	Gen0MetadataAnalysisComplete GenerationStatus = "gen0_metadata_analysis_complete"
	GenerationInProgress         GenerationStatus = "inprogress"
	GenerationComplete           GenerationStatus = "complete"

	findTaskTimeWarnThreshold = 5 * time.Second
)

var failedStatuses = mapset.NewSet(
	verificationTaskFailed,
	verificationTaskMetadataMismatch,
)

// Check is the asynchronous entry point to Check, should only be called by the web server. Use
// CheckDriver directly for synchronous run.
// testChan is a pair of channels for coordinating generations in tests.
// testChan[0] is a channel signalled when when a generation is complete
// testChan[1] is a channel signalled when Check should continue with the next generation.
func (verifier *Verifier) Check(ctx context.Context, filter bson.D) {
	go func() {
		err := verifier.CheckDriver(ctx, filter)
		if err != nil {
			verifier.logger.Fatal().
				Int("generation", verifier.generation).
				Err(err).
				Msg("Fatal error.")
		}
	}()
	verifier.MaybeStartPeriodicHeapProfileCollection(ctx)
}

func (verifier *Verifier) waitForChangeStream(ctx context.Context, csr *ChangeStreamReader) error {
	select {
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	case <-csr.readerError.Ready():
		err := csr.readerError.Get()
		verifier.logger.Warn().Err(err).
			Msgf("Received error from %s.", csr)
		return err
	case <-csr.doneChan:
		verifier.logger.Debug().
			Msgf("Received completion signal from %s.", csr)
		break
	}

	return nil
}

func (verifier *Verifier) CheckWorker(ctxIn context.Context) error {
	generation := verifier.generation

	verifier.logger.Debug().
		Int("generation", generation).
		Int("workersCount", verifier.numWorkers).
		Msg("Starting verification worker threads.")

	// Since we do a progress report right at the start we don’t need
	// this to go in non-debug output.
	startLabel := fmt.Sprintf("Starting check generation %d", generation)
	verifier.logger.Debug().Msg(startLabel)

	genStartReport := startReport()
	genStartReport.WriteString(startLabel + "\n\n")
	genStartReport.WriteString("Gathering collection metadata …\n")

	verifier.writeStringBuilder(genStartReport)

	cancelableCtx, canceler := contextplus.WithCancelCause(ctxIn)
	eg, ctx := contextplus.ErrGroup(cancelableCtx)

	// If the change stream fails, everything should stop.
	eg.Go(func() error {
		select {
		case <-verifier.srcChangeStreamReader.readerError.Ready():
			err := verifier.srcChangeStreamReader.readerError.Get()
			return errors.Wrapf(err, "%s failed", verifier.srcChangeStreamReader)
		case <-verifier.dstChangeStreamReader.readerError.Ready():
			err := verifier.dstChangeStreamReader.readerError.Get()
			return errors.Wrapf(err, "%s failed", verifier.dstChangeStreamReader)
		case <-ctx.Done():
			return nil
		}
	})

	// Start the worker threads.
	for i := 0; i < verifier.numWorkers; i++ {
		eg.Go(func() error {
			return errors.Wrapf(
				verifier.work(ctx, i),
				"worker %d failed",
				i,
			)
		})
		time.Sleep(10 * time.Millisecond)
	}

	waitForTaskCreation := 0

	var finishedAllTasks bool

	go func() {
		delay := 30 * time.Second

		time.Sleep(delay)

		for cancelableCtx.Err() == nil {
			verifier.PrintVerificationSummary(cancelableCtx, GenerationInProgress)

			time.Sleep(delay)
		}
	}()

	eg.Go(func() error {
		for {
			verificationStatus, err := verifier.GetVerificationStatus(ctx)
			if err != nil {
				return errors.Wrapf(
					err,
					"failed to retrieve status of generation %d's tasks",
					generation,
				)
			}

			verifier.logger.Debug().
				Any("taskCountsByStatus", verificationStatus).
				Send()

			// The generation continues as long as >=1 task for this generation is
			// “added” or “pending”.
			if verificationStatus.AddedTasks > 0 || verificationStatus.ProcessingTasks > 0 {
				waitForTaskCreation++

				time.Sleep(verifier.verificationStatusCheckInterval)
			} else {
				finishedAllTasks = true
				verifier.PrintVerificationSummary(ctx, GenerationComplete)

				canceler(errors.Errorf("generation %d finished", generation))
				return nil
			}
		}
	})

	err := eg.Wait()

	if finishedAllTasks && errors.Is(err, context.Canceled) {
		err = nil
	}

	if err == nil {
		verifier.logger.Debug().
			Int("generation", generation).
			Msg("Check finished.")
	}

	return errors.Wrapf(
		err,
		"check generation %d failed",
		generation,
	)
}

func (verifier *Verifier) CheckDriver(ctx context.Context, filter bson.D, testChan ...chan struct{}) error {
	verifier.mux.Lock()
	if verifier.running {
		verifier.mux.Unlock()
		verifier.logger.Info().Msg("Verifier already checking the collections")
		return nil
	}
	verifier.running = true
	verifier.globalFilter = filter

	defer func() {
		verifier.mux.Lock()
		verifier.running = false
		verifier.mux.Unlock()
	}()
	var err error
	if verifier.startClean {
		verifier.logger.Info().Msg("Dropping old verifier metadata")
		err = verifier.verificationDatabase().Drop(ctx)
		if err != nil {
			verifier.mux.Unlock()
			return errors.Wrap(err, "dropping metadata")
		}
	} else {
		genOpt, err := verifier.readGeneration(ctx)
		if err != nil {
			verifier.mux.Unlock()
			return errors.Wrap(err, "reading generation from metadata")
		}

		if gen, has := genOpt.Get(); has {
			verifier.generation = gen
			verifier.logger.Info().
				Int("generation", verifier.generation).
				Msg("Resuming in-progress verification.")
		} else {
			verifier.logger.Info().Msg("Starting new verification.")
		}
	}

	verifier.logger.Info().Msg("Starting change streams.")

	// Now that we’ve initialized verifier.generation we can
	// start the change stream readers.
	verifier.initializeChangeStreamReaders()
	verifier.mux.Unlock()

	err = retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			err = verifier.AddMetaIndexes(ctx)
			if err != nil {
				return errors.Wrap(err, "adding metadata indexes")
			}

			err = verifier.doInMetaTransaction(
				ctx,
				func(ctx context.Context, sCtx mongo.SessionContext) error {
					return verifier.ResetInProgressTasks(sCtx)
				},
			)
			if err != nil {
				return errors.Wrap(err, "failed to reset any in-progress tasks")
			}

			return nil
		},
		"setting up verifier metadata",
	).Run(ctx, verifier.logger)

	if err != nil {
		return err
	}

	verifier.logger.Debug().Msg("Starting Check")

	verifier.phase = Check
	defer func() {
		verifier.phase = Idle
	}()

	ceHandlerGroup, groupCtx := contextplus.ErrGroup(ctx)
	for _, csReader := range []*ChangeStreamReader{verifier.srcChangeStreamReader, verifier.dstChangeStreamReader} {
		if csReader.changeStreamRunning {
			verifier.logger.Debug().Msgf("Check: %s already running.", csReader)
		} else {
			verifier.logger.Debug().Msgf("%s not running; starting change stream", csReader)

			err = csReader.StartChangeStream(ctx)
			if err != nil {
				return errors.Wrapf(err, "failed to start %s", csReader)
			}
			ceHandlerGroup.Go(func() error {
				return verifier.RunChangeEventHandler(groupCtx, csReader)
			})
		}
	}

	// Log the verification status when initially booting up so it's easy to see the current state
	verificationStatus, err := verifier.GetVerificationStatus(ctx)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to retrieve verification status",
		)
	} else {
		verifier.logger.Debug().
			Any("status", verificationStatus).
			Msg("Initial verification phase.")
	}

	err = verifier.CreateInitialTasksIfNeeded(ctx)
	if err != nil {
		return err
	}
	// Now enter the multi-generational steady check state
	for {
		verifier.mux.Lock()
		err = retry.New().WithCallback(
			func(ctx context.Context, _ *retry.FuncInfo) error {
				return verifier.persistGenerationWhileLocked(ctx)
			},
			"persisting generation (%d)",
			verifier.generation,
		).Run(ctx, verifier.logger)
		if err != nil {
			verifier.mux.Unlock()
			return errors.Wrapf(err, "failed to persist generation (%d)", verifier.generation)
		}
		verifier.mux.Unlock()

		verifier.generationStartTime = time.Now()
		verifier.srcEventRecorder.Reset()
		verifier.dstEventRecorder.Reset()

		err := verifier.CheckWorker(ctx)
		if err != nil {
			return err
		}
		// we will only coordinate when the number of channels is exactly 2.
		// * Channel 0 informs the test of a generation bounary.
		// * Block until the test (via channel 1) tells us to do the
		//   next generation.
		if len(testChan) == 2 {

			verifier.logger.Debug().
				Msg("Telling test about generation boundary.")
			testChan[0] <- struct{}{}

			verifier.logger.Debug().
				Msg("Awaiting test's signal to continue.")
			<-testChan[1]

			verifier.logger.Debug().
				Msg("Received test's signal. Continuing.")
		}
		time.Sleep(verifier.generationPauseDelay)
		verifier.mux.Lock()
		if verifier.lastGeneration {
			verifier.mux.Unlock()

			verifier.logger.Debug().
				Int("generation", verifier.generation).
				Msg("Final generation done.")

			return nil
		}
		// TODO: wait here until writesOff is hit or enough time has passed, so we don't spin
		// doing empty rechecks.

		// possible issue: turning the writes off at the exact same time a new iteration starts
		// will result in an extra iteration. The odds of this are lower and the user should be
		// paying attention. Also, this should not matter too much because any failures will be
		// caught again on the next iteration.
		if verifier.writesOff {
			verifier.logger.Debug().
				Msg("Waiting for change streams to end.")

			// It's necessary to wait for the change stream to finish before incrementing the
			// generation number, or the last changes will not be checked.
			verifier.mux.Unlock()

			for _, csr := range mslices.Of(verifier.srcChangeStreamReader, verifier.dstChangeStreamReader) {
				if err = verifier.waitForChangeStream(ctx, csr); err != nil {
					return errors.Wrapf(
						err,
						"an error interrupted the wait for closure of %s",
						csr,
					)
				}

				verifier.logger.Debug().
					Stringer("changeStreamReader", csr).
					Msg("Change stream reader finished.")
			}

			if err = ceHandlerGroup.Wait(); err != nil {
				return err
			}
			verifier.mux.Lock()
			verifier.lastGeneration = true
		}
		verifier.generation++
		verifier.phase = Recheck

		// Generation of recheck tasks can partial-fail. The following will
		// cause a full redo in that case, which is inefficient but simple.
		// Such failures seem unlikely anyhow.
		err = retry.New().WithCallback(
			func(ctx context.Context, _ *retry.FuncInfo) error {
				return verifier.GenerateRecheckTasksWhileLocked(ctx)
			},
			"generating recheck tasks",
		).Run(ctx, verifier.logger)
		if err != nil {
			verifier.mux.Unlock()
			return err
		}

		err = verifier.DropOldRecheckQueueWhileLocked(ctx)
		if err != nil {
			verifier.logger.Warn().
				Err(err).
				Msg("Failed to clear out old recheck docs. (This is probably unimportant.)")
		}
		verifier.mux.Unlock()
	}
}

func (verifier *Verifier) setupAllNamespaceList(ctx context.Context) error {
	// We want to check all user collections on both source and dest.
	srcNamespaces, err := ListAllUserNamespaces(ctx, verifier.logger, verifier.srcClient, verifier.metaDBName)
	if err != nil {
		return errors.Wrap(err, "failed to list source collections")
	}

	dstNamespaces, err := ListAllUserNamespaces(ctx, verifier.logger, verifier.dstClient, verifier.metaDBName)
	if err != nil {
		return errors.Wrap(err, "failed to list destination collections")
	}

	srcMap := map[string]bool{}
	for _, ns := range srcNamespaces {
		srcMap[ns] = true
	}
	for _, ns := range dstNamespaces {
		if !srcMap[ns] {
			srcNamespaces = append(srcNamespaces, ns)
		}
	}
	verifier.logger.Debug().
		Strs("srcNamespaces", srcNamespaces).
		Msg("Namespaces to verify.")

	// In verifyAll mode, we do not support collection renames, so src and dest lists are the same.
	verifier.srcNamespaces = srcNamespaces
	verifier.dstNamespaces = srcNamespaces
	return nil
}

func (verifier *Verifier) CreateInitialTasksIfNeeded(ctx context.Context) error {
	// If we don't know the src namespaces, we're definitely not the primary task.
	if !verifier.verifyAll {
		if len(verifier.srcNamespaces) == 0 {
			return nil
		}
		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
		if len(verifier.srcNamespaces) != len(verifier.dstNamespaces) {
			return errors.Errorf(
				"source has %d namespace(s), but destination has %d (they must match)",
				len(verifier.srcNamespaces),
				len(verifier.dstNamespaces),
			)
		}
	}
	isPrimary, err := verifier.CreatePrimaryTaskIfNeeded(ctx)
	if err != nil {
		return errors.Wrap(err, "creating primary task")
	}
	if !isPrimary {
		verifier.logger.Info().Msg("Primary task already existed; skipping setup")
		return nil
	}
	if verifier.verifyAll {
		err := verifier.setupAllNamespaceList(ctx)
		if err != nil {
			return errors.Wrap(err, "creating namespace list")
		}
	}

	err = verifier.addTimeseriesBucketsToNamespaces(ctx)
	if err != nil {
		return errors.Wrap(err, "adding timeseries buckets to namespaces")
	}

	for _, src := range verifier.srcNamespaces {
		_, err := verifier.InsertCollectionVerificationTask(ctx, src)
		if err != nil {
			return errors.Wrapf(
				err,
				"failed to insert collection verification task for namespace %#q",
				src,
			)
		}
	}

	verifier.gen0PendingCollectionTasks.Store(int32(len(verifier.srcNamespaces)))

	err = verifier.UpdatePrimaryTaskComplete(ctx)
	if err != nil {
		return err
	}
	return nil
}

func FetchFailedAndIncompleteTasks(
	ctx context.Context,
	logger *logger.Logger,
	coll *mongo.Collection,
	taskType verificationTaskType,
	generation int,
) ([]VerificationTask, []VerificationTask, error) {
	var FailedTasks, allTasks, IncompleteTasks []VerificationTask

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			cur, err := coll.Find(ctx, bson.D{
				bson.E{Key: "type", Value: taskType},
				bson.E{Key: "generation", Value: generation},
			})
			if err != nil {
				return err
			}

			err = cur.All(ctx, &allTasks)
			if err != nil {
				return err
			}
			for _, t := range allTasks {
				if failedStatuses.Contains(t.Status) {
					FailedTasks = append(FailedTasks, t)
				} else if t.Status != verificationTaskCompleted {
					IncompleteTasks = append(IncompleteTasks, t)
				}
			}

			return nil
		},
		"fetching generation %d's failed & incomplete tasks",
		generation,
	).Run(ctx, logger)

	return FailedTasks, IncompleteTasks, err
}

// work is the logic for an individual worker thread.
func (verifier *Verifier) work(ctx context.Context, workerNum int) error {
	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Msg("Worker started.")

	defer verifier.logger.Debug().
		Int("workerNum", workerNum).
		Msg("Worker finished.")

	for {
		startedFind := time.Now()

		taskOpt, err := verifier.FindNextVerifyTaskAndUpdate(ctx)
		if err != nil {
			return errors.Wrap(
				err,
				"failed to seek next task",
			)
		}

		elapsed := time.Since(startedFind)
		if elapsed > findTaskTimeWarnThreshold {
			verifier.logger.Warn().
				Int("workerNum", workerNum).
				Stringer("elapsed", elapsed).
				Msg("This worker just queried for the next available task. That query took longer than expected. The metadata database may be under excess load.")
		}

		task, gotTask := taskOpt.Get()
		if !gotTask {
			duration := verifier.workerSleepDelay

			if duration > 0 {
				time.Sleep(duration)
			}

			continue
		}

		verifier.workerTracker.Set(workerNum, task)

		switch task.Type {
		case verificationTaskVerifyCollection:
			err := verifier.ProcessCollectionVerificationTask(ctx, workerNum, &task)
			verifier.workerTracker.Unset(workerNum)

			if err != nil {
				return err
			}
			if task.Generation == 0 {
				newVal := verifier.gen0PendingCollectionTasks.Add(-1)
				if newVal == 0 {
					verifier.PrintVerificationSummary(ctx, Gen0MetadataAnalysisComplete)
				}
			}
		case verificationTaskVerifyDocuments:
			err := verifier.ProcessVerifyTask(ctx, workerNum, &task)
			verifier.workerTracker.Unset(workerNum)

			if err != nil {
				return err
			}
		default:
			panic("Unknown verification task type: " + task.Type)
		}
	}
}
