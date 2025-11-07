package mmongo

import (
	"context"
	"fmt"
	"slices"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// GetBatch returns a batch of documents from a cursor. It does so by appending
// to passed-in slices, which allows you to minimize memory allocations.
func GetBatch(
	ctx context.Context,
	cursor *mongo.Cursor,
	docs []bson.Raw,
	buffer []byte,
) ([]bson.Raw, []byte, error) {
	if cursor.RemainingBatchLength() != 0 {
		panic(fmt.Sprintf("only call this between batches! batchlen=%d", cursor.RemainingBatchLength()))
	}

	if !cursor.Next(ctx) {
		return nil, nil, cursor.Err()
	}

	batchLen := 1 + cursor.RemainingBatchLength()

	docs = slices.Grow(docs, batchLen)

	for range batchLen {
		docPos := len(buffer)
		buffer = append(buffer, cursor.Current...)
		docs = append(docs, buffer[docPos:])
		if !cursor.Next(ctx) {
			return nil, nil, errors.Wrap(cursor.Err(), "iterating cursor mid-batch")
		}
	}

	return docs, buffer, nil
}
