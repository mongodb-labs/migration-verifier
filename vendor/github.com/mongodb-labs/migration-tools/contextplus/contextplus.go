// Package contextplus provides wrappers around the standard library’s context
// cancellation & expiry functions. These wrappers do some nice things:
//
//   - They mandate cancellation/expiry causes.
//   - Expiry causes contain the timeout/deadline.
//   - The returned Context’s Err() will include both Canceled/DeadlineExceeded
//     AND the Cause.
//
// The standard library would ideally do all of the above for us, but that
// would break Go’s backward compatibility with code that (alas) checks strict
// equality with Canceled or DeadlineExceeded instead of using errors.Is().
//
// # Interoperability Note
//
// Go’s style guide, as of this writing, categorically forbids custom
// context.Context implementations like this one. The rationale given, though,
// concerns interoperability between code bases: avoiding a state where calls
// to library A *must* use that library’s context implementation, while calls
// to library B require B’s context.
//
// contextplus doesn’t create that problem, though, because its changes to
// the standard library’s Context are purely informational. It goes without
// saying, of course, that publicly-accessible functions *SHOULD NOT*
// require a *contextplus.C, but should instead accept a context.Context.
// Internal functions *MAY* require a *contextplus.C for consistency.
package contextplus

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

// WithCancelCause works just like the standard library’s function of the
// same name, but the returned Context’s Err() will include both
// `context.Canceled` and the Cause.
func WithCancelCause(ctx context.Context) (*C, context.CancelCauseFunc) {
	newCtx, cancel := context.WithCancelCause(ctx)
	return New(newCtx), cancel
}

// WithDeadlineCause works just like the standard library’s function of the
// same name, but the returned context’s Err() will include both
// `context.DeadlineExceeded` and the Cause.
//
// The Cause will also be wrapped with the (stringified) deadline.
func WithDeadlineCause(
	ctx context.Context,
	deadline time.Time,
	cause error,
) (*C, context.CancelFunc) {
	wrappedCause := fmt.Errorf("deadline (%s) passed: %w", deadline, cause)
	newCtx, cancel := context.WithDeadlineCause(ctx, deadline, wrappedCause)
	return New(newCtx), cancel
}

// WithTimeoutCause is like WithDeadlineCause but for timeouts.
func WithTimeoutCause(
	ctx context.Context,
	timeout time.Duration,
	cause error,
) (*C, context.CancelFunc) {
	wrappedCause := fmt.Errorf("timed out after %s: %w", timeout, cause)
	newCtx, cancel := context.WithTimeoutCause(ctx, timeout, wrappedCause)
	return New(newCtx), cancel
}

// ErrGroup is like the standard library’s `errgroup.WithContext()`, but
// it returns a context from this package.
func ErrGroup(ctx context.Context) (*errgroup.Group, *C) {
	group, ctx2 := errgroup.WithContext(ctx)

	return group, New(ctx2)
}
