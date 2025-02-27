// contextplus builds on Go’s built-in context.Context implementation.
// Most notably, the context provided by this package includes
// cancellation/expiry cause in the error returned by `ctx.Err()`.
//
// # Cancellations & Deadlines
//
// The standard library’s `context` historically exposed static functions
// `WithCancel`, `WithTimeout`, and `WithDeadline` to cancel or expire
// contexts. When a context was canceled or expired, its Err() method would
// return context.Canceled or context.DeadlineExceeded, respectively.
//
// The problem with this is that those errors offered no additional
// information; i.e., an error that just says `context canceled` doesn’t tell
// you _why_ the context was canceled.  Go 1.20 & 1.21 addressed this by
// introducing functions `WithCancelCause`, `WithTimeoutCause`, and
// `WithDeadlineCause`. These mimic the non-`Cause` methods but additionally
// require an error to give additional information about the
// cancellation/expiry. That secondary error is available via the
// `context.Cause()` function.
//
// This works but is a bit cumbersome: existing code all has to be updated to
// call Cause() in order to fetch the additional information. It would seem
// more ideal for the "cause" to be part of the error that Err() returns, but
// the standard library couldn’t do that without breaking Go 1.x compatibility
// guarantees.
//
// This package, not having that limitation, solves this a bit more elegantly:
// its `WithCancel`, `WithTimeout`, and `WithDeadline` methods all require a
// "cause", and its Err() returns a wrapper error that incorporates both the
// original error (e.g., context.Canceled) _and_ the "cause". As a
// convenience, WithTimeout() and WithDeadline() include the timeout/deadline
// in their error wrappers.
//
// Note that this requires any 3rd-party code we may call (e.g., MongoDB’s Go
// driver) to compare errors via `errors.Is` rather than strict equality. This
// seems reasonable, though, given Go’s longstanding preference for comparing
// errors that way versus strict equalty.
//
// # Interoperability
//
// Go’s style guide, as of this writing, categorically forbids custom
// context.Context implementations like this one. The rationale given, though,
// concerns interoperabililty between code bases: avoiding a state where calls
// to library A *must* use that library’s context implementation, while calls
// to library B need to use B’s context.
//
// contextplus doesn’t create that problem, though, because its changes to
// the standard library’s Context are purely informational. It goes without
// saying, of course, that publicly-accessible functions *SHOULD NOT*
// require a contextplus, but should instead accept a context.Context.

package contextplus

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
)

// C wraps a `context.Context`.
//
// We explicitly _don't_ embed the `context.Context`. We don't want to
// encourage using an embedded context directly by accident.
type C struct {
	ctx context.Context
}

// New creates a context that wraps the given one.
func New(ctx context.Context) *C {
	return &C{ctx}
}

// Background is a convenience alias for New(context.Background()).
func Background() *C {
	return New(context.Background())
}

// Deadline returns the wrapped context’s deadline.
func (c *C) Deadline() (deadline time.Time, ok bool) {
	return c.ctx.Deadline()
}

// Done returns the wrapped context’s Done channel.
func (c *C) Done() <-chan struct{} {
	return c.ctx.Done()
}

// Value returns the requested value from the wrapped context.
func (c *C) Value(key any) any {
	return c.ctx.Value(key)
}

// WithCancel works just like the stdlib’s `context.WithCancelCause`, but the
// returned context is a *C.
func (c *C) WithCancel() (*C, context.CancelCauseFunc) {
	newCtx, cancel := context.WithCancelCause(c.ctx)
	return New(newCtx), cancel
}

// WithDeadline works like the stdlib’s `context.WithDeadlineCause`, but the
// returned context is a *C.
//
// This also wraps the given cause with the stringified deadline.
func (c *C) WithDeadline(deadline time.Time, cause error) (*C, context.CancelFunc) {
	wrappedCause := errors.Wrapf(cause, "deadline (%s) passed", deadline)
	newCtx, cancel := context.WithDeadlineCause(c.ctx, deadline, wrappedCause)
	return New(newCtx), cancel
}

// WithTimeout works like the stdlib’s `context.WithTimeoutCause`, but the
// returned context is a *C.
//
// This also wraps the given cause with the stringified timeout.
func (c *C) WithTimeout(timeout time.Duration, cause error) (*C, context.CancelFunc) {
	wrappedCause := errors.Wrapf(cause, "timed out after %s", timeout)
	newCtx, cancel := context.WithTimeoutCause(c.ctx, timeout, wrappedCause)
	return New(newCtx), cancel
}

// Err returns the error from the given context.
//
// Unlike in the standard library, this Err() is a wrapper around both:
// - the underlying context’s Err(), and
// - the cancellation or expiry’s “cause”
//
// Unlike with the standard library’s context.Context’s `Err()`,
// you *MUST* use errors.Is() to introspect this method’s returned error
// rather than checking simple equality. Linters generally enforce that
// anyhow, so it shouldn’t be a problem in actively-maintained Go code.
func (c *C) Err() error {
	cause := context.Cause(c.ctx)
	err := c.ctx.Err()

	if cause == nil {
		return err
	}

	// Usually the cause is distinct from the stdlib’s error. Sometimes, though,
	// you might have something like
	// `cancel(fmt.Errorf("All done (%w)", context.Canceled))`. In this case
	// the cause wraps the stdlib error, so there’s no point in wrapping again.
	// (It reads oddly: `context canceled: All done (context canceled)`.)
	//
	// Thus, we just return the cause here.
	if errors.Is(cause, err) {
		return cause
	}

	return fmt.Errorf("%w: %w", err, cause)
}
