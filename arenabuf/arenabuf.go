// Package arenabuf exposes a type that simplifies using a single backing array
// for multiple byte slices.
package arenabuf

type bufferType interface {
	~[]byte
}

// Buffer tracks multiple buffers in a single “arena” buffer.
type Buffer[T bufferType] struct {
	buf      []byte
	pointers []T
}

// Add copies a new slice to the Buffer’s internals.
func (b *Buffer[T]) Add(in T) T {
	start := len(b.buf)
	b.buf = append(b.buf, in...)
	newBuf := b.buf[start:]

	b.pointers = append(b.pointers, newBuf)

	return newBuf
}

// Slices returns the Buffer’s internal slices. It *does not* copy the buffer.
func (b *Buffer[T]) Slices() []T {
	return b.pointers
}

func (b *Buffer[T]) Reset() {
	b.buf = b.buf[:0]
	b.pointers = b.pointers[:0]
}
