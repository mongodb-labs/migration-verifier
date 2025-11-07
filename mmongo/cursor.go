package mmongo

import (
	"context"
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/mcmp"
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
	batchLen := cursor.RemainingBatchLength()

	docs = slices.Grow(docs, batchLen)

	for range batchLen {
		if !cursor.Next(ctx) {
			return nil, nil, mcmp.Or(
				errors.Wrap(cursor.Err(), "iterating cursor mid-batch"),
				fmt.Errorf("expected %d docs from cursor but only saw %d", batchLen, len(docs)),
			)
		}

		docPos := len(buffer)
		buffer = append(buffer, cursor.Current...)
		docs = append(docs, buffer[docPos:])
	}

	return docs, buffer, nil
}
