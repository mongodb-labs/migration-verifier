package mbson

import (
	"fmt"
	"iter"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

// RawElements returns an iterator over a Raw’s elements.
func RawElements(doc bson.Raw) iter.Seq2[bson.RawElement, error] {
	remaining := doc[4:]

	return func(yield func(bson.RawElement, error) bool) {
		var el bsoncore.Element
		var ok bool

		for len(remaining) > 1 {
			el, remaining, ok = bsoncore.ReadElement(remaining)

			var err error

			if !ok {
				err = bsoncore.NewInsufficientBytesError(doc, remaining)
			} else {
				err = el.Validate()
			}

			if err != nil {
				if yield(nil, err) {
					panic(fmt.Sprintf("Must stop iteration after error (%v)", err))
				}

				return
			}

			if !yield(bson.RawElement(el), nil) {
				return
			}
		}

	}
}

// ConvertToRawValue converts the specified argument to a bson.RawValue.
func ConvertToRawValue(thing any) (bson.RawValue, error) {
	if thing == nil {
		thing = primitive.Null{}
	}

	t, val, err := bson.MarshalValue(thing)
	if err != nil {
		return bson.RawValue{}, errors.Wrapf(err, "failed to encode value (%T) to BSON (%v)", thing, thing)
	}

	return bson.RawValue{
		Type:  t,
		Value: val,
	}, nil
}

// MustConvertToRawValue is like ConvertToRawValue, but it panics if the
// value can’t be marshaled. This is for use in tests only.
func MustConvertToRawValue(thing any) bson.RawValue {
	val, err := ConvertToRawValue(thing)
	if err != nil {
		panic(err)
	}

	return val
}
