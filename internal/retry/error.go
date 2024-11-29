package retry

import (
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
)

type RetryDurationLimitExceededErr struct {
	lastErr  error
	attempts int
	duration time.Duration
}

func (rde RetryDurationLimitExceededErr) Error() string {
	return fmt.Sprintf(
		"retryable function did not succeed after %d attempt(s) over %s; last error was: %v",
		rde.attempts,
		reportutils.DurationToHMS(rde.duration),
		rde.lastErr,
	)
}

func (rde RetryDurationLimitExceededErr) Unwrap() error {
	return rde.lastErr
}
