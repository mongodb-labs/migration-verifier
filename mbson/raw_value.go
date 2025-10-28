package mbson

import (
	"fmt"
	"math"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

type bsonCastRecipient interface {
	bson.Raw | primitive.Timestamp | string
}

type bsonSourceTypes interface {
	string | int | int32 | int64
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
func CastRawValue[T bsonCastRecipient](in bson.RawValue) (T, error) {
	switch any(*new(T)).(type) {
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
		panic(fmt.Sprintf("Unrecognized Go type: %T (maybe augment bsonType?)", in))
	}

	return *new(T), cannotCastErr{in.Type, any(in)}
}

func UnmarshalRawValue[T bsonCastRecipient](in bson.RawValue, recipient *T) error {
	var err error
	*recipient, err = CastRawValue[T](in)

	return err
}

// ToRawValue is a bit like bson.MarshalValue, but:
// - It’s faster since it avoids reflection.
// - It always succeeds since it only accepts certain known types.
func ToRawValue[T bsonSourceTypes](in T) bson.RawValue {
	switch typedIn := any(in).(type) {
	case int:
		if typedIn < math.MinInt32 || typedIn > math.MaxInt32 {
			return i64ToRawValue(int64(typedIn))
		}

		return i32ToRawValue(typedIn)
	case int32:
		return i32ToRawValue(typedIn)
	case int64:
		return i64ToRawValue(typedIn)
	case string:
		return bson.RawValue{
			Type:  bson.TypeString,
			Value: bsoncore.AppendString(nil, typedIn),
		}
	}

	panic(fmt.Sprintf("Unrecognized Go type: %T (maybe add marshal instructions?)", in))
}

type i32Ish interface {
	int | int32
}

func i32ToRawValue[T i32Ish](in T) bson.RawValue {
	return bson.RawValue{
		Type:  bson.TypeInt32,
		Value: bsoncore.AppendInt32(nil, int32(in)),
	}
}

func i64ToRawValue(in int64) bson.RawValue {
	return bson.RawValue{
		Type:  bson.TypeInt64,
		Value: bsoncore.AppendInt64(nil, in),
	}
}
