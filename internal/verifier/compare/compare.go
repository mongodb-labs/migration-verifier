package compare

import (
	"math"
	"slices"

	pool "github.com/libp2p/go-buffer-pool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// ToComparatorBatchSize is the max # of docs that readers send
	// to the comparator thread at once.
	ToComparatorBatchSize = 100

	// Only retain buffers under this byte limit.
	poolSizeThreshold = 100_000
)

func ToComparatorBatchCount(totalDocs int) int {
	return int(math.Ceil(float64(totalDocs) / ToComparatorBatchSize))
}

// DocID is how natural partitioning sends document IDs from the
// source-reader thread to the destination. This wraps a document ID
// with a struct that simplifies memory management.
type DocID struct {
	ID       bson.RawValue
	fromPool bool
}

// NewDocIDFromPool copies the value to a memory pool and returns a struct
// based on that buffer.
//
// Once done with this struct, callers should call PutInPool() on it.
func NewDocIDFromPool(rv bson.RawValue) DocID {
	docID := DocID{
		ID:       rv,
		fromPool: true,
	}

	if len(docID.ID.Value) > 0 {
		copiedValue := pool.Get(len(rv.Value))
		copy(copiedValue, rv.Value)

		docID.ID.Value = copiedValue
	}

	return docID
}

// Done allows reuse of the underlying buffer.
// It’s a no-op on structs not created with NewDocWithTSFromPool.
func (d DocID) Done() {
	if d.fromPool && len(d.ID.Value) > 0 {
		pool.Put(d.ID.Value)
	}
}

// DocWithTS holds a document that’s to be compared with its peer’s or, if not,
// marked as missing/extra on the destination.
type DocWithTS struct {
	Doc      bson.Raw
	TS       bson.Timestamp
	fromPool bool
}

// NewDocWithTSFromPool copies the given document to a memory pool
// then returns a struct containing that copy.
//
// Once done with this struct, callers should call PutInPool() on it.
func NewDocWithTSFromPool(doc bson.Raw, ts bson.Timestamp) DocWithTS {
	var copiedDoc []byte

	if len(doc) <= poolSizeThreshold {
		copiedDoc = pool.Get(len(doc))
		copy(copiedDoc, doc)
	} else {
		copiedDoc = slices.Clone(doc)
	}

	return DocWithTS{
		Doc:      copiedDoc,
		TS:       ts,
		fromPool: true,
	}
}

// Done allows reuse of the underlying buffer.
// It’s a no-op on structs not created with NewDocWithTSFromPool.
func (d DocWithTS) Done() {
	if d.fromPool && len(d.Doc) <= poolSizeThreshold {
		pool.Put(d.Doc)
	}
}
