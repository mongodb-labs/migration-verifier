package synctools

import (
	"context"
	"sync/atomic"

	"github.com/mongodb-labs/migration-tools/ringbuf"
	"github.com/samber/lo"
)

// bufferedItem pairs an item with its precomputed size.
// Size is computed once on receipt and reused on drain to ensure curMem invariants.
type bufferedItem[T any] struct {
	item T
	size int64
}

// BoundedQueueStats holds snapshot statistics about a bounded queue’s internal state.
type BoundedQueueStats struct {
	BufferedItems int // Number of items currently buffered
	MaxItems      int // Maximum items allowed

	BufferedBytes int64 // Total bytes of buffered items
	MaxBytes      int64 // Maximum bytes allowed
}

type boundedQueueWorker[T any] struct {
	in       <-chan T
	out      chan<- T
	buf      *ringbuf.RingBuf[bufferedItem[T]]
	curMem   atomic.Int64 // atomic for lock-free stats queries
	size     func(T) int64
	maxCount int
	maxMem   int64
}

// NewBoundedQueue is like a channel but also lets you soft-limit the
// amount of memory to which the channel members refer. For example, instead of
// a plain chan []byte, which limits only by len(chan), use NewBoundedQueue to
// limit both by len *and* the buffers’ total size.
//
// The total-size limitation is a “soft” limit: we receive items until the
// limit is met or exceeded, then we stop receiving and drain until we’re below
// the limit again. So the channel will, in all likelihood, regularly exceed
// limit by some amount, but it will not grow without bound.
//
// This returns separate read-from & write-to channels. (Similar to io.Pipe(),
// but with channels). The size function computes a single item’s size.
//
// NOTE: The returned channels are unbuffered and WILL NOT reflect current
// usage. Use the returned stats function for lock-free stats queries.
//
// Context cancellation/expiry will cause the queue to shut down immediately,
// even if there are still items waiting to be read. In this case, those items
// may be discarded rather than sent, even if the reader is still receiving.
//
// This panics if maxCount or maxTotalSize is nonpositive, if size is nil, or
// if size returns a negative value for any item.
func NewBoundedQueue[T any](
	ctx context.Context,
	maxCount int,
	maxTotalSize int64,
	size func(T) int64,
) (<-chan T, chan<- T, func() BoundedQueueStats) {
	lo.Assertf(
		maxCount > 0,
		"maxCount (%d) must be positive",
		maxCount,
	)

	lo.Assertf(
		maxTotalSize > 0,
		"maxTotalSize (%d) must be positive",
		maxTotalSize,
	)

	lo.Assertf(
		size != nil,
		"size must not be nil",
	)

	in := make(chan T)
	out := make(chan T)

	w := &boundedQueueWorker[T]{
		in:       in,
		out:      out,
		buf:      ringbuf.New[bufferedItem[T]](maxCount),
		size:     size,
		maxCount: maxCount,
		maxMem:   maxTotalSize,
	}

	go w.run(ctx)

	statsFn := func() BoundedQueueStats {
		return BoundedQueueStats{
			BufferedItems: w.buf.Len(),
			BufferedBytes: w.curMem.Load(),
			MaxItems:      maxCount,
			MaxBytes:      maxTotalSize,
		}
	}

	return out, in, statsFn
}

func (w *boundedQueueWorker[T]) run(ctx context.Context) {
	defer close(w.out)

	for w.processItems(ctx) {
	}
}

// Returns false to indicate that all work is done: the input channel
// is closed, and the buffer is drained.
func (w *boundedQueueWorker[T]) processItems(ctx context.Context) bool {
	if w.buf.Len() == 0 {
		return w.receiveItem(ctx)
	}
	return w.receiveOrSend(ctx)
}

// Called when the buffer is empty.
// Returns false to indicate that the queue should shut down.
func (w *boundedQueueWorker[T]) receiveItem(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case item, ok := <-w.in:
		if !ok {
			return false
		}
		w.push(item)
		return true
	}
}

// Called when the buffer is nonempty.
// Returns false to indicate that the queue should shut down.
func (w *boundedQueueWorker[T]) receiveOrSend(ctx context.Context) bool {
	// If the memory or count limits constrain us, we must drain (send)
	// before receiving more.
	if w.buf.Len() == w.maxCount || w.curMem.Load() >= w.maxMem {
		return w.drainOne(ctx)
	}

	return w.receiveOrSendNormal(ctx)
}

func (w *boundedQueueWorker[T]) drainOne(ctx context.Context) bool {
	entry := w.buf.Peek()

	select {
	case <-ctx.Done():
		return false
	case w.out <- entry.item:
		w.pop()
		return true
	}
}

// receiveOrSendNormal handles the normal case when below the count limit.
func (w *boundedQueueWorker[T]) receiveOrSendNormal(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case itemIn, ok := <-w.in:
		if !ok {
			w.flushRemaining(ctx)
			return false
		}
		w.push(itemIn)
		return true
	case w.out <- w.buf.Peek().item:
		w.pop()
		return true
	}
}

func (w *boundedQueueWorker[T]) flushRemaining(ctx context.Context) {
	for w.buf.Len() > 0 {
		entry := w.buf.Peek()

		select {
		case <-ctx.Done():
			return
		case w.out <- entry.item:
			w.pop()
		}
	}
}

func (w *boundedQueueWorker[T]) itemSize(item T) int64 {
	size := w.size(item)

	lo.Assertf(
		size >= 0,
		"bounded channel item size (%d) must be nonnegative",
		size,
	)

	return size
}

// push computes size once and stores it with the item.
func (w *boundedQueueWorker[T]) push(item T) {
	sz := w.itemSize(item)
	w.buf.Push(bufferedItem[T]{item: item, size: sz})
	w.curMem.Add(sz)
}

// pop removes the next item from the buffer and updates curMem using the precomputed size.
func (w *boundedQueueWorker[T]) pop() {
	entry := w.buf.Peek()
	w.curMem.Add(-entry.size)
	w.buf.Pop()
}
