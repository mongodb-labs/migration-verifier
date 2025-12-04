package mmongo

import (
	"context"
	"fmt"
	"slices"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type cursorLike interface {
	TryNext(context.Context) bool
	RemainingBatchLength() int
	Err() error
}

// GetBatch returns a batch of documents from a cursor. It does so by appending
// to passed-in slices, which lets you optimize memory handling.
func GetBatch[T cursorLike](
	ctx context.Context,
	cursor T,
	docs []bson.Raw,
	buffer []byte,
) ([]bson.Raw, []byte, error) {
	var docsCount, expectedCount int

	var curDoc bson.Raw

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

		switch typedCursor := any(cursor).(type) {
		case *mongo.Cursor:
			curDoc = typedCursor.Current
		case *mongo.ChangeStream:
			curDoc = typedCursor.Current
		default:
			panic(fmt.Sprintf("unknown cursor type: %T", cursor))
		}

		docPos := len(buffer)
		buffer = append(buffer, curDoc...)
		docs = append(docs, buffer[docPos:])
	}

	return docs, buffer, nil
}
