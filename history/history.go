package history

import (
	"slices"
	"sync"
	"time"

	"github.com/samber/lo"
	"golang.org/x/exp/constraints"
)

// History stores an ordered list of entries, each with a TTL (time-to-live).
// Once an entry expires, it goes away.
//
// This facilitates computation of data flow rates across batches.
type History[T any] struct {
	mu   sync.RWMutex
	ttl  time.Duration
	logs []Log[T]
}

// Log represents a single entry in a History.
type Log[T any] struct {
	At    time.Time
	Datum T
}

// New creates & returns a new History.
func New[T any](ttl time.Duration) *History[T] {
	return &History[T]{
		ttl: ttl,
	}
}

// Get returns a copy of the History’s (non-expired) elements.
func (h *History[T]) Get() []Log[T] {
	h.mu.RLock()
	defer h.mu.RUnlock()

	now := time.Now()

	return slices.Clone(h.logs[h.getFirstValidIdxWhileLocked(now):])
}

// Add augments the History’s Log list. It returns the list’s count of
// (non-expired) elements.
func (h *History[T]) Add(datum T) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	h.reapWhileLocked(now)

	h.logs = append(h.logs, Log[T]{now, datum})

	return len(h.logs)
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

type realNumber interface {
	constraints.Integer | constraints.Float
}

// SumLogs adds up each log’s Datum & returns the result.
// (This only works, of course, for real number types.)
func SumLogs[T realNumber](l []Log[T]) T {
	var sum T

	for _, log := range l {
		sum += log.Datum
	}

	return sum
}

// RatePer gives a rate per unit time.
func RatePer[T realNumber](logs []Log[T], dur time.Duration) float64 {
	lo.Assert(dur > 0, "duration must be nonzero")

	if len(logs) == 0 {
		return 0
	}

	denom := float64(time.Since(logs[0].At)) / float64(dur)

	lo.Assert(denom > 0, "time.Since should never be 0")

	return float64(SumLogs(logs)) / denom
}
