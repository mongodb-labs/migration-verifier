package chanutil

import "context"

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
//	cancel(ctx)
func StartIngestFilter[T any](
	ctx context.Context,
	out chan<- T,
	keep func(T) bool,
) (chan<- T, IngestCanceler) {
	return startIngest(ctx, out, func(v T) (T, bool) {
		return v, keep(v)
	})
}
