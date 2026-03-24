package chanutil

import (
	"context"

	"github.com/10gen/migration-verifier/internal/util"
)

// IngestFilterCanceler defines a callback that cancels an ingest filter.
// It closes the filter’s input channel then blocks until the filter finishes writing
// to its target channel.
type IngestFilterCanceler func(context.Context) error

// StartIngestFilter lets you abstract receiver channels by filtering items
// from one channel into another channel. It starts a goroutine that continually
// reads an item, tests it against a predicate function, and if the function
// returns true, inserts it into another channel.
//
// For example, if you want a called function to populate an int channel,
// but you only want to keep the even numbers:
//
//	intChan, cancel := StartIngestFilter(ctx, outChan, func(i int) bool {
//		return i%2 == 0
//	})
//
//	if err := functionThatSendsInts(ctx, intChan); err != nil {
//		// .. however you handle errors
//	}
//
//	// We’re done sending ints, so cancel the ingest filter:
//	if err := cancel(ctx); err != nil {
//		// .. however you handle context-expiry
//	}
func StartIngestFilter[T any](
	ctx context.Context,
	out chan<- T,
	keep func(T) bool,
) (chan<- T, IngestFilterCanceler) {
	// Create the channel we will hand back to the caller
	in := make(chan T)
	done := make(chan struct{})

	// innerCtx lets the canceler unblock the goroutine even when the parent
	// context is still live. Without this, calling the canceler while the
	// goroutine is blocked on `out <- val` would deadlock: the canceler waits
	// for `done`, but the goroutine only unblocks if `out` is consumed or
	// `ctx` is cancelled.
	innerCtx, innerCancel := context.WithCancel(ctx)

	go func() {
		defer close(done)
		defer innerCancel()

		for {
			opt, err := ReadWithDoneCheck(innerCtx, in)
			if err != nil || opt.IsNone() {
				return
			}

			val := opt.MustGet()
			if keep(val) {
				if WriteWithDoneCheck(innerCtx, out, val) != nil {
					return
				}
			}
		}
	}()

	return in, func(cancelCtx context.Context) error {
		// Signal the goroutine that no more items are coming. Under normal
		// conditions this is sufficient: the goroutine drains its current item
		// (if any), sees the close on the next outer-select iteration, and exits.
		close(in)

		// Wait for the goroutine to finish. If cancelCtx expires first it means
		// the goroutine is stuck writing to `out` (e.g. the consumer stopped
		// reading). In that case force-cancel via innerCtx so both select
		// branches unblock, then wait unconditionally for the goroutine to exit.
		select {
		case <-done:
			return nil
		case <-cancelCtx.Done():
			innerCancel()
			<-done
			return util.WrapCtxErrWithCause(cancelCtx)
		}
	}
}
