package synctools

import (
	"sync/atomic"
)

// AtomicMax stores a concurrently-tracked max value.
// Multiple goroutines can safely write & read from this struct.
type AtomicMax[T any] struct {
	ptr      atomic.Pointer[T]
	comparer func(a, b T) int
}

// NewAtomicMax initializes the tracker.
// The comparer function works the same way as cmp.Compare.
func NewAtomicMax[T any](
	initial T,
	comparer func(a, b T) int,
) *AtomicMax[T] {
	if comparer == nil {
		panic("synctools: nil comparer passed to NewAtomicMax")
	}

	am := &AtomicMax[T]{
		comparer: comparer,
	}
	val := initial
	am.ptr.Store(&val)
	return am
}

// Update concurrently checks and sets the new max if it is greater.
// Returns the previous max value (or the zero value of T if there was none).
func (a *AtomicMax[T]) Update(val T) T {
	for {
		currPtr := a.ptr.Load()

		// Dereference currPtr to get the actual value, then call comparer.
		if currPtr != nil && a.comparer(*currPtr, val) >= 0 {
			return *currPtr
		}

		// Allocate a new unique memory address for the CAS operation
		newVal := val
		if a.ptr.CompareAndSwap(currPtr, &newVal) {
			if currPtr == nil {
				var zero T
				return zero
			}
			return *currPtr
		}
	}
}

// Get returns the current maximum value.
func (a *AtomicMax[T]) Get() T {
	currPtr := a.ptr.Load()
	if currPtr == nil {
		var zero T
		return zero
	}
	return *currPtr
}
