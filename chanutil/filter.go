package chanutil

import (
	"context"
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

	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return // Clean up if context is cancelled
			case val, ok := <-in:
				if !ok {
					// The caller closed the 'in' channel we gave them.
					// We just exit the goroutine. We DO NOT close 'out'.
					return
				}

				// If the value passes the filter, push it to the output channel
				if keep(val) {
					select {
					case <-ctx.Done():
						return
					case out <- val:
					}
				}
			}
		}
	}()

	return in, func(cancelCtx context.Context) error {
		close(in)

		// Relying on the same helper function used in StartIngestMap
		_, err := ReadWithDoneCheck(cancelCtx, done)
		return err
	}
}
