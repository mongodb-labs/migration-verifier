package mongotools

import (
	"bytes"
	"fmt"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var typeComparator = map[bson.Type]func(a, b bson.RawValue) (int, error){
	bson.TypeInt64:  bsontools.CompareInt64s,
	bson.TypeString: bsontools.CompareStrings,
	bson.TypeBinary: compareBinaryRecordID,
}

// GetKnownRecordIDTypes returns all known record ID types.
func GetKnownRecordIDTypes() mapset.Set[bson.Type] {
	return mapset.NewSetFromMapKeys(typeComparator)
}

// CompareRecordIDs compares two BSON record IDs.
//
// For the most part this is just normal [BSON sorting]. The one exception
// is binary strings: here, instead of sorting length-first, we sort the
// buffers byte-by-byte.
//
// The comparands must have the same BSON type.
//
// [BSON sorting]: https://www.mongodb.com/docs/manual/reference/bson-type-comparison-order/
func CompareRecordIDs(a, b bson.RawValue) (int, error) {
	// NB: The “BSON binary” representation of a record ID is a view merely.
	// While v5 shows time-series buckets’ record IDs as strings, v6+ shows
	// the same buckets’ record IDs as BSON binary. Thus, we should never
	// need to compare mismatched BSON types.
	if a.Type != b.Type {
		return 0, fmt.Errorf("cannot compare mismatched BSON types (%s & %s)", a.Type, b.Type)
	}

	comparator, ok := typeComparator[a.Type]
	if !ok {
		return 0, createCannotCompareTypesErr(a, b)
	}

	return comparator(a, b)
}

func compareBinaryRecordID(a, b bson.RawValue) (int, error) {
	aBin, err := bsontools.RawValueToBinary(a)
	if err != nil {
		return 0, err
	}

	bBin, err := bsontools.RawValueToBinary(b)
	if err != nil {
		return 0, err
	}

	if aBin.Subtype != 0 || bBin.Subtype != 0 {
		return 0, fmt.Errorf("cannot compare BSON binary subtypes %d and %d", aBin.Subtype, bBin.Subtype)
	}

	return bytes.Compare(aBin.Data, bBin.Data), nil
}

func createCannotCompareTypesErr(a, b bson.RawValue) error {
	return fmt.Errorf("cannot compare BSON %s and %s record IDs", a.Type, b.Type)
}
