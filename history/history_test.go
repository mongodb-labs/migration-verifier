package history

import (
	"slices"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
)

func TestHistory(t *testing.T) {
	h := New[int](time.Hour)

	assert.Equal(t, 1, h.Add(234))
	assert.Equal(t, 2, h.Add(234))
	assert.Equal(t, 3, h.Add(345))

	got := h.Get()
	times, data := splitLogs(got)
	assert.True(
		t,
		slices.IsSortedFunc(times, time.Time.Compare),
		"times should be increasing",
	)
	assert.Equal(t, mslices.Of(234, 234, 345), data, "data as expected")

	got[0].Datum = 999
	got = h.Get()
	_, data = splitLogs(got)
	assert.Equal(t, mslices.Of(234, 234, 345), data, "slice is copied")
}

func splitLogs[T any](in []Log[T]) ([]time.Time, []T) {
	var times []time.Time
	var data []T

	for _, cur := range in {
		times = append(times, cur.At)
		data = append(data, cur.Datum)
	}

	return times, data
}
