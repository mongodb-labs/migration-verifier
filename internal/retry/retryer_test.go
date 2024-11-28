package retry

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/10gen/migration-verifier/internal/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func (suite *UnitTestSuite) TestRetryer() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	suite.Run("with a function that immediately succeeds", func() {
		attemptNumber := -1
		f := func(ri *Info, _ string) error {
			attemptNumber = ri.GetAttemptNumber()
			return nil
		}

		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.NoError(err)
		suite.Equal(0, attemptNumber)

		_, err = retryer.RunForUUIDErrorOnly(suite.Context(), logger, "foo", f)
		suite.NoError(err)
		suite.Equal(0, attemptNumber)

		f2 := func(ri *Info) error {
			attemptNumber = ri.GetAttemptNumber()
			return nil
		}

		err = retryer.RunForTransientErrorsOnly(suite.Context(), logger, f2)
		suite.NoError(err)
		suite.Equal(0, attemptNumber)
	})

	suite.Run("with a function that succeeds after two attempts", func() {
		attemptNumber := -1
		f := func(ri *Info, _ string) error {
			attemptNumber = ri.GetAttemptNumber()
			if attemptNumber < 2 {
				return mongo.CommandError{
					Labels: []string{"NetworkError"},
					Name:   "NetworkError",
				}
			}
			return nil
		}

		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.NoError(err)
		suite.Equal(2, attemptNumber)

		attemptNumber = -1
		f2 := func(ri *Info) error {
			attemptNumber = ri.GetAttemptNumber()
			if attemptNumber < 2 {
				return mongo.CommandError{
					Labels: []string{"NetworkError"},
					Name:   "NetworkError",
				}
			}
			return nil
		}

		err = retryer.RunForTransientErrorsOnly(suite.Context(), logger, f2)
		suite.NoError(err)
		suite.Equal(2, attemptNumber)
	})

	suite.Run("with a UUID vs name mismatch error", func() {
		attemptLimit := 4
		attemptNumber := -1
		f := func(ri *Info, _ string) error {
			attemptNumber = ri.GetNumCollectionUUIDMismatchRetries()
			if attemptNumber < attemptLimit/2 {
				// The actual name needs to change each time we retry or we'll
				// abort early.
				raw, err := bson.Marshal(bson.D{{"actualCollection", fmt.Sprintf("foo%d", attemptNumber)}})
				suite.NoError(err)
				return mongo.CommandError{
					Name: "UUIDMismatchError",
					Code: 361,
					Raw:  bson.Raw(raw),
				}
			}
			return nil
		}
		_, err := retryer.RunForUUIDErrorOnly(suite.Context(), logger, "bar", f)
		suite.NoError(err)
		suite.Equal(attemptLimit/2, attemptNumber)
	})

	suite.Run("the mismatch error returns the same name as was used", func() {
		attemptNumber := -1
		f := func(ri *Info, _ string) error {
			attemptNumber = ri.GetNumCollectionUUIDMismatchRetries()
			raw, err := bson.Marshal(bson.D{{"actualCollection", "foo"}})
			suite.NoError(err)
			return mongo.CommandError{
				Name: "UUIDMismatchError",
				Code: 361,
				Raw:  bson.Raw(raw),
			}
		}
		_, err := retryer.RunForUUIDErrorOnly(suite.Context(), logger, "bar", f)
		suite.NoError(err)
		// We only did one retry because the actual collection name matched the
		// previous attempt.
		suite.Equal(1, attemptNumber)
	})
}

func (suite *UnitTestSuite) TestRetryerDurationLimitIsZero() {
	retryer := New(0)

	attemptNumber := -1
	cmdErr := mongo.CommandError{
		Labels: []string{"NetworkError"},
		Name:   "NetworkError",
	}
	f := func(ri *Info, _ string) error {
		attemptNumber = ri.attemptNumber
		return cmdErr
	}

	_, err := retryer.RunForUUIDErrorOnly(suite.Context(), suite.Logger(), "bar", f)
	suite.Equal(cmdErr, err)
	suite.Equal(0, attemptNumber)
}

