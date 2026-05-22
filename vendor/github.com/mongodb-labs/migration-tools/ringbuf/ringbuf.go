// Package ringbuf provides a generic, fixed-capacity, array-backed ring buffer.
package ringbuf

import (
	"sync/atomic"

	"github.com/samber/lo"
)

// RingBuf is a generic, fixed-capacity, array-backed ring buffer.
// Some operations are concurrency-safe, but NOT all. See methods docs.
type RingBuf[T any] struct {
	buf   []T
	head  int
	tail  int
	count atomic.Int64
}

// New allocates a RingBuf with the given fixed capacity.
// It allocates the fixed backing storage up front; subsequent buffer
// operations do not grow the buffer or allocate additional backing storage.
func New[T any](capacity int) *RingBuf[T] {
	if capacity <= 0 {
		panic("ringbuf: capacity must be > 0")
	}
	return &RingBuf[T]{buf: make([]T, capacity)}
}

// Len returns the number of items currently in the buffer.
// Safe for concurrent reads.
func (r *RingBuf[T]) Len() int {
	return int(r.count.Load())
}

// Cap returns the maximum capacity of the buffer.
// Safe for concurrent reads.
func (r *RingBuf[T]) Cap() int {
	return len(r.buf)
}

// Push adds an item to the back of the buffer.
// Panics if the buffer is full.
//
// NOT concurrency-safe.
func (r *RingBuf[T]) Push(item T) {
	lo.Assertf(
		r.count.Load() < int64(len(r.buf)),
		"buffer must be non-full",
	)

	r.buf[r.tail] = item
	r.tail = (r.tail + 1) % len(r.buf)
	r.count.Add(1)
}

// Peek returns the oldest item in the buffer without removing it.
// Panics if the buffer is empty.
//
// NOT concurrency-safe.
func (r *RingBuf[T]) Peek() T {
	lo.Assert(
		r.count.Load() != 0,
		"peek needs nonempty buffer",
	)

	return r.buf[r.head]
}

// Pop removes the oldest item from the buffer.
// The removed slot is zeroed to allow GC to collect the item.
// Panics if the buffer is empty.
//
// NOT concurrency-safe.
func (r *RingBuf[T]) Pop() {
	lo.Assert(
		r.count.Load() != 0,
		"pop needs nonempty buffer",
	)

	var zero T
	r.buf[r.head] = zero // release reference for GC
	r.head = (r.head + 1) % len(r.buf)
	r.count.Add(-1)
}
