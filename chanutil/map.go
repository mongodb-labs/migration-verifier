package chanutil

import (
	"context"
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
//	intChan, cancel := IngestMap(ctx, stringChan, strconv.Itoa)
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

	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return // Clean up if context is cancelled
			case val, ok := <-in:
				if !ok {
					// The caller closed the 'in' channel we gave them.
					// We just exit the goroutine. We DO NOT close 'out' (see below).
					return
				}

				// Mutate the value and push it to the provided output channel
				select {
				case <-ctx.Done():
					return
				case out <- fn(val):
				}
			}
		}
	}()

	return in, func(ctx context.Context) error {
		close(in)

		_, err := ReadWithDoneCheck(ctx, done)
		return err
	}
}
