package mbson

import (
	"fmt"
	"math"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

type bsonCastRecipient interface {
	bson.Raw | bson.RawArray | bson.Timestamp | bson.ObjectID | string | int32
}

type bsonSourceTypes interface {
	string | int | int32 | int64
}

type cannotCastErr struct {
	gotBSONType bson.Type
	toGoType    any
}

func (ce cannotCastErr) Error() string {
	return fmt.Sprintf("cannot cast BSON %s to %T", ce.gotBSONType, ce.toGoType)
}

// CastRawValue is a “one-stop-shop” interface around bson.RawValue’s various
// casting interfaces. Unlike those functions, though, this returns an error
// if the target type doesn’t match the value.
//
// Augment bsonCastRecipient if you find a type here that’s missing.
func CastRawValue[T bsonCastRecipient](in bson.RawValue) (T, error) {
	switch any(*new(T)).(type) {
	case bson.Raw:
		if doc, isDoc := in.DocumentOK(); isDoc {
			return any(doc).(T), nil
		}
	case bson.RawArray:
		if arr, ok := in.ArrayOK(); ok {
			return any(arr).(T), nil
		}
	case bson.Timestamp:
		if t, i, ok := in.TimestampOK(); ok {
			return any(bson.Timestamp{t, i}).(T), nil
		}
	case bson.ObjectID:
		if id, ok := in.ObjectIDOK(); ok {
			return any(id).(T), nil
		}
	case string:
		if str, ok := in.StringValueOK(); ok {
			return any(str).(T), nil
		}
	case int32:
		if val, ok := in.Int32OK(); ok {
			return any(val).(T), nil
		}
	default:
		panic(fmt.Sprintf("Unrecognized Go type: %T (maybe augment bsonType?)", in))
	}

	return *new(T), cannotCastErr{in.Type, any(in)}
}

// Lookup fetches a value from a BSON document, casts it to the appropriate
// type, then returns the result.
func Lookup[T bsonCastRecipient](doc bson.Raw, pointer ...string) (T, error) {
	rv, err := doc.LookupErr(pointer...)

	if err != nil {
		return *new(T), fmt.Errorf("extracting %#q: %w", pointer, err)
	}

	return CastRawValue[T](rv)
}

// LookupTo is like Lookup but assigns to a referent value rather than
// returning a new one.
func LookupTo[T bsonCastRecipient](doc bson.Raw, recipient *T, pointer ...string) error {
	var err error
	*recipient, err = Lookup[T](doc, pointer...)

	return err
}

// UnmarshalElementValue is like UnmarshalRawValue but takes a RawElement.
// Any returned error will include the field name (if it parses validly).
func UnmarshalElementValue[T bsonCastRecipient](in bson.RawElement, recipient *T) error {
	rv, err := in.ValueErr()

	if err != nil {
		key, keyErr := in.KeyErr()
		if keyErr != nil {
			return fmt.Errorf("parsing element value (invalid key: %w): %w", keyErr, err)
		}

		return fmt.Errorf("parsing %#q element: %w", key, err)
	}

	return UnmarshalRawValue(rv, recipient)
}

// UnmarshalRawValue implements bson.Unmarshal’s semantics but with additional
// type constraints that avoid reflection.
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
