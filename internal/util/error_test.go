package util

import (
	"context"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/mongo"
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
	}
	for _, c := range testCases {
		if c.expect {
			suite.True(IsTransientError(c.err))
		} else {
			suite.False(IsTransientError(c.err))
		}
	}
}
