package compare

import (
	"math"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// ToComparatorBatchSize is the max # of docs that readers send
	// to the comparator thread at once.
	ToComparatorBatchSize = 100

	// ToComparatorByteLimit is the max # of bytes that readers send to the
	// comparator thread at once. This value is somewhat arbitrarily chosen.
	ToComparatorByteLimit = 32 << 20
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

// DocWithTS holds a document that’s to be compared with its peer’s or, if not,
// marked as missing/extra on the destination.
type DocWithTS struct {
	Doc bson.Raw
	TS  bson.Timestamp
}
