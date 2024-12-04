package msync

import "sync/atomic"

// TypedAtomic is a type-safe wrapper around the standard-library atomic.Value.
// TypedAtomic serves largely the same purpose as atomic.Pointer but stores
// the value itself rather than a pointer to it. This is often more ergonomic
// than an atomic.Pointer: it can be used to store constants directly (where
// taking a pointer is inconvenient), and it defaults to the type's zero value
// rather than a nil pointer.
type TypedAtomic[T any] struct {
	v atomic.Value
}

// NewTypedAtomic returns a new TypedAtomic, initialized to val.
func NewTypedAtomic[T any](val T) *TypedAtomic[T] {
	var v atomic.Value
	v.Store(val)
	return &TypedAtomic[T]{v}
}

// Load returns the value set by the most recent Store. It returns the zero
// value for the type if there has been no call to Store.
func (ta *TypedAtomic[T]) Load() T {
	return orZero[T](ta.v.Load())
}

// Store sets the value TypedAtomic to val. Store(nil) panics.
func (ta *TypedAtomic[T]) Store(val T) {
	ta.v.Store(val)
}

// Swap stores newVal into the TypedAtomic and returns the previous value. It
// returns the zero value for the type if the value is empty.
func (ta *TypedAtomic[T]) Swap(newVal T) T {
	return orZero[T](ta.v.Swap(newVal))
}

// CompareAndSwap executes the compare-and-swap operation for the TypedAtomic.
func (ta *TypedAtomic[T]) CompareAndSwap(oldVal, newVal T) bool {
	return ta.v.CompareAndSwap(oldVal, newVal)
}

func orZero[T any](val any) T {
	if val == nil {
		return *new(T)
	}

	return val.(T)
}
