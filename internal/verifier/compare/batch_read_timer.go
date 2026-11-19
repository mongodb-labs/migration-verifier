package compare

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BatchReadStats describes a read of one batch of documents.
type BatchReadStats struct {
	// First indicates the cursor’s first batch, which comes with the command
	// that opened the cursor. Later batches come from getMores.
	First bool

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

// BatchReadTimer measures each batch that a cursor returns.
type BatchReadTimer struct {
	record BatchReadStatsRecorder

	// started indicates that Next has been called.
	started bool

	// read describes the current batch, with its documents so far.
	read BatchReadStats
}

// NewBatchReadTimer returns a BatchReadTimer that sends each batch to record.
// openDuration is how long the command that opened the cursor took; it is the
// first batch’s duration. If record is nil, the timer measures nothing.
func NewBatchReadTimer(record BatchReadStatsRecorder, openDuration time.Duration) *BatchReadTimer {
	return &BatchReadTimer{
		record: record,
		read:   BatchReadStats{First: true, Duration: openDuration},
	}
}

// Next calls cursor.Next. If that call needs a getMore, Next times it.
func (t *BatchReadTimer) Next(ctx context.Context, cursor *mongo.Cursor) bool {
	if t.record == nil {
		return cursor.Next(ctx)
	}

	if !t.started {
		t.started = true

		// An empty first batch is still a read.
		if cursor.RemainingBatchLength() == 0 {
			t.record(ctx, t.read)
		}
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

		t.read = BatchReadStats{Duration: time.Since(start)}
	}

	t.read.Docs++
	t.read.Bytes += len(cursor.Current)

	if cursor.RemainingBatchLength() == 0 {
		t.record(ctx, t.read)
	}

	return true
}
