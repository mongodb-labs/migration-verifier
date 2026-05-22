// Package synctools exposes concurrency-related tools.
package synctools

import "sync"

// DataGuard encapsulates a value with a mutex to ensure that anything that
// accesses it does so in a race-safe way.
//
// DataGuard can be used in many instances where otherwise you’d use a
// sync.Mutex or RWMutex to synchronize access to a value. DataGuard confers
// these benefits:
// - It prevents access to the value without the mutex.
// - It prevents folks from forgetting to unlock the mutex.
// - It (arguably) clarifies the scope over which the value is locked.
//
// Note that, in some more complex cases, though, a simple mutex may still
// work better. Use your judgement, and get feedback from coworkers if needed.
//
// &DataGuard[T]{} is usable. See NewDataGuard to initialize a DataGuard with
// a specific value.
type DataGuard[T any] struct {
	mutex sync.RWMutex
	value T
}

// NewDataGuard returns a new DataGuard that wraps the given value.
func NewDataGuard[T any](val T) *DataGuard[T] {
	return &DataGuard[T]{
		value: val,
	}
}

// Load runs the given callback, passing it the DataGuard’s stored value.
func (l *DataGuard[T]) Load(cb func(T)) {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	cb(l.value)
}

// CopyValue is like Load, but returns a copy of the value directly. This is
// useful if you can tolerate your copy going “stale”.
func (l *DataGuard[T]) CopyValue() T {
	l.mutex.RLock()
	defer l.mutex.RUnlock()

	return l.value
}

// Store is like Load but will replace the DataGuard’s stored value with the
// callback’s return.
func (l *DataGuard[T]) Store(cb func(T) T) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	l.value = cb(l.value)
}
