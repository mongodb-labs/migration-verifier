package index

import (
	"bytes"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// NB: This is not suited for general use because use of DeepEqual means that
// NaN values will never match, even if their underlying binary representation
// does actually match.
//
// This method, rather than sorting the BSON documents’ fields, is used for
// parity with preexisting index-comparison logic. It assumes that DeepEqual
// is “close enough” to an order-agnostic BSON comparison for our needs.
func equalIgnoringOrder(a, b bson.Raw) (bool, error) {
	decoderA := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(a)))
	decoderA.DefaultDocumentMap()

	decoderB := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(b)))
	decoderB.DefaultDocumentMap()

	var mapA, mapB map[string]any

	if err := decoderA.Decode(&mapA); err != nil {
		return false, fmt.Errorf("unmarshaling A: %w", err)
	}

	if err := decoderB.Decode(&mapB); err != nil {
		return false, fmt.Errorf("unmarshaling B: %w", err)
	}

	return reflect.DeepEqual(mapA, mapB), nil
}
