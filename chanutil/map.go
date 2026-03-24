package chanutil

import "context"

// StartIngestMap lets you abstract receiver channels by converting items
// from one channel into another channel. It starts a goroutine that
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
//	cancel(ctx)
func StartIngestMap[In any, Out any](
	ctx context.Context,
	out chan<- Out,
	fn func(In) Out,
) (chan<- In, IngestCanceler) {
	return startIngest(ctx, out, func(v In) (Out, bool) {
		return fn(v), true
	})
}
