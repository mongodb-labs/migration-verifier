package retry

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/msync"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
)

type RetryCallback = func(context.Context, *FuncInfo) error

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
func (r *Retryer) Run(ctx context.Context, logger *logger.Logger) error {
	return r.runRetryLoop(ctx, logger)
}

// runRetryLoop contains the core logic for the retry loops.
func (r *Retryer) runRetryLoop(
	ctx context.Context,
	logger *logger.Logger,
) error {
	var err error

	if len(r.callbacks) == 0 {
		return errors.Errorf(
			"retryer (%s) run with no callbacks",
			r.description.OrElse("no description"),
		)
	}

	startTime := time.Now()

	li := &LoopInfo{
		durationLimit: r.retryLimit,
	}
	funcinfos := lo.Map(
		r.callbacks,
		func(cb retryCallbackInfo, _ int) *FuncInfo {
			return &FuncInfo{
				lastReset: msync.NewTypedAtomic(lastResetInfo{
					time: startTime,
				}),
				description:     cb.description,
				loopDescription: r.description,
				loopInfo:        li,
			}
		},
	)
	sleepTime := minSleepTime

	for {
		if li.attemptsSoFar > 0 {
			r.addDescriptionToEvent(logger.Info()).
				Int("attemptsSoFar", li.attemptsSoFar).
				Msg("Retrying after failure.")
		}

		if beforeFunc, hasBefore := r.before.Get(); hasBefore {
			err := beforeFunc()

			if err != nil {
				return errors.Wrapf(err, "before %#q", r.description.OrZero())
			}
		}

		eg, egCtx := contextplus.ErrGroup(ctx)

		for i, curCbInfo := range r.callbacks {
			curFunc := curCbInfo.callback

			if curFunc == nil {
				panic("curFunc should be non-nil")
			}
			if funcinfos[i] == nil {
				panic(fmt.Sprintf("funcinfos[%d] should be non-nil", i))
			}

			eg.Go(func() error {
				cbDoneChan := make(chan struct{})
				defer close(cbDoneChan)

				go func() {
					ticker := time.NewTicker(time.Minute)
					defer ticker.Stop()

					for {
						lastReset := funcinfos[i].lastReset.Load()

						select {
						case <-cbDoneChan:
							return
						case <-ticker.C:
							if funcinfos[i].lastReset.Load() == lastReset {
								event := logger.Warn().
									Strs("description", funcinfos[i].GetDescriptions()).
									Time("noSuccessSince", lastReset.time).
									Uint64("successesSoFar", lastReset.resetsSoFar)

								if successDesc, hasDesc := lastReset.description.Get(); hasDesc {
									event.
										Str("lastSuccessDescription", successDesc)
								}

								event.
									Str("elapsedTime", reportutils.DurationToHMS(time.Since(lastReset.time))).
									Msg("Operation has not reported success for a while.")
							}
						}
					}
				}()

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

		li.attemptsSoFar++

		// No error? Success!
		if err == nil {
			if li.attemptsSoFar > 1 {
				r.addDescriptionToEvent(logger.Info()).
					Int("attempts", li.attemptsSoFar).
					Msg("Retried operation succeeded.")
			}

			return nil
		}

		// Let's get the actual error from the function.
		groupErr := errgroupErr{}
		if !errors.As(err, &groupErr) {
			panic(fmt.Sprintf("Error should be a %T, not %T: %v", groupErr, err, err))
		}

		failedFuncInfo := funcinfos[groupErr.funcNum]
		descriptions := failedFuncInfo.GetDescriptions()
		cbErr := groupErr.errFromCallback

		// Not a transient error? Fail immediately.
		if !r.shouldRetryWithSleep(logger, sleepTime, descriptions, cbErr) {
			return wrapErrWithDescriptions(cbErr, descriptions)
		}

		// Our error is transient. If we've exhausted the allowed time
		// then fail.

		if failedFuncInfo.GetDurationSoFar() > li.durationLimit {
			var err error = RetryDurationLimitExceededErr{
				attempts: li.attemptsSoFar,
				duration: failedFuncInfo.GetDurationSoFar(),
				lastErr:  groupErr.errFromCallback,
			}

			return wrapErrWithDescriptions(err, descriptions)
		}

		// Sleep and increase the sleep time for the next retry,
		// up to maxSleepTime.
		select {
		case <-ctx.Done():
			r.addDescriptionToEvent(logger.Error()).
				Err(ctx.Err()).
				Msg("Context was canceled. Aborting retry loop.")
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
				curInfo.lastReset.Store(lastResetInfo{time: now})
			}
		}
	}
}

func (r *Retryer) addDescriptionToEvent(event *zerolog.Event) *zerolog.Event {
	if description, hasDesc := r.description.Get(); hasDesc {
		event.Str("description", description)
	} else {
		event.Strs("description", lo.Map(
			r.callbacks,
			func(cbInfo retryCallbackInfo, _ int) string {
				return cbInfo.description
			},
		))
	}

	return event
}

func wrapErrWithDescriptions(err error, descriptions []string) error {
	reversed := slices.Clone(descriptions)
	slices.Reverse(reversed)

	for _, d := range reversed {
		err = errors.Wrap(err, d)
	}

	return err
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
	descriptions []string,
	err error,
) bool {
	if err == nil {
		panic("nil error should not get here")
	}

	isTransient := util.IsTransientError(err) || lo.SomeBy(
		r.additionalErrorCodes,
		func(code int) bool {
			return mmongo.ErrorHasCode(err, code)
		},
	)

	event := logger.WithLevel(
		lo.Ternary(
			// If it’s transient, surface it as info.
			isTransient,
			zerolog.InfoLevel,

			lo.Ternary(
				// Context cancellation is unimportant, so debug.
				errors.Is(err, context.Canceled),
				zerolog.DebugLevel,

				// Other non-retryables are serious, so warn.
				zerolog.WarnLevel,
			),
		),
	)

	event.Strs("description", descriptions).
		Int("error code", util.GetErrorCode(err)).
		Err(err)

	if isTransient {
		event.
			Stringer("delay", sleepTime).
			Msg("Got retryable error. Pausing, then will retry.")

		return true
	}

	event.Msg("Non-retryable error occurred.")

	return false
}
