package chanutil

import "context"

// IngestMap takes a destination channel and a mutator, and returns an input channel.
func IngestMap[In any, Out any](ctx context.Context, out chan<- Out, fn func(In) Out) chan<- In {
	// Create the channel we will hand back to the caller
	in := make(chan In)

	go func() {
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
				case out <- fn(val):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return in
}
