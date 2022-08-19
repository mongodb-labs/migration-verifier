package retry

import (
	"time"

	"github.com/rs/zerolog"
)

//
// Info stores information relevant to the retrying done. It should
// primarily be used within the closure passed to the retry helpers.
//
// The attempt number is 0-indexed (0 means this is the first attempt).
// The duration tracks the duration of retrying for transient errors only.
//
type Info struct {
	attemptNumber int

	durationSoFar time.Duration
	durationLimit time.Duration

	// Used to reset the time elapsed for long running operations.
	shouldResetDuration bool

	// Mostly useful for testing.
	numCollectionUUIDRetries int
}

// Log will log a debug-level message for the current Info values and the provided strings.
//
// The clientType should be either "source" or "destination" corresponding to the cluster that
// the command applies to. The msg provides additional context on the function being retried.
// Parameters that don't apply can be empty strings.
//
// Useful for keeping track of DDL commands that access/change the cluster in some way.
// Generally not recommended for CRUD commands, which may result in too many log lines.
func (ri *Info) Log(logger *zerolog.Logger, cmdName string, clientType string, database string, collection string, msg string) {
	// Don't log if no logger is provided. Mostly useful for
	// integration tests where we don't want additional logs.
	if logger == nil {
		return
	}

	event := logger.Debug()
	if cmdName != "" {
		event.Str("command", cmdName)
	}
	if clientType != "" {
		event.Str("client", clientType)
	}
	if database != "" {
		event.Str("database", database)
	}
	if collection != "" {
		event.Str("collection", collection)
	}
	event.Str("context", msg).
		Int("attemptNumber", ri.attemptNumber).
		Int("durationSoFar (seconds)", int(ri.durationSoFar.Seconds())).
		Int("durationLimit (seconds)", int(ri.durationLimit.Seconds())).
		Msg("Running retryable function")
}

// GetAttemptNumber returns the Info's current attempt number (0-indexed).
func (ri *Info) GetAttemptNumber() int {
	return ri.attemptNumber
}

// GetDurationSoFar returns the Info's current duration so far. This duration
// applies to the duration of retrying for transient errors only.
func (ri *Info) GetDurationSoFar() time.Duration {
	return ri.durationSoFar
}

// GetNumCollectionUUIDMismatchRetries returns the number of retries for
// CollectionUUIDMismatch errors so far. Mostly useful for testing.
func (ri *Info) GetNumCollectionUUIDMismatchRetries() int {
	return ri.numCollectionUUIDRetries
}

// IterationSuccess is used to tell the retry util to reset its measurement
// of how long the closure has been running for. This is useful for long
// running operations that might run successfully for a few days and then fail.
// Essentially, calling this function tells the retry util not to include the
// closure's run time as a part of the overall measurement of how long the
// closure took including retries, since that measurement is used to determine
// whether we want to retry the operation or not. (If the measurement is greater
// than the retry time, we will not retry.)
func (ri *Info) IterationSuccess() {
	ri.shouldResetDuration = true
}
