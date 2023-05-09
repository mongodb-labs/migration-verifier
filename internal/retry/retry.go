package retry

import (
	"context"
	"math/rand"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
)

// RunForUUIDAndTransientErrors retries f() for the CollectionUUIDMismatch error and for transient errors.
// This should be used to run a driver operation that optionally specifies the `collectionUUID` parameter
// for a collection that may have been:
//
//   - Dropped and requires no further action from f().
//   - Renamed and requires a retry for f() with an updated collection name.
//
// IMPORTANT: This function should generally NOT be used within a transaction callback. It may be used
// within a transaction callback if and only if:
//
//   - f() uses a different context than the transaction's session context, and:
//   - f() runs driver operations on a different cluster than the one the transaction is being run on.
//
// See: https://github.com/mongodb/specifications/blob/master/source/transactions/transactions.rst#interaction-with-retryable-writes
//
// The collection's expected name should be provided as the expectedCollName parameter if f() uses the
// `collectionUUID` parameter. The expectedCollName may be set to the empty string if f() is guaranteed
// to know the collection's name (e.g. for mongosync's metadata) or if f() does not need to reference a
// collection (e.g. when running a command against the admin database).
//
// f() is provided with a collection name string, which is the one that should be used in the body
// of f() when a collection name is needed. The initial value of this string is expectedCollName.
//
// RunForUUIDAndTransientErrors always returns the collection's current name. It returns
// an error if the duration limit is reached, or if f() returns a non-transient error.
func (r *Retryer) RunForUUIDAndTransientErrors(
	ctx context.Context, logger *logger.Logger, expectedCollName string, f func(*Info, string) error,
) (string, error) {
	return r.runRetryLoop(ctx, logger, expectedCollName, f, true, true)
}

// RunForUUIDErrorOnly retries f() for the CollectionUUIDMismatch error only. This should primarily
// be used to wrap a transaction callback containing an operation that specifies the `collectionUUID`
// parameter for a collection that may have been:
//
//   - Dropped and requires no further action from f().
//   - Renamed and requires a retry for f() with an updated collection name.
//
// We retry only CollectionUUIDMismatch errors for transactions since transactions are already sufficiently
// retried for transient errors by the driver.
//
// The collection's expected name should be provided as the expectedCollName parameter and shouldn't be blank.
//
// f() is provided with a collection name string, which is the one that should be used in the body
// of f() where a collection name is needed. The initial value of this string is expectedCollName.
//
// RunForUUIDErrorOnly returns the collection's current name in all cases.
func (r *Retryer) RunForUUIDErrorOnly(
	logger *logger.Logger, expectedCollName string, f func(*Info, string) error,
) (string, error) {
	// Since we're not actually sleeping when checking for UUID/name mismatch
	// errors, we don't need to provide a real context to handle
	// cancellations.
	return r.runRetryLoop(context.Background(), logger, expectedCollName, f, false, true)
}

// RunForTransientErrorsOnly retries f() for transient errors only, and
// does not check for UUID mismatch error.
//
// RunForUUIDErrorOnly returns the collection's current name if f() succeeds.
func (r *Retryer) RunForTransientErrorsOnly(
	ctx context.Context, logger *logger.Logger, f func(*Info) error,
) error {
	wrapper := func(ri *Info, _ string) error {
		return f(ri)
	}
	_, err := r.runRetryLoop(ctx, logger, "", wrapper, true, false)
	return err
}

