package compare

import (
	pool "github.com/libp2p/go-buffer-pool"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type ToComparatorMsg interface {
	_isToComparatorMsg()
}

type DocsToComparatorMsg []DocWithTS

func (DocsToComparatorMsg) _isToComparatorMsg() {}

// BatchDoneComparatorMsg ends one source cursor batch in natural partitioning.
// The source reader sends it after the batch’s documents, and the destination
// reader sends it after the documents from its query for that batch’s IDs.
// Its value is the batch’s number, which the comparator checks matches on
// both sides.
type BatchDoneComparatorMsg uint64

func (BatchDoneComparatorMsg) _isToComparatorMsg() {}

var (
	_ ToComparatorMsg = DocsToComparatorMsg(nil)
	_ ToComparatorMsg = BatchDoneComparatorMsg(0)
)

// NaturalSrcToDstMsg is one source cursor batch’s document IDs, which natural
// partitioning sends from the source reader to the destination reader.
type NaturalSrcToDstMsg struct {
	// BatchNum is the batch’s number, which the destination reader echoes
	// in its BatchDoneComparatorMsg.
	BatchNum uint64

	IDs []DocID
}

const (
	// ToComparatorBatchSize is the max # of docs that readers send
	// to the comparator thread at once.
	ToComparatorBatchSize = 100

	// ToComparatorByteLimit is the max # of bytes that readers send to the
	// comparator thread at once. This value is somewhat arbitrarily chosen.
	ToComparatorByteLimit = 32 << 20
)

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

// PutInPool puts the underlying buffer into the buffer pool.
// Call this on all pool-created structs once you finish with them.
//
// If the struct was not created with NewDocIDFromPool, this panics.
func (d DocID) PutInPool() {
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
// Once done with this struct, callers should call PutInPool() on it.
func NewDocWithTSFromPool(doc bson.Raw, ts bson.Timestamp) DocWithTS {
	copiedDoc := pool.Get(len(doc))
	copy(copiedDoc, doc)

	return DocWithTS{
		Doc:      copiedDoc,
		TS:       ts,
		fromPool: true,
	}
}

// PutInPool puts the underlying buffer into the buffer pool.
// Call this on all pool-created structs once you finish with them.
//
// If the struct was not created with NewDocWithTSFromPool, this panics.
func (d DocWithTS) PutInPool() {
	lo.Assertf(
		d.fromPool,
		"BackToPool() called on non-pool %T",
		d,
	)

	pool.Put(d.Doc)
}
