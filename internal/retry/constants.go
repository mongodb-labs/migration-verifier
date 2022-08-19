package retry

import "time"

const (
	// DefaultDurationLimit is the default time limit for all retries. This is
	// currently 10 minutes.
	DefaultDurationLimit = 10 * time.Minute

	// The number of CollectionUUIDMismatch error retries we do
	// before logging a warning. Under normal circumstances, we
	// don't ever expect more than a couple. Anything more than
	// that requires collection renames to interleave between
	// retry attempts.
	numCollectionUUIDRetriesBeforeWarning = 10

	// Constants for spacing out the retry attempts.
	// See: https://en.wikipedia.org/wiki/Exponential_backoff
	//
	// The sequence, in seconds, is: 1, 2, 4, 8, 16, 16, 16, ...
	minSleepTime        = 1 * time.Second
	maxSleepTime        = 16 * time.Second
	sleepTimeMultiplier = 2
)