func (suite *UnitTestSuite) TestRetryerDurationReset() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	// In this test, the given function f takes longer than the durationLimit
	// to execute. (f will artificially advance the time to greater than the
	// durationLimit.)

	transientNetworkError := mongo.CommandError{
		Labels: []string{"NetworkError"},
		Name:   "NetworkError",
	}

	// 1) Not calling IterationSuccess() means f will not be retried, since the
	// durationLimit is exceeded
	noSuccessIterations := 0
	f1 := func(ri *Info, _ string) error {
		// Artificially advance how much time was taken.
		ri.durationSoFar += 2 * ri.durationLimit

		noSuccessIterations++
		if noSuccessIterations == 1 {
			return transientNetworkError
		}

		return nil
	}

	_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f1)
	// err is not nil and err is not the network error thrown by the function.
	suite.Error(err)
	suite.NotEqual(err, transientNetworkError)
	suite.Equal(1, noSuccessIterations)

	// 2) Calling IterationSuccess() means f will run more than once because the
	// duration should be reset.
	successIterations := 0
	f2 := func(ri *Info, _ string) error {
		// Artificially advance how much time was taken.
		ri.durationSoFar += 2 * ri.durationLimit

		ri.IterationSuccess()

		successIterations++
		if successIterations == 1 {
			return transientNetworkError
		}

		return nil
	}

	_, err = retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f2)
	suite.NoError(err)
	suite.Equal(2, successIterations)
}

func (suite *UnitTestSuite) TestCancelViaContext() {
	retryer := New(DefaultDurationLimit)
	logger := suite.Logger()

	counter := 0
	var wg sync.WaitGroup
	wg.Add(1)
	f := func(_ *Info, _ string) error {
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
		_, err := retryer.RunForUUIDAndTransientErrors(ctx, logger, "foo", f)
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
	f := func(ri *Info, _ string) error {
		attemptNumber = ri.GetAttemptNumber()
		if attemptNumber == 0 {
			return customError
		}
		return nil
	}

	suite.Run("with no additional error codes", func() {
		retryer := New(DefaultDurationLimit)
		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.Equal(42, util.GetErrorCode(err))
		suite.Equal(0, attemptNumber)
	})

	suite.Run("with one additional error code", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(42)
		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.NoError(err)
		suite.Equal(1, attemptNumber)
	})

	suite.Run("with multiple additional error codes", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(42, 43, 44)
		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.NoError(err)
		suite.Equal(1, attemptNumber)
	})

	suite.Run("with multiple additional error codes that don't match error", func() {
		retryer := New(DefaultDurationLimit)
		retryer = retryer.WithErrorCodes(41, 43, 44)
		_, err := retryer.RunForUUIDAndTransientErrors(suite.Context(), logger, "foo", f)
		suite.Equal(42, util.GetErrorCode(err))
		suite.Equal(0, attemptNumber)
	})
}

func (suite *UnitTestSuite) TestRetryerWithEmptyCollectionName() {
	retryer := New(DefaultDurationLimit)
	emptyCollNameError := errors.New("Empty collection name")
	f := func(_ *Info, collName string) error {
		if collName == "" {
			return emptyCollNameError
		}
		return nil
	}

	name, err := retryer.RunForUUIDErrorOnly(suite.Context(), suite.Logger(), "", f)
	suite.NoError(err)
	suite.Equal("", name)
}

// Test fix for REP-4197
func (suite *UnitTestSuite) TestV50RetryerWithUUIDNotSupportedError() {
	retryer := New(0)

	attemptNumber := 0
	cmdErr := mongo.CommandError{
		Message: "(Location4928902) collectionUUID is not supported on a mongos",
	}
	f := func(ri *Info, _ string) error {
		attemptNumber = ri.attemptNumber
		if attemptNumber == 0 {
			return cmdErr
		} else {
			return nil
		}
	}

	r := retryer.SetRetryOnUUIDNotSupported()
	r.aggregateDisallowsUUIDs = false
	_, err := r.RunForUUIDAndTransientErrors(suite.Context(), suite.Logger(), "bar", f)
	// The aggregateDisallowsUUIDs will be set to True in the retry
	suite.True(r.aggregateDisallowsUUIDs)
	suite.NoError(err)
	suite.Equal(1, attemptNumber)
}

func (suite *UnitTestSuite) TestPreV50RetryerWithUUIDNotSupportedError() {
	retryer := New(0)

	attemptNumber := 0
	cmdErr := mongo.CommandError{
		Message: "(FailedToParse) unrecognized field 'collectionUUID",
		Name:    "FailedToParseError",
		Code:    9,
	}
	f := func(ri *Info, _ string) error {
		attemptNumber = ri.attemptNumber
		if attemptNumber == 0 {
			return cmdErr
		} else {
			return nil
		}
	}

	r := retryer.SetRetryOnUUIDNotSupported()
	r.aggregateDisallowsUUIDs = false
	_, err := r.RunForUUIDAndTransientErrors(suite.Context(), suite.Logger(), "bar", f)
	// The aggregateDisallowsUUIDs will be set to True in the retry
	suite.True(r.aggregateDisallowsUUIDs)
	suite.NoError(err)
	suite.Equal(1, attemptNumber)
}
