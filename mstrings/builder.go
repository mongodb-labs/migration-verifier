package mstrings

import (
	"strings"
	"sync"
)

// SyncBuilder is a race-safe version of the standard library’s strings.Builder.
// As of this writing it doesn’t implement all of the standard library’s logic.
type SyncBuilder struct {
	mutex   sync.RWMutex
	builder strings.Builder
}

func (b *SyncBuilder) String() string {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	return b.builder.String()
}

func (b *SyncBuilder) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.builder.Write(p)
}
