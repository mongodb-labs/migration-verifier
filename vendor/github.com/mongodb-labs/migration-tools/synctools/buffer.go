package synctools

import (
	"bytes"
	"io"
	"slices"
	"sync"
)

// Buffer is a race-safe version of bytes.Buffer. It must never be copied.
//
// Because this is meant for concurrent use, not all Buffer methods are
// implemented. In particular, io.Reader is not implemented.
//
// The ideal use for this is in testing, e.g., to capture logs from concurrent
// goroutines.
type Buffer struct {
	mutex   sync.RWMutex
	builder bytes.Buffer
}

var _ io.Writer = &Buffer{}

// Bytes is like bytes.Buffer.Bytes, but the returned slice is a clone.
// Because of this, further Buffer modifications DO NOT affect slices
// that this function returns.
func (b *Buffer) Bytes() []byte {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	return slices.Clone(b.builder.Bytes())
}

// Write implements io.Writer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.builder.Write(p)
}

// Reset resets the underlying buffer.
func (b *Buffer) Reset() {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.builder.Reset()
}
