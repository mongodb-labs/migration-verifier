package dockey

import (
	"strings"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

// This extracts the document key from a document gets its field names.
//
// NB: This avoids the problem documented in SERVER-109340; as a result,
// the returned key may not always match the change stream’s `documentKey`
// (because the server misreports its own sharding logic).
func ExtractTrueDocKeyFromDoc(
	fieldNames []string,
	doc bson.Raw,
) (bson.Raw, error) {
	var dk bson.D
	for _, field := range fieldNames {

		// This is how sharding routes documents: it always
		// splits on the dot and looks deeply into the document.
		parts := strings.Split(field, ".")
		val, err := doc.LookupErr(parts...)

		if errors.Is(err, bsoncore.ErrElementNotFound) || errors.As(err, &bsoncore.InvalidDepthTraversalError{}) {
			// If the document lacks a value for this field
			// then don’t add it to the document key.
			continue
		} else if err == nil {
			dk = append(dk, bson.E{field, val})
		} else {
			return nil, errors.Wrapf(err, "extracting doc key field %#q from doc %+v", field, doc)
		}
	}

	docKey, err := bson.Marshal(dk)
	if err != nil {
		return nil, errors.Wrapf(err, "marshaling doc key %v from doc %v", dk, docKey)
	}

	return docKey, nil
}
