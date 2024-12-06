package util

import (
	"sync"

	"github.com/10gen/migration-verifier/option"
)

// Eventual represents a value that isn’t available when this struct is created
// but can be awaited via a channel.
//
// This is much like how context.Context’s Done() and Err() methods work.
// It’s useful to await a value’s readiness via channel but then read it
// multiple times.
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

// Get returns an option that contains the Eventual’s value, or
// empty if the value isn’t ready yet.
func (e *Eventual[T]) Get() option.Option[T] {
	e.mux.RLock()
	defer e.mux.RUnlock()

	return e.val
}

// Set
func (e *Eventual[T]) Set(val T) {
	e.mux.Lock()
	defer e.mux.Unlock()

	if e.val.IsSome() {
		panic("Double set on eventual!")
	}

	e.val = option.Some(val)

	close(e.ready)
}
