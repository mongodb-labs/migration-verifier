package verifier

import (
	"context"
	"fmt"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/sync/errgroup"
)

type GenerationStatus string

const (
	Gen0MetadataAnalysisComplete GenerationStatus = "gen0_metadata_analysis_complete"
	GenerationInProgress         GenerationStatus = "inprogress"
	GenerationComplete           GenerationStatus = "complete"
)

var failedStatus = mapset.NewSet(
	verificationTaskFailed,
	verificationTaskMetadataMismatch,
)

// Check is the asynchronous entry point to Check, should only be called by the web server. Use
// CheckDriver directly for synchronous run.
// testChan is a pair of channels for coordinating generations in tests.
// testChan[0] is a channel signalled when when a generation is complete
// testChan[1] is a channel signalled when Check should continue with the next generation.
func (verifier *Verifier) Check(ctx context.Context, filter map[string]any) {
	go func() {
		err := verifier.CheckDriver(ctx, filter)
		if err != nil {
			verifier.logger.Fatal().Err(err).Msgf("Fatal error in generation %d", verifier.generation)
		}
	}()
	verifier.MaybeStartPeriodicHeapProfileCollection(ctx)
}

func (verifier *Verifier) waitForChangeStream(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-verifier.changeStreamErrChan:
		verifier.logger.Warn().Err(err).
			Msg("Received error from change stream.")
		return err
	case <-verifier.changeStreamDoneChan:
		verifier.logger.Debug().
			Msg("Received completion signal from change stream.")
		break
	}

	return nil
}

func (verifier *Verifier) CheckWorker(ctxIn context.Context) error {
	generation := verifier.generation

	verifier.logger.Debug().
		Int("generation", generation).
		Int("workersCount", verifier.numWorkers).
		Msgf("Starting verification worker threads.")

	// Since we do a progress report right at the start we don’t need
	// this to go in non-debug output.
	startLabel := fmt.Sprintf("Starting check generation %d", generation)
	verifier.logger.Debug().Msg(startLabel)

	genStartReport := startReport()
	genStartReport.WriteString(startLabel + "\n\n")
	genStartReport.WriteString("Gathering collection metadata …\n")

	verifier.writeStringBuilder(genStartReport)

	cancelableCtx, canceler := context.WithCancelCause(ctxIn)
	eg, ctx := errgroup.WithContext(cancelableCtx)

	// If the change stream fails, everything should stop.
	eg.Go(func() error {
		select {
		case err := <-verifier.changeStreamErrChan:
			return errors.Wrap(err, "change stream failed")
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

	succeeded := false

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

			if waitForTaskCreation%2 == 0 {
				if generation > 0 || verifier.gen0PendingCollectionTasks.Load() == 0 {
					verifier.PrintVerificationSummary(ctx, GenerationInProgress)
				}
			}

			// The generation continues as long as >=1 task for this generation is
			// “added” or “pending”.
			if verificationStatus.AddedTasks > 0 || verificationStatus.ProcessingTasks > 0 {
				waitForTaskCreation++
				time.Sleep(verifier.verificationStatusCheckInterval)
			} else {
				verifier.PrintVerificationSummary(ctx, GenerationComplete)
				succeeded = true
				canceler(errors.Errorf("generation %d succeeded", generation))
				return nil
			}
		}
	})

	err := eg.Wait()

	if succeeded {
		if !errors.Is(err, context.Canceled) {
			panic("success should mean that err is context.Canceled, not: " + err.Error())
		}

		err = nil
	}

	if err != nil {
		verifier.logger.Debug().
			Int("generation", generation).
			Msgf("Check finished.")
	}

	return errors.Wrapf(
		err,
		"check generation %d failed",
		generation,
	)
}

