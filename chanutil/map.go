package chanutil

import (
	"context"

	"github.com/10gen/migration-verifier/internal/util"
)

// IngestMapCanceler defines a callback that cancels an ingest map.
// It closes the map’s input channel then blocks until the map finishes writing
// to its target channel.
type IngestMapCanceler func(context.Context) error

// StartIngestMap lets you abstract receiver channels by converting items
// from one channel into items on another channel. It starts a goroutine that
// continually reads an item, mutates it, then inserts it into another channel.
//
// For example, if you have a string channel and want a
// called function to populate it, but that function expects to send ints,
// you can do:
//
//	intChan, cancel := StartIngestMap(ctx, stringChan, strconv.Itoa)
//
//	if err := functionThatSendsInts(ctx, intChan); err != nil {
//		// .. however you handle errors
//	}
//
//	// We’re done sending ints, so cancel the ingest map:
//	if err := cancel(ctx); err != nil {
//		// .. however you handle context-expiry
//	}
func StartIngestMap[In any, Out any](
	ctx context.Context,
	out chan<- Out,
	fn func(In) Out,
) (chan<- In, IngestMapCanceler) {
	// Create the channel we will hand back to the caller
	in := make(chan In)
	done := make(chan struct{})

	// innerCtx lets the canceler unblock the goroutine even when the parent
	// context is still live. Without this, calling the canceler while the
	// goroutine is blocked on `out <- fn(val)` would deadlock.
	innerCtx, innerCancel := context.WithCancel(ctx)

	go func() {
		defer close(done)
		defer innerCancel()

		for {
			opt, err := ReadWithDoneCheck(innerCtx, in)
			if err != nil || opt.IsNone() {
				return
			}

			if WriteWithDoneCheck(innerCtx, out, fn(opt.MustGet())) != nil {
				return
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
