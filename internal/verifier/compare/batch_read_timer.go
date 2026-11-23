package compare

import (
	"context"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
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

// ReaderMetrics receives a document reader’s measurements.
type ReaderMetrics interface {
	// RecordBatchRead receives each batch that a BatchReadTimer measures.
	RecordBatchRead(ctx context.Context, read BatchReadStats)

	// RecordSendWait receives how long a send to the comparator waited.
	RecordSendWait(ctx context.Context, wait time.Duration)
}

// SendToComparator sends msg to the comparator and, if metrics is non-nil,
// records how long the send waited.
func SendToComparator(
	ctx context.Context,
	metrics ReaderMetrics,
	toCompare chan<- ToComparatorMsg,
	msg ToComparatorMsg,
) error {
	if metrics == nil {
		return chanutil.WriteWithDoneCheck(ctx, toCompare, msg)
	}

	start := time.Now()
	err := chanutil.WriteWithDoneCheck(ctx, toCompare, msg)
	metrics.RecordSendWait(ctx, time.Since(start))

	return err
}

// BatchReadTimer measures each batch that a cursor returns.
type BatchReadTimer struct {
	metrics ReaderMetrics

	// started indicates that Next has been called.
	started bool

	// read describes the current batch, with its documents so far.
	read BatchReadStats
}

// NewBatchReadTimer returns a BatchReadTimer that sends each batch to
// metrics. openDuration is how long the command that opened the cursor took;
// it is the first batch’s duration. If metrics is nil, the timer measures
// nothing.
func NewBatchReadTimer(metrics ReaderMetrics, openDuration time.Duration) *BatchReadTimer {
	return &BatchReadTimer{
		metrics: metrics,
		read:    BatchReadStats{First: true, Duration: openDuration},
	}
}

// Next calls cursor.Next. If that call needs a getMore, Next times it.
func (t *BatchReadTimer) Next(ctx context.Context, cursor *mongo.Cursor) bool {
	if t.metrics == nil {
		return cursor.Next(ctx)
	}

	if !t.started {
		t.started = true

		// An empty first batch is still a read.
		if cursor.RemainingBatchLength() == 0 {
			t.metrics.RecordBatchRead(ctx, t.read)
		}
	}

	// Only read the clock when the cursor must fetch.
	if cursor.RemainingBatchLength() > 0 {
		if !cursor.Next(ctx) {
			return false
		}
	} else {
		// If the cursor is still open, Next will send a getMore.
		fetches := cursor.ID() != 0

		start := time.Now()

		if !cursor.Next(ctx) {
			// A getMore that exhausts the cursor can return an empty batch.
			if fetches && cursor.Err() == nil {
				t.record(ctx, BatchReadStats{Duration: time.Since(start)})
			}

			return false
		}

		t.read = BatchReadStats{Duration: time.Since(start)}
	}

	t.read.Docs++
	t.read.Bytes += len(cursor.Current)

	if cursor.RemainingBatchLength() == 0 {
		t.metrics.RecordBatchRead(ctx, t.read)
	}

	return true
}
