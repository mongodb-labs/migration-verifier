package dockey

import (
	"fmt"
	"strings"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

func AppendDocKeyFields(
	in []bson.RawValue,
	doc bson.Raw,
	fieldNames []string,
) ([]bson.RawValue, error) {
	for _, field := range fieldNames {
		var val bson.RawValue

		// This is how sharding routes documents: it always
		// splits on the dot and looks deeply into the document.
		parts := strings.Split(field, ".")
		val, err := doc.LookupErr(parts...)

		if err == nil {
			// Do nothing (yet).
		} else if errors.Is(err, bsoncore.ErrElementNotFound) || errors.As(err, &bsoncore.InvalidDepthTraversalError{}) {
			// If the document lacks a value for this field
			// then make it null in the document key.
			val = bson.RawValue{Type: bson.TypeNull}
		} else {
			return nil, errors.Wrapf(err, "extracting doc key field %#q from doc %+v", field, doc)
		}

		in = append(in, val)
	}

	return in, nil
}

// ExtractTrueDocKeyFromDoc extracts the document key from a document
// given its field names.
//
// NB: This avoids the problem documented in SERVER-109340; as a result,
// the returned key may not always match the change stream’s `documentKey`
// (because the server misreports its own sharding logic).
func ExtractTrueDocKeyFromDoc(
	fieldNames []string,
	doc bson.Raw,
) (bson.Raw, error) {
	assertFieldNameUniqueness(fieldNames)

	docBuilder := bsoncore.NewDocumentBuilder()

	vals, err := AppendDocKeyFields(nil, doc, fieldNames)
	if err != nil {
		return nil, errors.Wrapf(err, "fetch document key values")
	}

	for i, field := range fieldNames {
		docBuilder.AppendValue(
			field,
			bsoncore.Value{
				Type: bsoncore.Type(vals[i].Type),
				Data: vals[i].Value,
			},
		)
	}

	return bson.Raw(docBuilder.Build()), nil
}

func assertFieldNameUniqueness(fieldNames []string) {
	if mslices.FindFirstDupe(fieldNames).IsSome() {
		panic(fmt.Sprintf("Duplicate field names: %v", fieldNames))
	}
}
