package testutil

import (
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Marshal wraps `bsonMarshal` with a panic on failure.
func MustMarshal(doc any) bson.Raw {
	raw, err := bson.Marshal(doc)
	if err != nil {
		panic("bson.Marshal (error in test): " + err.Error())
	}

	return raw
}

// DocsToCursor returns an in-memory cursor created from the given
// documents with a panic on failure.
func DocsToCursor(docs []bson.D) *mongo.Cursor {
	cursor, err := mongo.NewCursorFromDocuments(convertDocsToAnys(docs), nil, nil)
	if err != nil {
		panic("NewCursorFromDocuments (error in test): " + err.Error())
	}

	return cursor
}

func convertDocsToAnys(docs []bson.D) []any {
	anys := make([]any, len(docs))
	for i, doc := range docs {
		anys[i] = doc
	}

	return anys
}
