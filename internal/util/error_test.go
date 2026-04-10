package util

import (
	"context"
	"fmt"
	"io"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
)

func (suite *UnitTestSuite) TestIsTransientError() {
	type testCase struct {
		err    error
		expect bool
	}
	testCases := []testCase{
		{errors.New("Not transient"), false},
		{context.Canceled, false},
		{mongo.WriteConcernError{}, false},
		{mongo.CommandError{Code: 6}, true},
		{mongo.CommandError{Code: 42}, false},
		{mongo.CommandError{Code: 175}, true},
		{mongo.CommandError{Code: 0}, false},
		{mongo.CommandError{Code: 0, Message: "not master"}, true},
		{mongo.CommandError{Code: 1234567, Labels: []string{"NetworkError"}}, true},
		{mongo.CommandError{Code: 1234567, Labels: []string{"SomeNotTransientThing"}}, false},
		{mongo.CommandError{Code: 1234567, Labels: []string{"SomeNotTransientThing", "NetworkError"}}, true},
		{mongo.CommandError{Code: 1234567, Labels: []string{"TransientTransactionError"}}, true},
		{mongo.CommandError{Code: 1234567, Labels: []string{"SomeNotTransientThing", "TransientTransactionError"}}, true},

		// Test wrapped sentinel errors - these should be transient when unwrapped correctly
		{errors.Wrap(io.EOF, "connection lost"), true},
		{errors.Wrap(io.ErrUnexpectedEOF, "unexpected close"), true},
	}
	for _, c := range testCases {
		if c.expect {
			suite.True(IsTransientError(c.err), "error should be transient: %v", c.err)
		} else {
			suite.False(IsTransientError(c.err), "error should not be transient: %v", c.err)
		}
	}
}

func (suite *UnitTestSuite) TestIsConnectionErrorWithWrappedTopologyError() {
	// Regression test: isConnectionError should detect topology.ConnectionError
	// values both directly and through wrapping.
	baseErr := topology.ConnectionError{
		Wrapped: io.EOF,
	}

	// Direct (unwrapped) error should be detected.
	suite.True(isConnectionError(baseErr), "direct ConnectionError should be detected")

	// Wrapped error should also be detected when the error chain is inspected.
	wrappedErr := errors.Wrap(baseErr, "connection issue")
	suite.True(isConnectionError(wrappedErr), "wrapped ConnectionError should be detected even when wrapped")
}

func (suite *UnitTestSuite) TestWrappedServerErrorWithTransientLabel() {
	// This test verifies the fix for the CI bug where wrapped ServerError
	// with transient labels (like "Interrupted") weren't being detected as retryable.

	// Create a ServerError with the transient "NetworkError" label
	baseErr := mongo.CommandError{
		Code:   11601, // Interrupted
		Labels: []string{"NetworkError"},
	}

	// Unwrapped error - should be transient
	suite.True(IsTransientError(baseErr), "unwrapped ServerError with NetworkError label should be transient")

	// Wrapped error - should ALSO be transient after fix
	// This is the bug from CI: the error chain was:
	// wrappedErr -> "failed to find sharding info..." -> baseErr
	wrappedErr := errors.Wrap(baseErr, "failed to find sharding info for staging.watermelon")
	suite.True(IsTransientError(wrappedErr), "wrapped ServerError with transient label should still be detected as transient")
}

func (suite *UnitTestSuite) TestErrorChainHasMessageExactness() {
	// Verify errorChainHasMessage uses exact equality, not substring matching.
	// This ensures "there are no reachable servers" does NOT match "no reachable servers"

	suite.Run("exact message match", func() {
		err := errors.New("no reachable servers")
		suite.True(errorChainHasMessage(err, "no reachable servers"))
	})

	suite.Run("substring should not match", func() {
		err := errors.New("there are no reachable servers")
		suite.False(errorChainHasMessage(err, "no reachable servers"), "substring should not match")
	})

	suite.Run("message in wrapped chain", func() {
		baseErr := errors.New("no reachable servers")
		wrappedErr := errors.Wrap(baseErr, "connection failed")
		suite.True(errorChainHasMessage(wrappedErr, "no reachable servers"), "exact match in wrapped chain should work")
	})

	suite.Run("substring in wrapped chain should not match", func() {
		baseErr := errors.New("there are no reachable servers")
		wrappedErr := errors.Wrap(baseErr, "connection failed")
		suite.False(errorChainHasMessage(wrappedErr, "no reachable servers"), "substring in wrapped chain should not match")
	})
}

func (suite *UnitTestSuite) TestIsNetworkErrorExactMessageMatching() {
	// Verify isNetworkError uses exact message matching for "no reachable servers"

	suite.Run("exact message is network error", func() {
		err := errors.New("no reachable servers")
		suite.True(isNetworkError(err), "exact 'no reachable servers' message should be network error")
	})

	suite.Run("substring is NOT network error", func() {
		err := errors.New("there are no reachable servers")
		suite.False(isNetworkError(err), "substring match should not be treated as network error")
	})

	suite.Run("wrapped exact message is network error", func() {
		baseErr := errors.New("no reachable servers")
		wrappedErr := errors.Wrap(baseErr, "failed to connect")
		suite.True(isNetworkError(wrappedErr), "wrapped exact message should be network error")
	})

	suite.Run("wrapped substring is NOT network error", func() {
		baseErr := errors.New("there are no reachable servers")
		wrappedErr := errors.Wrap(baseErr, "failed to connect")
		suite.False(isNetworkError(wrappedErr), "wrapped substring should not be network error")
	})
}

func (suite *UnitTestSuite) TestErrorChainHasMessageWithMultipleWrappedErrors() {
	// Verify errorChainHasMessage supports fmt.Errorf("%w: %w"), which creates
	// errors with multiple wrapped errors via Unwrap() []error.

	suite.Run("single wrapped error chain", func() {
		baseErr := errors.New("no reachable servers")
		wrapped := fmt.Errorf("connection failed: %w", baseErr)
		suite.True(errorChainHasMessage(wrapped, "no reachable servers"))
	})

	suite.Run("multiple wrapped errors - message in first position", func() {
		err1 := errors.New("no reachable servers")
		err2 := errors.New("timeout")
		// This creates an error with multiple wrapped errors
		combined := fmt.Errorf("%w: %w", err1, err2)
		suite.True(errorChainHasMessage(combined, "no reachable servers"),
			"should find 'no reachable servers' in multiple wrapped errors")
	})

	suite.Run("multiple wrapped errors - message in second position", func() {
		err1 := errors.New("connection lost")
		err2 := errors.New("no reachable servers")
		// This creates an error with multiple wrapped errors
		combined := fmt.Errorf("%w: %w", err1, err2)
		suite.True(errorChainHasMessage(combined, "no reachable servers"),
			"should find 'no reachable servers' even when it's not the first wrapped error")
	})

	suite.Run("deeply nested multiple wrapped errors", func() {
		err1 := errors.New("no reachable servers")
		err2 := errors.New("timeout")
		combined := fmt.Errorf("%w: %w", err1, err2)
		wrapped := fmt.Errorf("failed to connect: %w", combined)
		suite.True(errorChainHasMessage(wrapped, "no reachable servers"),
			"should find message in deeply nested multiple wrapped errors")
	})
}
