package util

import (
	"sync"
)

// Eventual solves the “one writer, many readers” problem: a value gets
// written once, then the readers will see that the value is `Ready()` and
// can then `Get()` it.
//
// It’s like how `context.Context`’s `Done()` and `Err()` methods work, but
// generalized to any data type.
type Eventual[T any] struct {
	ready chan struct{}
	val   T
	mux   sync.RWMutex
}

// NewEventual creates an Eventual and returns a pointer
// to it.
func NewEventual[T any]() *Eventual[T] {
	return &Eventual[T]{
		ready: make(chan struct{}),
	}
}

// Ready returns a channel that closes once the Eventual’s value is ready.
func (e *Eventual[T]) Ready() <-chan struct{} {
	return e.ready
}

// Get returns the Eventual’s value if it’s ready.
// It panics otherwise.
func (e *Eventual[T]) Get() T {
	e.mux.RLock()
	defer e.mux.RUnlock()

	// If the ready channel is still open then there’s no value yet,
	// which means this method should not have been called.
	select {
	case <-e.ready:
		return e.val
	default:
		panic("Eventual's Get() called before value was ready.")
	}
}

// Set sets the Eventual’s value. It may be called only once;
// if called again it will panic.
func (e *Eventual[T]) Set(val T) {
	e.mux.Lock()
	defer e.mux.Unlock()

	select {
	case <-e.ready:
		panic("Tried to set an eventual twice!")
	default:
	}

	// NB: This *must* happen before the close(), or else a fast reader may
	// not see this value.
	e.val = val

	// This allows Get() to work:
	close(e.ready)
}
