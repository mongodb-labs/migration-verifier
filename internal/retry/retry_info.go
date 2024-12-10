package retry

import (
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	"github.com/rs/zerolog"
)

// LoopInfo stores information relevant to the retrying done. It should
// primarily be used within the closure passed to the retry helpers.
//
// The attempt number is 0-indexed (0 means this is the first attempt).
// The duration tracks the duration of retrying for transient errors only.
type LoopInfo struct {
	attemptsSoFar int
	durationLimit time.Duration
}

type lastResetInfo struct {
	time time.Time

	// These go into logs to facilitate debugging.
	description option.Option[string]
	resetsSoFar uint64
}

type FuncInfo struct {
	loopInfo        *LoopInfo
	description     string
	loopDescription option.Option[string]

	lastReset *msync.TypedAtomic[lastResetInfo]
}

// Log will log a debug-level message for the current Info values and the provided strings.
//
// The clientType should be either "source" or "destination" corresponding to the cluster that
// the command applies to. The msg provides additional context on the function being retried.
// Parameters that don't apply can be empty strings.
//
// Useful for keeping track of DDL commands that access/change the cluster in some way.
// Generally not recommended for CRUD commands, which may result in too many log lines.
func (fi *FuncInfo) Log(logger *zerolog.Logger, cmdName string, clientType string, database string, collection string, msg string) {
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
		Int("attemptNumber", fi.GetAttemptNumber()).
		Str("durationSoFar", reportutils.DurationToHMS(fi.GetDurationSoFar())).
		Str("durationLimit", reportutils.DurationToHMS(fi.loopInfo.durationLimit)).
		Msg("Running retryable function")
}

// GetAttemptNumber returns the Info's current attempt number (0-indexed).
func (fi *FuncInfo) GetAttemptNumber() int {
	return fi.loopInfo.attemptsSoFar
}

// GetDurationSoFar returns the Info's current duration so far. This duration
// applies to the duration of retrying for transient errors only.
func (fi *FuncInfo) GetDurationSoFar() time.Duration {
	return time.Since(fi.lastReset.Load().time)
}

// NoteSuccess is used to tell the retry util to reset its measurement
// of how long the closure has been running for. This is useful for long
// running operations that might run successfully for a few days and then fail.
//
// Call this after every successful command in a multi-command callback.
// (It’s useless--but harmless--in a single-command callback.)
func (i *FuncInfo) NoteSuccess(description string, descArgs ...any) {
	totalResets := i.lastReset.Load().resetsSoFar

	i.lastReset.Store(lastResetInfo{
		description: option.Some(fmt.Sprintf(description, descArgs...)),
		time:        time.Now(),
		resetsSoFar: 1 + totalResets,
	})
}

func (i *FuncInfo) GetDescriptions() []string {
	descriptions := mslices.Of(i.description)
	if loopDesc, hasDesc := i.loopDescription.Get(); hasDesc {
		descriptions = slices.Insert(descriptions, 0, loopDesc)
	}

	return descriptions
}
