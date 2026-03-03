package chanutil

import (
	"context"
)

// IngestMap lets you abstract receiver channels by mutating each item to
// something else.
//
// For example, if you have a string channel and want a
// called function to populate it, but that function expects to send ints,
// you can do:
//
//	byteChan, done := IngestMap(ctx, stringChan, strconv.Itoa)
//
//	functionThatSendsBytes(ctx, byteChan)
//
//	close(byteChan)
//	<-done
//
// The last bits are needed because this starts a goroutine internally, which
// has to shut down cleanly to avoid race conditions.
func IngestMap[In any, Out any](
	ctx context.Context,
	out chan<- Out,
	fn func(In) Out,
) (chan<- In, <-chan struct{}) {
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

	return in, done
}
