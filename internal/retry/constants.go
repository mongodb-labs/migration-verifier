package retry

import "time"

const (
	// DefaultDurationLimit is the default time limit for all retries.
	DefaultDurationLimit = 10 * time.Minute

	// Constants for spacing out the retry attempts.
	// See: https://en.wikipedia.org/wiki/Exponential_backoff
	//
	// The sequence, in seconds, is: 1, 2, 4, 8, 16, 16, 16, ...
	minSleepTime        = 1 * time.Second
	maxSleepTime        = 16 * time.Second
	sleepTimeMultiplier = 2
)
