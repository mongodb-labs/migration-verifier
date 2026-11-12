package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	assert.ErrorIs(t, verifier.StartCheck(t.Context(), nil), api.ErrCheckInProgress)
}
