package dockey

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

// DocumentKey represents a document key from the change stream.
type DocumentKey bson.Raw

// NewDocumentKey returns a document key from a bson.Raw, like you might get
// from the change stream.
func NewDocumentKey(raw bson.Raw) DocumentKey {
	return DocumentKey(raw)
}

// ExtractDocumentKey takes a shard key pattern and a document and returns its
// DocumentKey. This is a reimplementation of the same method in the server,
// so that we can reliably generate document keys even without a change
// stream.
func ExtractDocumentKey(pattern Pattern, input bson.Raw) (DocumentKey, error) {
	builder, err := extractShardKey(pattern, input)
	if err != nil {
		return nil, err
	}

	if !pattern.ContainsID() {
		id, err := input.LookupErr("_id")
		if err != nil {
			return nil, errors.Wrap(err, "failed to get document _id")
		}

		val := bsoncore.Value{Type: id.Type, Data: id.Value}
		builder.AppendValue("_id", val)
	}

	return DocumentKey(builder.Build()), nil
}

// extractShardKey does the shard-key part of the document key extraction.
func extractShardKey(pattern Pattern, input bson.Raw) (*bsoncore.DocumentBuilder, error) {
	db := bsoncore.NewDocumentBuilder()

	if pattern.IsEmpty() {
		return db, nil
	}

	// For every element of the shard key pattern:
	for _, e := range pattern.Elements() {
		key := e.Key()

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
			val := bsoncore.Value{Type: rv.Type, Data: rv.Value}
			db.AppendValue(key, val)
		}
	}

	// We're done: return the whole document.
	return db, nil
}

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
				Type: el.Type,
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