func (verifier *Verifier) CheckDriver(ctx context.Context, filter map[string]any, testChan ...chan struct{}) error {
	verifier.mux.Lock()
	if verifier.running {
		verifier.mux.Unlock()
		verifier.logger.Info().Msg("Verifier already checking the collections")
		return nil
	}
	verifier.running = true
	verifier.globalFilter = filter
	verifier.mux.Unlock()
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
			return err
		}
	}
	err = verifier.AddMetaIndexes(ctx)
	if err != nil {
		return err
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

	verifier.logger.Debug().Msg("Starting Check")

	verifier.phase = Check
	defer func() {
		verifier.phase = Idle
	}()

	verifier.mux.RLock()
	csRunning := verifier.changeStreamRunning
	verifier.mux.RUnlock()
	if csRunning {
		verifier.logger.Debug().Msg("Check: Change stream already running.")
	} else {
		verifier.logger.Debug().Msg("Change stream not running; starting change stream")

		err = verifier.StartChangeStream(ctx)
		if err != nil {
			return errors.Wrap(err, "failed to start change stream on source")
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
		verifier.logger.Debug().Msgf("Initial verification phase: %+v", verificationStatus)
	}

	err = verifier.CreateInitialTasks(ctx)
	if err != nil {
		return err
	}
	// Now enter the multi-generational steady check state
	for {
		verifier.generationStartTime = time.Now()
		verifier.eventRecorder.Reset()

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
		time.Sleep(verifier.generationPauseDelayMillis * time.Millisecond)
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
				Msg("Waiting for change stream to end.")

			// It's necessary to wait for the change stream to finish before incrementing the
			// generation number, or the last changes will not be checked.
			verifier.mux.Unlock()
			err := verifier.waitForChangeStream(ctx)
			if err != nil {
				return err
			}
			verifier.mux.Lock()
			verifier.lastGeneration = true
		}
		verifier.generation++
		verifier.phase = Recheck
		err = verifier.GenerateRecheckTasks(ctx)
		if err != nil {
			verifier.mux.Unlock()
			return err
		}

		err = verifier.ClearRecheckDocs(ctx)
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
	srcNamespaces, err := ListAllUserCollections(ctx, verifier.logger, verifier.srcClient,
		true /* include views */, verifier.metaDBName)
	if err != nil {
		return err
	}

	dstNamespaces, err := ListAllUserCollections(ctx, verifier.logger, verifier.dstClient,
		true /* include views */, verifier.metaDBName)
	if err != nil {
		return err
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
	verifier.logger.Debug().Msgf("Namespaces to verify %+v", srcNamespaces)
	// In verifyAll mode, we do not support collection renames, so src and dest lists are the same.
	verifier.srcNamespaces = srcNamespaces
	verifier.dstNamespaces = srcNamespaces
	return nil
}

func (verifier *Verifier) CreateInitialTasks(ctx context.Context) error {
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
	isPrimary, err := verifier.CheckIsPrimary(ctx)
	if err != nil {
		return err
	}
	if !isPrimary {
		verifier.logger.Info().Msgf("Primary task already existed; skipping setup")
		return nil
	}
	if verifier.verifyAll {
		err := verifier.setupAllNamespaceList(ctx)
		if err != nil {
			return err
		}
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

func FetchFailedAndIncompleteTasks(ctx context.Context, coll *mongo.Collection, taskType verificationTaskType, generation int) ([]VerificationTask, []VerificationTask, error) {
	var FailedTasks, allTasks, IncompleteTasks []VerificationTask

	cur, err := coll.Find(ctx, bson.D{
		bson.E{Key: "type", Value: taskType},
		bson.E{Key: "generation", Value: generation},
	})
	if err != nil {
		return FailedTasks, IncompleteTasks, err
	}

	err = cur.All(ctx, &allTasks)
	if err != nil {
		return FailedTasks, IncompleteTasks, err
	}
	for _, t := range allTasks {
		if failedStatus.Contains(t.Status) {
			FailedTasks = append(FailedTasks, t)
		} else if t.Status != verificationTaskCompleted {
			IncompleteTasks = append(IncompleteTasks, t)
		}
	}

	return FailedTasks, IncompleteTasks, nil
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
		task, err := verifier.FindNextVerifyTaskAndUpdate(ctx)
		if errors.Is(err, mongo.ErrNoDocuments) {
			duration := verifier.workerSleepDelayMillis * time.Millisecond

			if duration > 0 {
				verifier.logger.Debug().
					Int("workerNum", workerNum).
					Stringer("duration", duration).
					Msg("No tasks found. Sleeping.")

				time.Sleep(duration)
			}

			continue
		} else if err != nil {
			return errors.Wrap(
				err,
				"failed to seek next task",
			)
		}

		switch task.Type {
		case verificationTaskVerifyCollection:
			err := verifier.ProcessCollectionVerificationTask(ctx, workerNum, task)
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
			err := verifier.ProcessVerifyTask(ctx, workerNum, task)
			if err != nil {
				return err
			}
		default:
			panic("Unknown verification task type: " + task.Type)
		}
	}
}
