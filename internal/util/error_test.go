package util

import (
	"context"
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

func (suite *UnitTestSuite) TestDirectCallToIsConnectionErrorWithWrappedTopologyError() {
	// This test exposes a bug: isConnectionError uses direct type assertion
	// which doesn't work with wrapped topology.ConnectionError.
	baseErr := topology.ConnectionError{
		Wrapped: io.EOF,
	}

	// Direct (unwrapped) error - this works
	suite.True(isConnectionError(baseErr), "direct ConnectionError should be detected")

	// Wrapped error - this fails because isConnectionError uses direct type
	// assertion instead of stderrors.AsType
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
