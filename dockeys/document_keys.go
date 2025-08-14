// Package dockeys was copied from mongosync@5b99bac58405b and tweaked
// not to depend on mongosync’s shardkeys package but instead just to take
// a slice of field names.
package dockeys

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// DocumentKey represents a document key from the change stream.
type DocumentKey bson.Raw

// NewDocumentKey returns a document key from a bson.Raw, like you might get
// from the change stream.
func NewDocumentKey(raw bson.Raw) DocumentKey {
	return DocumentKey(raw)
}

// ExtractDocumentKey takes a slice of shard key fields and a document and returns its
// DocumentKey. This is a reimplementation of the same method in the server,
// so that we can reliably generate document keys even without a change
// stream.
func ExtractDocumentKey(fields []string, inputBytes []byte) (DocumentKey, error) {
	input := bson.Raw(inputBytes)

	builder, err := extractShardKey(fields, input)
	if err != nil {
		return nil, err
	}

	if !slices.Contains(fields, "_id") {
		id, err := input.LookupErr("_id")
		if err != nil {
			return nil, errors.Wrap(err, "failed to get document _id")
		}

		val := bsoncore.Value{Type: bsoncore.Type(id.Type), Data: id.Value}
		builder.AppendValue("_id", val)
	}

	return DocumentKey(builder.Build()), nil
}

// extractShardKey does the shard-key part of the document key extraction. This code is a
// reimplementation of the server's extractElementsBasedOnTemplate function (in
// dotted_path_support.cpp), which is called from getDocumentKey in op_observer_util.cpp (which is
// used by the change stream to generate document keys).
func extractShardKey(fields []string, input bson.Raw) (*bsoncore.DocumentBuilder, error) {
	db := bsoncore.NewDocumentBuilder()

	if len(fields) == 0 {
		return db, nil
	}

	// For every element of the shard key pattern:
	for _, key := range fields {
		rv, err := extractElementAtDottedPath(key, input)

		switch {
		case errors.Is(err, bsoncore.ErrElementNotFound),
			errors.As(err, &bsoncore.InvalidDepthTraversalError{}):
			// This key doesn't exist, do nothing. The server treats these in some
			// cases as though an explicit null were present, but when generating
			// document keys, it always omits the field entirely.

		case err != nil:
			// Something deeply weird went wrong. Try to grab the ID from the
			// document so this error is plausibly useful.
			id := input.Lookup("_id")
			return nil, errors.Wrapf(err, "failed to lookup path for %s (_id: %#q)", key, id)

		default:
			// We found the element; add it to the result.
			val := bsoncore.Value{Type: bsoncore.Type(rv.Type), Data: rv.Value}
			db.AppendValue(key, val)
		}
	}

	// We're done: return the whole document.
	return db, nil
}

// This code is a reimplementation of the server's extractElementAtDottedPath function in
// dotted_path_support.cpp.
func extractElementAtDottedPath(path string, input bson.Raw) (bson.RawValue, error) {
	// First, try to look up the field verbatim. We must do this because field
	// names can have dots in them: a shard key pattern like {"a.b": 1} will
	// first match the literal field "a.b", and then the nested lookup for "b"
	// in the embedded document "a".
	rv, err := input.LookupErr(path)

	// If it's not there, and it's a dotted path (like a.b.c), try a nested lookup.
	if errors.Is(err, bsoncore.ErrElementNotFound) && strings.Contains(path, ".") {
		left, right, ok := strings.Cut(path, ".")
		if !ok {
			panic(fmt.Sprintf("Could not cut %#q on dot", path))
		}

		el, err := input.LookupErr(left)
		if err != nil {
			return el, err
		}

		doc, ok := el.DocumentOK()
		if !ok {
			return rv, bsoncore.InvalidDepthTraversalError{
				Key:  left,
				Type: bsoncore.Type(el.Type),
			}
		}

		return extractElementAtDottedPath(right, doc)
	}

	return rv, err
}

func (dk DocumentKey) String() string {
	return bson.Raw(dk).String()
}

// Raw provides access to the document key as a bson.Raw value.
func (dk DocumentKey) Raw() bson.Raw {
	return bson.Raw(dk)
}

// Bytes provides access to the underlying bytes of the document key.
func (dk DocumentKey) Bytes() []byte {
	return []byte(dk)
}

// ID returns the _id portion of the document key as a bson.RawValue.
func (dk DocumentKey) ID() bson.RawValue {
	return dk.Raw().Lookup("_id")
}
