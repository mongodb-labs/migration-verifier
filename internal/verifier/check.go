package verifier

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/10gen/migration-verifier/internal/retry"
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
)

var failedStatus = mapset.NewSet(
	verificationTaskFailed,
	verificationTaskMetadataMismatch,
)

var verificationStatusCheckInterval time.Duration = 15 * time.Second

// Check is the asynchronous entry point to Check, should only be called by the web server. Use
// CheckDriver directly for synchronous run.
// testChan is a pair of channels for coordinating generations in tests.
// testChan[0] is a channel signalled when when a generation is complete
// testChan[1] is a channel signalled when Check should continue with the next generation.
func (verifier *Verifier) Check(ctx context.Context) {
	go func() {
		err := verifier.CheckDriver(ctx, nil)
		if err != nil {
			verifier.logger.Fatal().Err(err).Msgf("Fatal error in generation %d", verifier.generation)
		}
	}()
}

func (verifier *Verifier) waitForChangeStream() error {
	verifier.mux.RLock()
	csRunning := verifier.changeStreamRunning
	verifier.mux.RUnlock()
	if csRunning {
		verifier.logger.Debug().Msg("Changestream still running, signalling that writes are done and waiting for change stream to exit")
		verifier.changeStreamEnderChan <- struct{}{}
		select {
		case err := <-verifier.changeStreamErrChan:
			return err
		case <-verifier.changeStreamDoneChan:
			break
		}
	}
	return nil
}

