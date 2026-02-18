package bsontools

import (
	"bytes"
	"cmp"

	mapset "github.com/deckarep/golang-set/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	comparatorByType = map[bson.Type]func(bson.RawValue, bson.RawValue) (int, error){
		bson.TypeInt64:  CompareInt64s,
		bson.TypeString: CompareStrings,
		bson.TypeBinary: CompareBinaries,
	}
)

// GetComparableTypes returns a Set of BSON types that this library can compare.
func GetComparableTypes() mapset.Set[bson.Type] {
	return mapset.NewSetFromMapKeys(comparatorByType)
}

// CompareInt64s compares two BSON int64s.
func CompareInt64s(a, b bson.RawValue) (int, error) {
	aGo, err := RawValueToInt64(a)
	if err != nil {
		return 0, err
	}

	bGo, err := RawValueToInt64(b)
	if err != nil {
		return 0, err
	}

	return cmp.Compare(aGo, bGo), nil
}

// CompareStrings compares two BSON strings.
func CompareStrings(a, b bson.RawValue) (int, error) {
	aGo, err := RawValueToStringBytes(a)
	if err != nil {
		return 0, err
	}

	bGo, err := RawValueToStringBytes(b)
	if err != nil {
		return 0, err
	}

	return bytes.Compare(aGo, bGo), nil
}

// CompareBinaries compares two BSON binary strings per [BSON sort order].
//
// [BSON sort order]: https://www.mongodb.com/docs/manual/reference/bson-type-comparison-order/
func CompareBinaries(a, b bson.RawValue) (int, error) {
	aGo, err := RawValueToBinary(a)
	if err != nil {
		return 0, err
	}

	bGo, err := RawValueToBinary(b)
	if err != nil {
		return 0, err
	}

	if ret := cmp.Compare(len(aGo.Data), len(bGo.Data)); ret != 0 {
		return ret, nil
	}

	if ret := cmp.Compare(aGo.Subtype, bGo.Subtype); ret != 0 {
		return ret, nil
	}

	return bytes.Compare(aGo.Data, bGo.Data), nil
}
