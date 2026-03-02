// Package arenabuf exposes a type that simplifies using a single backing array
// for multiple byte slices.
package arenabuf

type bufferType interface {
	~[]byte
}

// Buffer tracks multiple buffers in a single “arena” buffer.
type Buffer[T bufferType] struct {
	buf    []byte
	slices []T
}

// Add copies a new slice to the Buffer’s internals and returns the copied
// slice.
func (b *Buffer[T]) Add(in T) T {
	start := len(b.buf)
	b.buf = append(b.buf, in...)

	newSlice := b.buf[start:]
	b.slices = append(b.slices, newSlice)

	return newSlice
}

// Slices returns the Buffer’s internally-tracked slices.
func (b *Buffer[T]) Slices() []T {
	return b.slices
}

// Reset creates a new backing buffer & slices. To minimize allocations on
// subsequent Add() calls, this makes the new buffer & slices the same size as
// the existing ones.
func (b *Buffer[T]) Reset() {
	b.buf = make([]byte, 0, len(b.buf))
	b.slices = make([]T, 0, len(b.slices))
}
