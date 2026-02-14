package bsontools

import (
	"bytes"
	"cmp"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CompareRawValues compares two RawValue structs. They MUST be of the same
// BSON type, and only a subset of valid BSON types is supported:
// - int64
// - string
// - binary string (same subtype required)
func CompareRawValues(a, b bson.RawValue) (int, error) {
	if a.Type != b.Type {
		return 0, fmt.Errorf("can’t compare BSON %s against %s", a.Type, b.Type)
	}

	switch a.Type {
	case bson.TypeInt64:
		aI64, err := RawValueToInt64(a)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", a.Type, a, err)
		}

		bI64, err := RawValueToInt64(b)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", b.Type, b, err)
		}

		return cmp.Compare(aI64, bI64), nil
	case bson.TypeString:
		aBytes, err := RawValueToStringBytes(a)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", a.Type, a, err)
		}

		bBytes, err := RawValueToStringBytes(b)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", b.Type, b, err)
		}

		return bytes.Compare(aBytes, bBytes), nil
	case bson.TypeBinary:
		aBin, err := RawValueToBinary(a)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", a.Type, a, err)
		}

		bBin, err := RawValueToBinary(b)
		if err != nil {
			return 0, fmt.Errorf("parsing BSON %T (%s): %w", b.Type, b, err)
		}

		if aBin.Subtype != bBin.Subtype {
			return 0, fmt.Errorf(
				"cannot compare BSON binary subtype %d against %d",
				aBin.Subtype,
				bBin.Subtype,
			)
		}

		return bytes.Compare(aBin.Data, bBin.Data), nil
	default:
		return 0, fmt.Errorf("can’t compare BSON %s and %s", a.Type, b.Type)
	}
}
