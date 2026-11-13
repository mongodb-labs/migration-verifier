package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// StartCheck refuses to start a check while another is running, without
// waiting for that check’s setup.
func TestStartCheck_CheckInProgress(t *testing.T) {
	verifier := &Verifier{}

	require.True(t, verifier.claimCheck(), "the first claim succeeds")
	assert.False(t, verifier.claimCheck(), "a second claim fails while the check runs")

	// Hold the lock that a check’s setup holds, to show that StartCheck
	// doesn’t wait for it.
	verifier.mux.Lock()
	defer verifier.mux.Unlock()

	assert.ErrorIs(t, verifier.StartCheck(t.Context(), func(err error) {}, nil), api.ErrCheckInProgress)
}

// A check that StartCheck started reports its failure through the function
// that StartCheck received.
func (suite *IntegrationTestSuite) TestStartCheck_ReportsFailure() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	// Make the check fail while its context is still live.
	suite.Require().NoError(verifier.metaClient.Disconnect(ctx))

	failure := make(chan error, 1)
	suite.Require().NoError(
		verifier.StartCheck(ctx, func(err error) { failure <- err }, nil),
	)

	select {
	case err := <-failure:
		suite.Assert().ErrorIs(err, mongo.ErrClientDisconnected)
	case <-time.After(time.Minute):
		suite.Require().Fail("the check should report its failure")
	}
}

// A check that fails because its context ended doesn’t report that failure:
// the context’s end, such as a shutdown, is the cause.
func (suite *IntegrationTestSuite) TestStartCheck_IgnoresFailureAfterContextEnds() {
	verifier := suite.BuildVerifier()

	ctx, cancel := context.WithCancel(suite.Context())
	defer cancel()

	failure := make(chan error, 1)
	suite.Require().NoError(
		verifier.StartCheck(ctx, func(err error) { failure <- err }, nil),
	)

	cancel()

	suite.Require().Eventually(
		func() bool { return !verifier.running.Load() },
		time.Minute,
		10*time.Millisecond,
		"the check should stop and release its claim",
	)

	// The claim is released just before StartCheck decides whether to report,
	// so give a report a moment to arrive.
	suite.Assert().Never(
		func() bool { return len(failure) > 0 },
		time.Second,
		10*time.Millisecond,
		"the check should not report a failure",
	)
}
