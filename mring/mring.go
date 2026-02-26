package mring

import (
	"container/ring"
	"sync"
)

// Queue is a thread-safe, generic wrapper around container/ring.
type Queue[T any] struct {
	mu   sync.RWMutex
	size int
	r    *ring.Ring
}

// New creates a new generic ring buffer of the specified size.
func New[T any](capacity int) *Queue[T] {
	return &Queue[T]{
		size: capacity,
		r:    ring.New(capacity),
	}
}

// Push adds an element to the ring and advances the pointer.
func (q *Queue[T]) Push(val T) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Set the current node's value, then advance to the next node
	q.r.Value = val
	q.r = q.r.Next()
}

// Get returns all populated elements in chronological order.
func (q *Queue[T]) Get() []T {
	q.mu.RLock()
	defer q.mu.RUnlock()

	out := make([]T, 0, q.size)

	// Because Push advances the pointer after writing, q.r always points
	// to the oldest element (or the next empty slot). ring.Do iterates
	// starting from this pointer, so we naturally get chronological order!
	q.r.Do(func(p any) {
		if p != nil {
			out = append(out, p.(T))
		}
	})

	return out
}
