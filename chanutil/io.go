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
	case <-ctx.Done():
		return option.None[T](), util.WrapCtxErrWithCause(ctx)
	case val, ok := <-ch:
		if ok {
			return option.Some(val), nil
		}

		return option.None[T](), nil
	}
}

// WriteWithDoneCheck takes a context, a channel to write to, and a value to write to that
// channel. It will read from `ctx.Done()` and write to the given channel in a select. If it reads
// an error from the `ctx.Done()` channel, it returns the value of `Err(ctx)`, which includes the
// cancellation cause. If it writes a value to that channel, it returns `nil`.
func WriteWithDoneCheck[T any](ctx context.Context, ch chan<- T, val T) error {
	select {
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	case ch <- val:
		return nil
	}
}
