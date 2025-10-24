package mbson

import (
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type bsonType interface {
	bson.Raw | primitive.Timestamp | string
}

type cannotCastErr struct {
	gotBSONType bsontype.Type
	toGoType    any
}

func (ce cannotCastErr) Error() string {
	return fmt.Sprintf("cannot cast BSON %s to %T", ce.gotBSONType, ce.toGoType)
}

// CastRawValue is a “one-stop-shop” interface around bson.RawValue’s various
// casting interfaces. Unlike those functions, though, this returns an error
// if the target type doesn’t match the value.
//
// Augment bsonType if you find a type here that’s missing.
func CastRawValue[T bsonType](in bson.RawValue) (T, error) {
	retPtr := new(T)

	switch any(*retPtr).(type) {
	case bson.Raw:
		if doc, isDoc := in.DocumentOK(); isDoc {
			return any(doc).(T), nil
		}
	case primitive.Timestamp:
		if t, i, ok := in.TimestampOK(); ok {
			return any(primitive.Timestamp{t, i}).(T), nil
		}
	case string:
		if str, ok := in.StringValueOK(); ok {
			return any(str).(T), nil
		}
	default:
		panic(fmt.Sprintf("Unrecognized Go type: %T (maybe augment bsonType?)", *retPtr))
	}

	return *retPtr, cannotCastErr{in.Type, *retPtr}
}
