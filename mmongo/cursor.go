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
// to passed-in slices, which lets you optimize memory handling.
func GetBatch(
	ctx context.Context,
	cursor *mongo.Cursor,
	docs []bson.Raw,
	buffer []byte,
) ([]bson.Raw, []byte, error) {
	var docsCount, expectedCount int

	for hasDocs := true; hasDocs; hasDocs = cursor.RemainingBatchLength() > 0 {
		got := cursor.TryNext(ctx)

		if cursor.Err() != nil {
			return nil, nil, errors.Wrap(cursor.Err(), "cursor iteration failed")
		}

		if !got {
			if docsCount != 0 {
				panic(fmt.Sprintf("Docs batch ended after %d but expected %d", docsCount, expectedCount))
			}

			break
		}

		// This ensures we only reallocate once (if at all):
		if docsCount == 0 {
			expectedCount = 1 + cursor.RemainingBatchLength()
			docs = slices.Grow(docs, expectedCount)
		}

		docsCount++

		docPos := len(buffer)
		buffer = append(buffer, cursor.Current...)
		docs = append(docs, buffer[docPos:])
	}

	return docs, buffer, nil
}
