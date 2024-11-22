package mbson

import (
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

// RawLookup combines bson.Raw’s LookupErr method with an additional
// unmarshal step. The result is a convenient way to extract values from
// bson.Raw. The returned boolean indicates whether the value was found.
func RawLookup[T any](doc bson.Raw, dest *T, keys ...string) (bool, error) {
	val, err := doc.LookupErr(keys...)

	if err == nil {
		return true, val.Unmarshal(dest)
	} else if errors.Is(err, bsoncore.ErrElementNotFound) {
		return false, nil
	}

	return false, errors.Wrapf(err, "failed to look up %+v in BSON doc", keys)
}

// RawContains is like RawLookup but makes no effort to unmarshal
// the value.
func RawContains(doc bson.Raw, keys ...string) (bool, error) {
	val := any(nil)
	return RawLookup(doc, &val, keys...)
}
