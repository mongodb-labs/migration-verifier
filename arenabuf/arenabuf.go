// Package arenabuf exposes a type that simplifies using a single backing array
// for multiple byte slices.
package arenabuf

type bufferType interface {
	~[]byte
}

// Buffer tracks multiple buffers in a single “arena” buffer.
type Buffer[T bufferType] struct {
	buf []byte
}

// Add copies a new slice to the Buffer’s internals and returns the copied
// slice.
func (b *Buffer[T]) Add(in T) T {
	start := len(b.buf)
	b.buf = append(b.buf, in...)

	return b.buf[start:]
}

// Reset creates a new backing buffer. To minimize allocations on subsequent
// Add() calls, this makes the new buffer the same size as the existing one.
func (b *Buffer[T]) Reset() {
	b.buf = make([]byte, 0, len(b.buf))
}
