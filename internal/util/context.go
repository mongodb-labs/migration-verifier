package util

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
)

// WrapCtxErrWithCause returns the error from the given context.
//
// Unlike in the standard library, this WrapCtxErrWithCause() is a wrapper around:
// - the underlying context’WrapCtxErrWithCauserr(), and
// - the "cause" from a cancellation, timeout, or deadline
//
// Thus, errors.Is() will still work against built-in errors
// (e.g., context.Canceled).
//
// NB: This is copied from mongosync.
func WrapCtxErrWithCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	err := ctx.Err() //nolint:gocritic

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

	// This can happen if the given ctx is a contextplus, or some other
	// context.Context immplementation that includes the cause in the
	// context’s Err().
	if errors.Is(err, cause) {
		return err
	}

	return fmt.Errorf("%w: %w", err, cause)
}
