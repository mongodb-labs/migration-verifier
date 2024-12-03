package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/samber/lo"
	"golang.org/x/sync/errgroup"
)

type RetryCallback = func(context.Context, *FuncInfo) error

// Retry is a convenience that creates a retryer and executes it.
// See RunForTransientErrorsOnly for argument details.
func Retry(
	ctx context.Context,
	logger *logger.Logger,
	callbacks ...RetryCallback,
) error {
	retryer := New(DefaultDurationLimit)
	return retryer.Run(ctx, logger, callbacks...)
}

// Run() runs each given callback in parallel. If none of them fail,
// then no error is returned.
//
// If one of them fails, the other callbacks' contexts are canceled.
// If the error is non-transient, it's returned. If the error is transient,
// though, then the retryer reruns each callback.
//
// The retryer tracks the last time each callback either a) succeeded or b)
// was canceled. Whenever a callback fails, the retryer checks how long it
// has gone since a success/cancellation. If that time period exceeds the
// retryer's duration limit, then the retry loop ends, and a
// RetryDurationLimitExceededErr is returned.
//
// Note that, if a given callback runs multiple potentially-retryable requests,
// each successful request should be noted in the callback's FuncInfo.
// See that struct's documentation for more details.
//
// IMPORTANT: This function should generally NOT be used within a transaction
// callback. It may be used within a transaction callback if and only if:
//
//   - f() uses a different context than the transaction's session context, and:
//   - f() runs driver operations on a different cluster than the one the
//     transaction is being run on.
//
// See: https://github.com/mongodb/specifications/blob/master/source/transactions/transactions.rst#interaction-with-retryable-writes
//
// This returns an error if the duration limit is reached, or if f() returns a
// non-transient error.
func (r *Retryer) Run(
	ctx context.Context, logger *logger.Logger, f ...RetryCallback,
) error {
	return r.runRetryLoop(ctx, logger, f)
}

// runRetryLoop contains the core logic for the retry loops.
func (r *Retryer) runRetryLoop(
	ctx context.Context,
	logger *logger.Logger,
	funcs []RetryCallback,
) error {
	var err error

	startTime := time.Now()

	li := &LoopInfo{
		durationLimit: r.retryLimit,
	}
	funcinfos := lo.RepeatBy(
		len(funcs),
		func(_ int) *FuncInfo {
			return &FuncInfo{
				lastResetTime: startTime,
				loopInfo:      li,
			}
		},
	)
	sleepTime := minSleepTime

	for {
		eg, egCtx := errgroup.WithContext(ctx)
		for i, curFunc := range funcs {

			eg.Go(func() error {
				err := curFunc(egCtx, funcinfos[i])

				if err != nil {
					return errgroupErr{
						funcNum:         i,
						errFromCallback: err,
					}
				}

				return nil
			})
		}
		err = eg.Wait()

		// No error? Success!
		if err == nil {
			return nil
		}

		// Let's get the actual error from the function.
		groupErr := errgroupErr{}
		if !errors.As(err, &groupErr) {
			panic(fmt.Sprintf("Error should be a %T, not %T: %v", groupErr, err, err))
		}

		// Not a transient error? Fail immediately.
		if !r.shouldRetryWithSleep(logger, sleepTime, groupErr.errFromCallback) {
			return groupErr.errFromCallback
		}

		li.attemptNumber++

		// Our error is transient. If we've exhausted the allowed time
		// then fail.
		failedFuncInfo := funcinfos[groupErr.funcNum]
		if failedFuncInfo.GetDurationSoFar() > li.durationLimit {
			return RetryDurationLimitExceededErr{
				attempts: li.attemptNumber,
				duration: failedFuncInfo.GetDurationSoFar(),
				lastErr:  groupErr.errFromCallback,
			}
		}

		// Sleep and increase the sleep time for the next retry,
		// up to maxSleepTime.
		select {
		case <-ctx.Done():
			logger.Error().Err(ctx.Err()).Msg("Context was canceled. Aborting retry loop.")
			return ctx.Err()
		case <-time.After(sleepTime):
			sleepTime *= sleepTimeMultiplier
			if sleepTime > maxSleepTime {
				sleepTime = maxSleepTime
			}
		}

		now := time.Now()

		// Set all of the funcs that did *not* fail as having just succeeded.
		for i, curInfo := range funcinfos {
			if i != groupErr.funcNum {
				curInfo.lastResetTime = now
			}
		}
	}
}

//
// For the above function, there have historically been concerns regarding majority write concern
// upon retrying a write operation to the server. Mongomirror explicitly handled this:
// https://github.com/10gen/mongomirror/blob/7dc961b1fe0d8986815277179c1e97f92f6b9808/mongomirror/mongomirror.go#L1265-L1272
//
// However, the server will generally honor write concern for a command that attempted a write, even if the write results in an error.
// So explicit handling this upon a write command retry is not necessary:
// https://github.com/mongodb/mongo/blob/303071db10ec4e49c4fd7617d9f59828c47ee06e/jstests/replsets/noop_writes_wait_for_write_concern.js#L1-L6
//

func (r *Retryer) shouldRetryWithSleep(
	logger *logger.Logger,
	sleepTime time.Duration,
	err error,
) bool {
	// Randomly retry approximately 1 in 100 calls to the wrapped
	// function. This is only enabled in tests.
	if r.retryRandomly && rand.Int()%100 == 0 {
		logger.Debug().Msgf("Waiting %s seconds to retry operation because of test code forcing a retry.", sleepTime)
		return true
	}

	if err == nil {
		return false
	}

	errCode := util.GetErrorCode(err)
	if util.IsTransientError(err) {
		logger.Warn().Int("error code", errCode).Err(err).Msgf(
			"Waiting %s seconds to retry operation after transient error.", sleepTime)
		return true
	}

	for _, code := range r.additionalErrorCodes {
		if code == errCode {
			logger.Warn().Int("error code", errCode).Err(err).Msgf(
				"Waiting %s seconds to retry operation after an error because it is in our additional codes list.", sleepTime)
			return true
		}
	}

	logger.Debug().Err(err).Int("error code", errCode).
		Msg("Not retrying on error because it is not transient nor is it in our additional codes list.")

	return false
}
