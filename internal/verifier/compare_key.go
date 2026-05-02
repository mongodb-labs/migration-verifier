package verifier

import (
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// This appends a given RawValue to a given buffer.
func rvToMapKey(buf []byte, rv bson.RawValue) []byte {
	buf = slices.Grow(buf, 1+len(rv.Value))
	buf = append(buf, byte(rv.Type))
	return append(buf, rv.Value...)
}
