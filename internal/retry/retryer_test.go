package retry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/10gen/migration-verifier/internal/util"
	"go.mongodb.org/mongo-driver/mongo"
)

var someNetworkError = &mongo.CommandError{
	Labels: []string{"NetworkError"},
	Name:   "NetworkError",
}

var badError = errors.New("I am fatal!")

func (suite *UnitTestSuite) TestRetryer() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	suite.Run("with a function that immediately succeeds", func() {
		attemptNumber := -1
		f := func(_ context.Context, ri *FuncInfo) error {
			attemptNumber = ri.GetAttemptNumber()
			return nil
		}

		err := retryer.Run(suite.Context(), logger, f)
		suite.NoError(err)
		suite.Equal(0, attemptNumber)

		f2 := func(_ context.Context, ri *FuncInfo) error {
			attemptNumber = ri.GetAttemptNumber()
			return nil
		}

		err = retryer.Run(suite.Context(), logger, f2)
		suite.NoError(err)
		suite.Equal(0, attemptNumber)
	})

	suite.Run("with a function that succeeds after two attempts", func() {
		attemptNumber := -1
		f := func(_ context.Context, ri *FuncInfo) error {
			attemptNumber = ri.GetAttemptNumber()
			if attemptNumber < 2 {
				return someNetworkError
			}
			return nil
		}

		err := retryer.Run(suite.Context(), logger, f)
		suite.NoError(err)
		suite.Equal(2, attemptNumber)

		attemptNumber = -1
		f2 := func(_ context.Context, ri *FuncInfo) error {
			attemptNumber = ri.GetAttemptNumber()
			if attemptNumber < 2 {
				return someNetworkError
			}
			return nil
		}

		err = retryer.Run(suite.Context(), logger, f2)
		suite.NoError(err)
		suite.Equal(2, attemptNumber)
	})
}

func (suite *UnitTestSuite) TestRetryerDurationLimitIsZero() {
	retryer := New(0)

	attemptNumber := -1
	f := func(_ context.Context, ri *FuncInfo) error {
		attemptNumber = ri.GetAttemptNumber()
		return someNetworkError
	}

	err := retryer.Run(suite.Context(), suite.Logger(), f)
	suite.Assert().ErrorIs(err, someNetworkError)
	suite.Assert().Equal(0, attemptNumber)
}

func (suite *UnitTestSuite) TestRetryerDurationReset() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	// In this test, the given function f takes longer than the durationLimit
	// to execute. (f will artificially advance the time to greater than the
	// durationLimit.)

	// 1) Not calling IterationSuccess() means f will not be retried, since the
	// durationLimit is exceeded
	noSuccessIterations := 0
	f1 := func(_ context.Context, ri *FuncInfo) error {
		// Artificially advance how much time was taken.
		ri.lastResetTime = ri.lastResetTime.Add(-2 * ri.loopInfo.durationLimit)

		noSuccessIterations++
		if noSuccessIterations == 1 {
			return someNetworkError
		}

		return nil
	}

	err := retryer.Run(suite.Context(), logger, f1)

	// The error should be the limit-exceeded error, with the
	// last-noted error being the transient error.
	suite.Assert().ErrorAs(err, &RetryDurationLimitExceededErr{})
	suite.Assert().ErrorIs(err, someNetworkError)
	suite.Equal(1, noSuccessIterations)

	// 2) Calling IterationSuccess() means f will run more than once because the
	// duration should be reset.
	successIterations := 0
	f2 := func(_ context.Context, ri *FuncInfo) error {
		// Artificially advance how much time was taken.
		ri.lastResetTime = ri.lastResetTime.Add(-2 * ri.loopInfo.durationLimit)

		ri.NoteSuccess()

		successIterations++
		if successIterations == 1 {
			return someNetworkError
		}

		return nil
	}

	err = retryer.Run(suite.Context(), logger, f2)
	suite.Assert().NoError(err)
	suite.Assert().Equal(2, successIterations)
}

