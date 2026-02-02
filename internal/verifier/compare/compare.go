package compare

import (
	"math"

	pool "github.com/libp2p/go-buffer-pool"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// ToComparatorBatchSize is the max # of docs that readers send
	// to the comparator thread at once.
	ToComparatorBatchSize = 100
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
// Once done with this struct, callers should call BackToPool() on it.
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

// BackToPool puts the underlying buffer into the buffer pool.
// Call this on all pool-created structs once you finish with them.
//
// If the struct was not created with NewDocIDFromPool, this panics.
func (d DocID) BackToPool() {
	lo.Assertf(
		d.fromPool,
		"BackToPool() called on non-pool %T",
		d,
	)

	if len(d.ID.Value) > 0 {
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
// Once done with this struct, callers should call BackToPool() on it.
func NewDocWithTSFromPool(doc bson.Raw, ts bson.Timestamp) DocWithTS {
	copiedDoc := pool.Get(len(doc))
	copy(copiedDoc, doc)

	return DocWithTS{
		Doc:      copiedDoc,
		TS:       ts,
		fromPool: true,
	}
}

// BackToPool puts the underlying buffer into the buffer pool.
// Call this on all pool-created structs once you finish with them.
//
// If the struct was not created with NewDocWithTSFromPool, this panics.
func (d DocWithTS) BackToPool() {
	lo.Assertf(
		d.fromPool,
		"BackToPool() called on non-pool %T",
		d,
	)

	pool.Put(d.Doc)
}
