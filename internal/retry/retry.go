package retry

import (
	"context"
	"math/rand"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
)

// Run retries f() whenever a transient error happens, up to the retryer's
// configured duration limit.
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
	ctx context.Context, logger *logger.Logger, f func(*Info) error,
) error {
	return r.runRetryLoop(ctx, logger, f)
}

// runRetryLoop contains the core logic for the retry loops.
func (r *Retryer) runRetryLoop(
	ctx context.Context,
	logger *logger.Logger,
	f func(*Info) error,
) error {
	var err error

	ri := &Info{
		durationLimit: r.retryLimit,
		lastResetTime: time.Now(),
	}
	sleepTime := minSleepTime

	for {
		err = f(ri)

		// If f() returned a transient error, sleep and increase the sleep
		// time for the next retry, maxing out at the maxSleepTime.
		if err == nil {
			return nil
		}

		if !r.shouldRetryWithSleep(logger, sleepTime, err) {
			return err
		}

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

		ri.attemptNumber++

		if ri.shouldResetDuration {
			ri.lastResetTime = time.Now()
			ri.shouldResetDuration = false
		}

		if ri.GetDurationSoFar() > ri.durationLimit {
			return RetryDurationLimitExceededErr{
				attempts: ri.attemptNumber,
				duration: ri.GetDurationSoFar(),
				lastErr:  err,
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
