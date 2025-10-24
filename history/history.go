package history

import (
	"slices"
	"sync"
	"time"
)

// Log represents a single entry in a History.
type Log[T any] struct {
	At    time.Time
	Datum T
}

// History represents a series of expiring log entries.
type History[T any] struct {
	mu   sync.RWMutex
	ttl  time.Duration
	logs []Log[T]
}

// New creates & returns a new History.
func New[T any](ttl time.Duration) *History[T] {
	return &History[T]{
		ttl: ttl,
	}
}

// Add augments the History’s Log list.
func (h *History[T]) Add(datum T) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	h.reapWhileLocked(now)

	h.logs = append(h.logs, Log[T]{now, datum})
}

func (h *History[T]) AddAndGet(datum T) []Log[T] {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	h.reapWhileLocked(now)

	return slices.Clone(h.logs)
}

// Get returns a copy of the History’s Logs.
func (h *History[T]) Get() []Log[T] {
	h.mu.RLock()
	defer h.mu.RUnlock()

	now := time.Now()

	return slices.Clone(h.logs[h.getFirstValidIdxWhileLocked(now):])
}

// NB: If all entries are invalid this returns len(logs).
func (h *History[T]) getFirstValidIdxWhileLocked(now time.Time) int {
	cutoff := now.Add(-h.ttl)

	for i, logItem := range h.logs {
		if logItem.At.Before(cutoff) {
			continue
		}

		return i
	}

	// We only get here if all logs are stale.
	return len(h.logs)
}

func (h *History[T]) reapWhileLocked(now time.Time) {
	h.logs = h.logs[h.getFirstValidIdxWhileLocked(now):]
}