// runRetryLoop contains the core logic for the retry loops. This is separated
// into its own method so we can share this logic between retries with and
// without transient error handling. It's important that we _always_ return a
// collection name, even if we're also returning an error. The reason is that
// there is some code that does something like this:
//
//	collName, err := retryer.RunForUUIDAndTransientErrors(ctx, logger, collName, f)
//	if someCondition && isSomeSortOfError(err) {
//	    collName, err := retryer.RunForUUIDAndTransientErrors(ctx, logger, collName, f)
//	}
//
// If we return an empty string on an error then we will break that second
// call. Arguably, this sort of pattern should maybe be moved into the
// retryer, but until it is we cannot change this function's return vals.
func (r *Retryer) runRetryLoop(
	ctx context.Context,
	logger *logger.Logger,
	expectedCollName string,
	f func(*Info, string) error,
	handleTransientErrors bool,
	handleUUIDMismatchErrors bool,
) (string, error) {
	util.Invariant(
		logger,
		handleTransientErrors || handleUUIDMismatchErrors,
		"runRetryLoop must be called with at least one of handleTransientErrors or handleUUIDMismatchErrors set to true",
	)

	// If we are given an empty string as a name, that means that the
	// collection was already dropped, so there's nothing else to do.
	if handleUUIDMismatchErrors && expectedCollName == "" {
		logger.Debug().Msg(
			"The collection name is empty, meaning the collection was dropped, so we will not start this operation")
		return "", nil
	}

	var err error

	ri := &Info{
		attemptNumber:            0,
		durationSoFar:            0,
		durationLimit:            r.retryLimit,
		shouldResetDuration:      false,
		numCollectionUUIDRetries: 0,
	}
	sleepTime := minSleepTime

	// We start with the expected collection name that is provided.
	currCollName := expectedCollName

	for ri.durationSoFar <= ri.durationLimit {
		retryStartTime := time.Now()

		// Run f() with the current collection name.
		err = f(ri, currCollName)

		// If f() returned a CollectionUUIDMismatch error, get the updated collection name
		// from the error. If the name is blank, it means the collection no longer exists.
		if handleUUIDMismatchErrors && util.IsCollectionUUIDMismatchError(err) {
			prevCollName := currCollName
			currCollName, err = util.GetActualCollectionFromCollectionUUIDMismatchError(logger, err)
			if err != nil {
				return currCollName, err
			}
			if currCollName == "" {
				logger.Debug().
					Str("expected collection name", expectedCollName).
					Str("previous collection name", prevCollName).
					Msg("The collection was dropped so we will not retry this operation")
				return currCollName, nil
			}

			// If this happens something is terribly wrong with the server
			// we're talking to or in our code.
			if prevCollName == currCollName {
				logger.Error().
					Str("expected collection name", expectedCollName).
					Str("previous collection name", prevCollName).
					Str("actual collection name", currCollName).
					Msg("Got a collection UUID mismatch error but the actual collection name matches the last used name. Giving up on retries.")
				return currCollName, err
			}

			logger.Debug().
				Str("expected collection name", expectedCollName).
				Str("previous collection name", prevCollName).
				Str("actual collection name", currCollName).
				Msg("Got a collection UUID mismatch error and will retry with the actual collection name.")

			ri.numCollectionUUIDRetries++
			// We warn for an unusually high number of CollectionUUIDMismatch errors. This shouldn't happen in practice.
			if ri.numCollectionUUIDRetries >= numCollectionUUIDRetriesBeforeWarning {
				logger.Warn().
					Msgf("Unexpectedly high number of CollectionUUIDMismatch error retries for collection with "+
						"expected name %s (%d retries so far).", expectedCollName, ri.numCollectionUUIDRetries)
			}

			continue
		}

		// If this is the first time we've come across a failure to parse
		// collection UUID, try again with the UUID elided (if the caller used
		// RequestWithUUID).
		if r.retryOnUUIDNotSupported && !r.aggregateDisallowsUUIDs && util.IsFailedToParseError(err) && util.HasServerErrorMessage(err, "collectionUUID") {
			logger.Debug().Msg("Server version (< 5.0) does not support UUIDs in 'aggregate'. Will retry without UUID.")
			r.aggregateDisallowsUUIDs = true
			continue
		}

		// If f() returned a transient error, sleep and increase the sleep
		// time for the next retry, maxing out at the maxSleepTime.
		if r.shouldRetryWithSleep(logger, handleTransientErrors, sleepTime, err) {
			select {
			case <-ctx.Done():
				logger.Error().Err(ctx.Err()).Msg("Context was canceled. Aborting retry loop.")
				return currCollName, ctx.Err()
			case <-time.After(sleepTime):
				sleepTime *= sleepTimeMultiplier
				if sleepTime > maxSleepTime {
					sleepTime = maxSleepTime
				}
			}

			ri.attemptNumber++

			if ri.shouldResetDuration {
				ri.durationSoFar = 0
				ri.shouldResetDuration = false
			} else {
				ri.durationSoFar += time.Since(retryStartTime)
			}
			continue
		}

		// If f() had no error, return the current collection name.
		if err == nil {
			return currCollName, nil
		}

		return currCollName, err
	}

	return currCollName, errors.Wrapf(err,
		"retryable function did not succeed after %d attempt(s) over %.2f second(s)",
		ri.attemptNumber,
		ri.durationSoFar.Seconds())
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
	handleTransientErrors bool,
	sleepTime time.Duration,
	err error,
) bool {
	// Randomly retry approximately 1 in 100 calls to the wrapped
	// function. This is only enabled in tests.
	if r.retryRandomly && rand.Int()%100 == 0 {
		logger.Debug().Msgf("Waiting %s seconds to retry operation because of test code forcing a retry.", sleepTime)
		return true
	}

	if !handleTransientErrors {
		return false
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

// Use this method for aggregates which should take a UUID in new versions but not old ones.
// Pass the request without the collectionUUID field in 'request'.
// Currently, use only for 'aggregate'; 'find' always supports UUID.
func (r *Retryer) RequestWithUUID(request bson.D, uuid util.UUID) bson.D {
	if r.aggregateDisallowsUUIDs {
		return request
	}
	return append(append(bson.D{}, request[0], bson.E{"collectionUUID", uuid}), request[1:]...)
}
