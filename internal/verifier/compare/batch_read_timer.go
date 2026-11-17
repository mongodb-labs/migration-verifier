package compare

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BatchReadRecorder receives each batch that a BatchReadTimer measures: how
// long the cursor took to fetch it and the total size of its documents.
type BatchReadRecorder func(ctx context.Context, elapsed time.Duration, bytes int)

// BatchReadTimer measures the batches that a cursor fetches with getMore. It
// skips the cursor’s first batch, which comes with the command that opened
// the cursor.
type BatchReadTimer struct {
	record BatchReadRecorder

	// inFetchedBatch indicates that the current batch came from a getMore,
	// whose duration is elapsed. bytes totals the batch’s documents so far.
	inFetchedBatch bool
	elapsed        time.Duration
	bytes          int
}

// NewBatchReadTimer returns a BatchReadTimer that sends each batch to record.
// If record is nil, the timer measures nothing.
func NewBatchReadTimer(record BatchReadRecorder) *BatchReadTimer {
	return &BatchReadTimer{record: record}
}

// Next calls cursor.Next. If that call needs a getMore, Next times it.
func (t *BatchReadTimer) Next(ctx context.Context, cursor *mongo.Cursor) bool {
	if t.record == nil {
		return cursor.Next(ctx)
	}

	// Only read the clock when the cursor must fetch.
	if cursor.RemainingBatchLength() > 0 {
		if !cursor.Next(ctx) {
			return false
		}
	} else {
		start := time.Now()

		if !cursor.Next(ctx) {
			return false
		}

		t.inFetchedBatch = true
		t.elapsed = time.Since(start)
		t.bytes = 0
	}

	if t.inFetchedBatch {
		t.bytes += len(cursor.Current)

		if cursor.RemainingBatchLength() == 0 {
			t.record(ctx, t.elapsed, t.bytes)
			t.inFetchedBatch = false
		}
	}

	return true
}
