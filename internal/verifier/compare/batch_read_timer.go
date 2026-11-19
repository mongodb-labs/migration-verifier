package compare

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BatchReadStats describes a read of one batch of documents.
type BatchReadStats struct {
	// Duration is how long the read took.
	Duration time.Duration

	// Docs is the number of documents in the batch.
	Docs int

	// Bytes is the documents’ total size.
	Bytes int
}

// BatchReadStatsRecorder receives the stats for each batch that a
// BatchReadTimer measures.
type BatchReadStatsRecorder func(ctx context.Context, read BatchReadStats)

// BatchReadTimer measures the batches that a cursor fetches with getMore. It
// skips the cursor’s first batch, which comes with the command that opened
// the cursor.
type BatchReadTimer struct {
	record BatchReadStatsRecorder

	// inFetchedBatch indicates that the current batch came from a getMore.
	// read describes that batch, with its documents so far.
	inFetchedBatch bool
	read           BatchReadStats
}

// NewBatchReadTimer returns a BatchReadTimer that sends each batch to record.
// If record is nil, the timer measures nothing.
func NewBatchReadTimer(record BatchReadStatsRecorder) *BatchReadTimer {
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
		t.read = BatchReadStats{Duration: time.Since(start)}
	}

	if t.inFetchedBatch {
		t.read.Docs++
		t.read.Bytes += len(cursor.Current)

		if cursor.RemainingBatchLength() == 0 {
			t.record(ctx, t.read)
			t.inFetchedBatch = false
		}
	}

	return true
}
