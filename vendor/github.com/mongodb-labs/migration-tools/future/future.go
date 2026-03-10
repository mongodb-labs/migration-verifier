package future

import (
	"sync"
)

// Future solves the “one writer, many readers” problem: a value gets
// written once, then the readers will see that the value is `Ready()` and
// can then `Get()` it.
//
// It’s like how `context.Context`’s `Done()` and `Err()` methods work, but
// generalized to any data type.
//
// Note that, as with Context, there is no method to set the Future’s value.
// Instead, the factory function returns a callback that sets the value.
type Future[T any] struct {
	ready chan struct{}
	val   T
	mux   sync.RWMutex
}

// Setter is the type for functions that set a Future’s value.
type Setter[T any] func(T)

// New creates a Future and returns a pointer to it as well as the Future’s
// setter function.
func New[T any]() (*Future[T], Setter[T]) {
	f := &Future[T]{ready: make(chan struct{})}

	return f, func(val T) {
		f.mux.Lock()
		defer f.mux.Unlock()

		select {
		case <-f.ready:
			panic("Tried to set a future twice!")
		default:
		}

		// NB: This *must* happen before the close(), or else a fast reader may
		// not see this value.
		f.val = val

		// This allows Get() to work:
		close(f.ready)
	}
}

// Ready returns a channel that closes once the Eventual’s value is ready.
func (f *Future[T]) Ready() <-chan struct{} {
	return f.ready
}

// Get returns the Future’s value if it’s ready.
// It panics otherwise.
func (f *Future[T]) Get() T {
	f.mux.RLock()
	defer f.mux.RUnlock()

	// If the ready channel is still open then there’s no value yet,
	// which means this method should not have been called.
	select {
	case <-f.ready:
		return f.val
	default:
		panic("Future’s Get() called before value was ready.")
	}
}
