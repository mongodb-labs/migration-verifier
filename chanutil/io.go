package chanutil

import (
	"context"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
)

// ReadWithDoneCheck takes a context and a channel to read from. It will read
// from `ctx.Done()` and the given channel in a select. If it reads an error
// from the `ctx.Done()` channel, it returns the value of `Err(ctx)`, which
// includes the cancellation cause. If it reads a value from that channel, it
// returns the value. If the channel was closed, the return value is None.
func ReadWithDoneCheck[T any](ctx context.Context, ch <-chan T) (option.Option[T], error) {
	select {
	case val, ok := <-ch:
		if ok {
			return option.Some(val), nil
		}

		return option.None[T](), nil
	default:
	}

	select {
	case val, ok := <-ch:
		if ok {
			return option.Some(val), nil
		}

		return option.None[T](), nil
	case <-ctx.Done():
		return option.None[T](), util.WrapCtxErrWithCause(ctx)
	}
}

// WriteWithDoneCheck takes a context, a channel to write to, and a value to write to that
// channel. It will read from `ctx.Done()` and write to the given channel in a select. If it reads
// an error from the `ctx.Done()` channel, it returns the value of `Err(ctx)`, which includes the
// cancellation cause. If it writes a value to that channel, it returns `nil`.
func WriteWithDoneCheck[T any](ctx context.Context, ch chan<- T, val T) error {
	// 1. Try to send immediately.
	// If there is room on the channel, this succeeds and returns nil.
	// If the channel is blocked, the 'default' case catches it so we don't hang.
	select {
	case ch <- val:
		return nil
	default:
	}

	// 2. We couldn't send immediately, so now we wait for either
	// the channel to become ready OR the context to be canceled.
	select {
	case ch <- val:
		return nil
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	}
}
