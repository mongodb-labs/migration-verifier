package partitions

import (
	"slices"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// Notes:
// - can’t have undefined, array, or regexp as _id
// - numeric types index together, as do symbol & string

var numericTypes = mslices.Of(
	bson.TypeInt32,
	bson.TypeInt64,
	bson.TypeDouble,
	bson.TypeDecimal128,
)

var stringTypes = mslices.Of(
	bson.TypeString,
	bson.TypeSymbol,
)

var bsonTypeSortOrder = lo.Flatten(mslices.Of(
	mslices.Of(
		bson.TypeMinKey,
		bson.TypeNull,
	),
	numericTypes,
	stringTypes,
	mslices.Of(
		bson.TypeEmbeddedDocument,
		bson.TypeBinary,
		bson.TypeObjectID,
		bson.TypeBoolean,
		bson.TypeDateTime,
		bson.TypeTimestamp,
		bson.TypeDBPointer,
		bson.TypeJavaScript,
		bson.TypeCodeWithScope,
		bson.TypeMaxKey,
	),
))

var bsonTypeString = map[bsontype.Type]string{
	bson.TypeMinKey:           "minKey",
	bson.TypeNull:             "null",
	bson.TypeBoolean:          "bool",
	bson.TypeInt32:            "int",
	bson.TypeInt64:            "long",
	bson.TypeDouble:           "double",
	bson.TypeDecimal128:       "decimal",
	bson.TypeString:           "string",
	bson.TypeSymbol:           "symbol",
	bson.TypeObjectID:         "objectId",
	bson.TypeDateTime:         "date",
	bson.TypeTimestamp:        "timestamp",
	bson.TypeJavaScript:       "javascript",
	bson.TypeCodeWithScope:    "javascriptWithScope",
	bson.TypeEmbeddedDocument: "object",
	bson.TypeBinary:           "binData",
	bson.TypeDBPointer:        "dbPointer",
	bson.TypeMaxKey:           "maxKey",
}

// This returns BSON types that the server excludes from queries against the
// given value. The returned slices are types before & after, respectively.
func splitBSONTypesOnId(val any) ([]string, []string, error) {
	bsonType, _, err := bson.MarshalValue(val)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "marshaling min value (%v)", val)
	}

	curSortOrder := slices.Index(bsonTypeSortOrder, bsonType)
	if curSortOrder < 0 {
		return nil, nil, errors.Errorf("go value (%T: %v) marshaled to BSON %s, which is invalid", val, val, bsonType)
	}

	earlier := bsonTypeSortOrder[:curSortOrder]
	later := bsonTypeSortOrder[1+curSortOrder:]

	if slices.Contains(numericTypes, bsonType) {
		earlier = lo.Without(earlier, numericTypes...)
		later = lo.Without(later, numericTypes...)
	} else if slices.Contains(stringTypes, bsonType) {
		earlier = lo.Without(earlier, stringTypes...)
		later = lo.Without(later, stringTypes...)
	}

	return typesToStrings(earlier), typesToStrings(later), nil
}

func typesToStrings(in []bsontype.Type) []string {
	return lo.Map(
		in,
		func(t bsontype.Type, _ int) string {
			return bsonTypeString[t]
		},
	)
}
