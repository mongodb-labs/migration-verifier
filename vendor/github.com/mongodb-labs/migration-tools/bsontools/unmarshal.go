package bsontools

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// UnmarshalRaw mimics bson.Unmarshal to a bson.D.
func UnmarshalRaw(raw bson.Raw) (bson.D, error) {
	elsCount := 0

	for _, err := range RawElements(raw) {
		if err != nil {
			return nil, fmt.Errorf("parsing BSON: %w", err)
		}

		elsCount++
	}

	d := make(bson.D, 0, elsCount)

	for el, err := range RawElements(raw) {
		if err != nil {
			panic("parsing BSON (no error earlier?!?): " + err.Error())
		}

		key, err := el.KeyErr()
		if err != nil {
			return nil, fmt.Errorf("extracting field %d’s name: %w", len(d), err)
		}

		e := bson.E{}
		e.Key = key

		val, err := el.ValueErr()
		if err != nil {
			return nil, fmt.Errorf("extracting %#q value: %w", key, err)
		}

		e.Value, err = unmarshalValue(val)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling %#q value: %w", key, err)
		}

		d = append(d, e)
	}

	return d, nil
}

// UnmarshalArray is like UnmarshalRaw but for an array.
func UnmarshalArray(raw bson.RawArray) (bson.A, error) {
	elsCount := 0

	for _, err := range RawElements(bson.Raw(raw)) {
		if err != nil {
			return nil, fmt.Errorf("parsing BSON: %w", err)
		}

		elsCount++
	}

	a := make(bson.A, 0, elsCount)

	for el, err := range RawElements(bson.Raw(raw)) {
		if err != nil {
			panic("parsing BSON (no error earlier?!?): " + err.Error())
		}

		val, err := el.ValueErr()
		if err != nil {
			return nil, fmt.Errorf("extracting element %d: %w", len(a), err)
		}

		anyVal, err := unmarshalValue(val)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling element %d: %w", len(a), err)
		}

		a = append(a, anyVal)
	}

	return a, nil
}

func unmarshalValue(val bson.RawValue) (any, error) {
	switch val.Type {
	case bson.TypeDouble:
		return val.Double(), nil
	case bson.TypeString:
		return val.StringValue(), nil
	case bson.TypeEmbeddedDocument:
		tVal, err := UnmarshalRaw(val.Value)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling subdoc: %w", err)
		}

		return tVal, nil
	case bson.TypeArray:
		tVal, err := UnmarshalArray(val.Value)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling array: %w", err)
		}

		return tVal, nil
	case bson.TypeBinary:
		subtype, bin := val.Binary()
		return bson.Binary{subtype, bin}, nil
	case bson.TypeUndefined:
		return bson.Undefined{}, nil
	case bson.TypeObjectID:
		return val.ObjectID(), nil
	case bson.TypeBoolean:
		return val.Boolean(), nil
	case bson.TypeDateTime:
		return bson.DateTime(val.DateTime()), nil
	case bson.TypeNull:
		return nil, nil
	case bson.TypeRegex:
		pattern, opts := val.Regex()
		return bson.Regex{pattern, opts}, nil
	case bson.TypeDBPointer:
		db, ptr := val.DBPointer()
		return bson.DBPointer{DB: db, Pointer: ptr}, nil
	case bson.TypeJavaScript:
		return bson.JavaScript(val.JavaScript()), nil
	case bson.TypeSymbol:
		return bson.Symbol(val.Symbol()), nil
	case bson.TypeCodeWithScope:
		code, scope := val.CodeWithScope()
		return bson.CodeWithScope{
			Code:  bson.JavaScript(code),
			Scope: scope,
		}, nil
	case bson.TypeInt32:
		return val.Int32(), nil
	case bson.TypeTimestamp:
		t, i := val.Timestamp()
		return bson.Timestamp{t, i}, nil
	case bson.TypeInt64:
		return val.Int64(), nil
	case bson.TypeDecimal128:
		return val.Decimal128(), nil
	case bson.TypeMaxKey:
		return bson.MaxKey{}, nil
	case bson.TypeMinKey:
		return bson.MinKey{}, nil
	default:
		panic(fmt.Sprintf("unknown BSON type: %d", val.Type))
	}
}
