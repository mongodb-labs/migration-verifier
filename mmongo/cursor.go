package mmongo

import (
	"context"
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
	/*
		batchLen := cursor.RemainingBatchLength()

		docs = slices.Grow(docs, batchLen)

		for range batchLen {

			if !cursor.Next(ctx) {
				return nil, nil, errors.Wrap(cursor.Err(), "iterating cursor mid-batch")
			}
		}
	*/

	allocated := false

	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cursor.RemainingBatchLength() > 0 {
		gotEvent := cursor.TryNext(ctx)

		if cursor.Err() != nil {
			return nil, nil, errors.Wrap(cursor.Err(), "iterating cursor")
		}

		if !gotEvent {
			break
		}

		if !allocated {
			batchSize := cursor.RemainingBatchLength() + 1

			if batchSize > len(docs) {
				docs = slices.Grow(docs, batchSize-len(docs))
			}

			allocated = true
		}

		docPos := len(buffer)
		buffer = append(buffer, cursor.Current...)
		docs = append(docs, buffer[docPos:])
	}

	return docs, buffer, nil
}