func (verifier *Verifier) CheckWorker(ctx context.Context) error {
	verifier.logger.Debug().Msgf("Starting %d verification workers", verifier.numWorkers)
	ctx, cancel := context.WithCancel(ctx)
	wg := sync.WaitGroup{}
	for i := 0; i < verifier.numWorkers; i++ {
		wg.Add(1)
		go verifier.Work(ctx, i, &wg)
		time.Sleep(10 * time.Millisecond)
	}

	generation := verifier.generation

	// Since we do a progress report right at the start we don’t need
	// this to go in non-debug output.
	startLabel := fmt.Sprintf("Starting check generation %d", generation)
	verifier.logger.Debug().Msg(startLabel)

	genStartReport := startReport()
	genStartReport.WriteString(startLabel + "\n\n")
	genStartReport.WriteString("Gathering collection metadata …\n")

	verifier.writeStringBuilder(genStartReport)

	waitForTaskCreation := 0

	for {
		select {
		case err := <-verifier.changeStreamErrChan:
			cancel()
			return err
		case <-ctx.Done():
			cancel()
			return nil
		default:
		}

		verificationStatus, err := verifier.GetVerificationStatus()
		if err != nil {
			verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
		}

		if waitForTaskCreation%2 == 0 {
			if generation > 0 || verifier.gen0PendingCollectionTasks.Load() == 0 {
				verifier.PrintVerificationSummary(ctx, GenerationInProgress)
			}
		}

		//wait for task to be created, if none of the tasks found.
		if verificationStatus.AddedTasks > 0 || verificationStatus.ProcessingTasks > 0 || verificationStatus.RecheckTasks > 0 {
			waitForTaskCreation++
			time.Sleep(verificationStatusCheckInterval)
		} else {
			verifier.PrintVerificationSummary(ctx, GenerationComplete)
			verifier.logger.Debug().Msg("Verification tasks complete")
			cancel()
			wg.Wait()
			break
		}
	}
	verifier.logger.Debug().Msgf("Check generation %d finished", generation)
	return nil
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
	verifier.mux.Unlock()
	defer func() {
		verifier.mux.Lock()
		verifier.running = false
		verifier.globalFilter = nil
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
	verifier.logger.Debug().Msg("Starting Check")

	verifier.phase = Check
	defer func() {
		verifier.phase = Idle
	}()

	verifier.mux.RLock()
	csRunning := verifier.changeStreamRunning
	verifier.mux.RUnlock()
	if !csRunning {
		verifier.logger.Debug().Msg("Change stream not running; starting change stream")
		retryer := retry.New(retry.DefaultDurationLimit).SetRetryOnUUIDNotSupported()
		// Ignore the error from this call -- if it fails, we use an alternate method
		// where we use the change stream's initial resume token.
		startAtTs, _ := GetLastOpTimeAndSyncShardClusterTime(ctx,
			verifier.logger,
			retryer,
			verifier.srcClient,
			true)
		err = verifier.StartChangeStream(ctx, startAtTs)
		if err != nil {
			return err
		}
	}
	// Log out the verification status when initially booting up so it's easy to see the current state
	verificationStatus, err := verifier.GetVerificationStatus()
	if err != nil {
		verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
	} else {
		verifier.logger.Debug().Msgf("Initial verification phase: %+v", verificationStatus)
	}

	err = verifier.CreateInitialTasks()
	if err != nil {
		return err
	}
	// Now enter the multi-generational steady check state
	for {
		verifier.generationStartTime = time.Now()

		err := verifier.CheckWorker(ctx)
		if err != nil {
			return err
		}
		// we will only coordinate when the number of channels is exactly 2.
		// * Channel 0 signals a generation is done
		// * Channel 1 signals to check to continue the next generation
		if len(testChan) == 2 {
			testChan[0] <- struct{}{}
			<-testChan[1]
		}
		time.Sleep(verifier.generationPauseDelayMillis * time.Millisecond)
		verifier.mux.Lock()
		if verifier.lastGeneration {
			verifier.mux.Unlock()
			return nil
		}
		// TODO: wait here until writesOff is hit or enough time has passed, so we don't spin
		// doing empty rechecks.

		// possible issue: turning the writes off at the exact same time a new iteration starts
		// will result in an extra iteration. The odds of this are lower and the user should be
		// paying attention. Also, this should not matter too much because any failures will be
		// caught again on the next iteration.
		if verifier.writesOff {
			// It's necessary to wait for the change stream to finish before incrementing the
			// generation number, or the last changes will not be checked.
			verifier.mux.Unlock()
			err := verifier.waitForChangeStream()
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
			verifier.logger.Error().Msgf("Failed trying to clear out old recheck docs, continuing: %v",
				err)
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

func (verifier *Verifier) CreateInitialTasks() error {
	// If we don't know the src namespaces, we're definitely not the primary task.
	if !verifier.verifyAll {
		if len(verifier.srcNamespaces) == 0 {
			return nil
		}
		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
		if len(verifier.srcNamespaces) != len(verifier.dstNamespaces) {
			err := errors.Errorf("Different number of source and destination namespaces")
			verifier.logger.Error().Msgf("%s", err)
			return err
		}
	}
	isPrimary, err := verifier.CheckIsPrimary()
	if err != nil {
		return err
	}
	if !isPrimary {
		verifier.logger.Info().Msgf("Primary task already existed; skipping setup")
		return nil
	}
	if verifier.verifyAll {
		err := verifier.setupAllNamespaceList(context.Background())
		if err != nil {
			return err
		}
	}
	for _, src := range verifier.srcNamespaces {
		_, err := verifier.InsertCollectionVerificationTask(src)
		if err != nil {
			verifier.logger.Error().Msgf("Failed to insert collection verification task: %s", err)
			return err
		}
	}

	verifier.gen0PendingCollectionTasks.Store(int32(len(verifier.srcNamespaces)))

	err = verifier.UpdatePrimaryTaskComplete()
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

func (verifier *Verifier) Work(ctx context.Context, workerNum int, wg *sync.WaitGroup) {
	defer wg.Done()
	verifier.logger.Debug().Msgf("[Worker %d] Started", workerNum)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			task, err := verifier.FindNextVerifyTaskAndUpdate()
			if errors.Is(err, mongo.ErrNoDocuments) {
				verifier.logger.Debug().Msgf("[Worker %d] No tasks found, sleeping...", workerNum)
				time.Sleep(verifier.workerSleepDelayMillis * time.Millisecond)
				continue
			} else if err != nil {
				panic(err)
			}

			if task.Type == verificationTaskVerifyCollection {
				verifier.ProcessCollectionVerificationTask(ctx, workerNum, task)
				if task.Generation == 0 {
					newVal := verifier.gen0PendingCollectionTasks.Add(-1)
					if newVal == 0 {
						verifier.PrintVerificationSummary(ctx, Gen0MetadataAnalysisComplete)
					}
				}
			} else {
				verifier.ProcessVerifyTask(workerNum, task)
			}
		}
	}
}
