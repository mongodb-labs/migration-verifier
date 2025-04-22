package util

import (
	"sync"

	"github.com/mongodb-labs/migration-verifier/option"
)

// Eventual solves the “one writer, many readers” problem: a value gets
// written once, then the readers will see that the value is `Ready()` and
// can then `Get()` it.
//
// It’s like how `context.Context`’s `Done()` and `Err()` methods work, but
// generalized to any data type.
type Eventual[T any] struct {
	ready chan struct{}
	val   option.Option[T]
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

	val, has := e.val.Get()
	if has {
		return val
	}

	panic("Eventual's Get() called before value was ready.")
}

// Set sets the Eventual’s value. It may be called only once;
// if called again it will panic.
func (e *Eventual[T]) Set(val T) {
	e.mux.Lock()
	defer e.mux.Unlock()

	if e.val.IsSome() {
		panic("Tried to set an eventual twice!")
	}

	// NB: This *must* happen before the close(), or else a fast reader may
	// not see this value.
	e.val = option.Some(val)

	close(e.ready)
}