func (suite *UnitTestSuite) TestCancelViaContext() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	counter := 0
	var wg sync.WaitGroup
	wg.Add(1)
	f := func(_ context.Context, _ *FuncInfo) error {
		counter++
		if counter == 1 {
			return errors.New("not master")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(suite.Context())

	// We need to cancel before we allow the f() func to do any work. This ensures that the
	// retry code will see the cancel before the timer it sets expires.
	cancel()
	go func() {
		err := retryer.Run(ctx, logger, f)
		suite.ErrorIs(err, context.Canceled)
		suite.Equal(1, counter)
		wg.Done()
	}()

	wg.Wait()
}

func (suite *UnitTestSuite) TestRetryerAdditionalErrorCodes() {
	logger := suite.Logger()

	customError := mongo.CommandError{
		Name: "CustomError",
		Code: 42,
	}

	var attemptNumber int
	f := func(_ context.Context, ri *FuncInfo) error {
		attemptNumber = ri.GetAttemptNumber()
		if attemptNumber == 0 {
			return customError
		}
		return nil
	}

	suite.Run("with no additional error codes", func() {
		retryer := New(DefaultDurationLimit)
		err := retryer.Run(suite.Context(), logger, f)
		suite.Equal(42, util.GetErrorCode(err))
		suite.Equal(0, attemptNumber)
	})

	suite.Run("with one additional error code", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(42)
		err := retryer.Run(suite.Context(), logger, f)
		suite.NoError(err)
		suite.Equal(1, attemptNumber)
	})

	suite.Run("with multiple additional error codes", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(42, 43, 44)
		err := retryer.Run(suite.Context(), logger, f)
		suite.NoError(err)
		suite.Equal(1, attemptNumber)
	})

	suite.Run("with multiple additional error codes that don't match error", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(41, 43, 44)
		err := retryer.Run(suite.Context(), logger, f)
		suite.Equal(42, util.GetErrorCode(err))
		suite.Equal(0, attemptNumber)
	})
}

func (suite *UnitTestSuite) TestMulti_NonTransient() {
	ctx := suite.Context()
	logger := suite.Logger()

	retryer := New(DefaultDurationLimit)

	err := retryer.Run(
		ctx,
		logger,
		func(ctx context.Context, _ *FuncInfo) error {
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		func(_ context.Context, _ *FuncInfo) error {
			return badError
		},
	)

	suite.Assert().ErrorIs(err, badError)
}

func (suite *UnitTestSuite) TestMulti_Transient() {
	ctx := suite.Context()
	logger := suite.Logger()

	for _, finalErr := range []error{nil, badError} {
		suite.Run(
			fmt.Sprintf("final error: %v", finalErr),
			func() {
				retryer := New(DefaultDurationLimit)
				cb1Attempts := 0
				cb2Attempts := 0

				err := retryer.Run(
					ctx,
					logger,

					// This one succeeds every time.
					func(ctx context.Context, _ *FuncInfo) error {
						cb1Attempts++

						return nil
					},
					func(_ context.Context, _ *FuncInfo) error {
						cb2Attempts++

						switch cb2Attempts {
						case 1, 2:
							return someNetworkError
						default:
							return finalErr
						}
					},
				)

				if finalErr == nil {
					suite.Assert().NoError(err)
				} else {
					suite.Assert().ErrorIs(err, finalErr)
				}

				suite.Assert().Greater(cb2Attempts, 1)
				suite.Assert().Equal(cb2Attempts, cb1Attempts, "both should be retried each time")
			},
		)
	}
}

// TestMulti_LongRunningSuccess verifies that a long-running
// success won’t spuriously fail because another thread keeps
// restarting.
func (suite *UnitTestSuite) TestMulti_LongRunningSuccess() {
	ctx := suite.Context()
	logger := suite.Logger()

	startTime := time.Now()
	retryerLimit := 2 * time.Second
	retryer := New(retryerLimit)

	succeedPastTime := startTime.Add(retryerLimit + 1*time.Second)

	err := retryer.Run(
		ctx,
		logger,
		func(ctx context.Context, fi *FuncInfo) error {
			fi.NoteSuccess()

			if time.Now().Before(succeedPastTime) {
				time.Sleep(1 * time.Second)
				return someNetworkError
			}

			return nil
		},
		func(ctx context.Context, fi *FuncInfo) error {
			if time.Now().Before(succeedPastTime) {
				<-ctx.Done()
				return ctx.Err()
			}

			return nil
		},
	)

	suite.Assert().NoError(err)
}
