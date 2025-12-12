package verifier

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/exp/slices"
)

// This appends a given RawValue to a given buffer.
func rvToMapKey(buf []byte, rv bson.RawValue) []byte {
	buf = slices.Grow(buf, 1+len(rv.Value))
	buf = append(buf, byte(rv.Type))
	return append(buf, rv.Value...)
}
