package contextplus

import (
	"context"
	"time"

	"github.com/mongodb-labs/migration-verifier/internal/util"
)

// C stores a `context.Context`. This is the concrete type of the contexts that
// this package’s static functions return.
//
// This type is exported so that you can “upgrade” a stdlib context to have
// this type’s `Err()`. To avoid interoperability problems, this type
// *MUST NOT* be used in signatures of functions meant for external use.
//
// We explicitly _don't_ embed the `context.Context`. We don't want to
// encourage using an embedded context directly by accident.
type C struct {
	ctx context.Context
}

var _ context.Context = &C{}

// New returns a context from this package. See its `Err()` for why that’s
// useful.
func New(ctx context.Context) *C {
	return &C{ctx}
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

// Err returns the error from the given context.
//
// Unlike in the standard library, this Err() is a wrapper around both:
// - the underlying context’s Err(), and
// - the cancellation or expiry’s “cause”
//
// This usefully ensures that all cancellation error messages include the
// cancellation cause, which can simplify debugging.
//
// Unlike with the standard library’s context.Context’s `Err()`,
// you *MUST* use errors.Is() or errors.As() to introspect this method’s
// returned error. Simple equality checks against, e.g., `context.Canceled`
// will fail where they succeed against the standard library’s Context.
// Linters generally forbid simple equality checks against errors anyway,
// though, so this shouldn’t affect actively-maintained Go code.
func (c *C) Err() error {
	return util.WrapCtxErrWithCause(c.ctx)
}
