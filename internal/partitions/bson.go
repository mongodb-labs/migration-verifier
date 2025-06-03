package partitions

import (
	"fmt"
	"slices"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// Notes:
// - can’t have undefined, array, or regexp as _id
// - symbol & string index together, as do all numeric types

var numericTypes = mapset.NewSet(
	bson.TypeInt32,
	bson.TypeInt64,
	bson.TypeDouble,
	bson.TypeDecimal128,
)

var stringTypes = mapset.NewSet(
	bson.TypeString,
	bson.TypeSymbol,
)

var numericAndStringTypes = numericTypes.Union(stringTypes)

var bsonTypeSortOrder = []bsontype.Type{
	bson.TypeMinKey,
	bson.TypeNull,
	bson.TypeInt32, bson.TypeInt64, bson.TypeDouble, bson.TypeDecimal128,
	bson.TypeString, bson.TypeSymbol,
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
}

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

var minNumericType = lo.MinBy(
	numericTypes.ToSlice(),
	func(a, b bsontype.Type) bool {
		return slices.Index(bsonTypeSortOrder, a) < slices.Index(bsonTypeSortOrder, b)
	},
)

var maxNumericType = lo.MinBy(
	numericTypes.ToSlice(),
	func(a, b bsontype.Type) bool {
		return slices.Index(bsonTypeSortOrder, a) > slices.Index(bsonTypeSortOrder, b)
	},
)

var minStringType = lo.MinBy(
	stringTypes.ToSlice(),
	func(a, b bsontype.Type) bool {
		return slices.Index(bsonTypeSortOrder, a) < slices.Index(bsonTypeSortOrder, b)
	},
)

var maxStringType = lo.MinBy(
	stringTypes.ToSlice(),
	func(a, b bsontype.Type) bool {
		return slices.Index(bsonTypeSortOrder, a) > slices.Index(bsonTypeSortOrder, b)
	},
)

// This function returns the stringified form of all BSON types that are
// in between the given min & max *and* that the server doesn’t consider
// type-equivalent. So for example, if the min bound is a BSON int, the
// returned slice won’t contain numeric types because they’ll all be checked
// as part of comparing against the int.
func getBSONTypesBetweenValues(minVal, maxVal any) ([]string, error) {
	minBSONType, _, err := bson.MarshalValue(minVal)
	if err != nil {
		return nil, errors.Wrapf(err, "marshaling min value (%v)", minVal)
	}

	maxBSONType, _, err := bson.MarshalValue(maxVal)
	if err != nil {
		return nil, errors.Wrapf(err, "marshaling max value (%v)", maxVal)
	}

	// Equality checks elide numeric type. They also elide string vs. symbol.
	// These types also sort next to each other. Thus, any time both min & max
	// are either string or number, return empty.
	if numericAndStringTypes.Contains(minBSONType) && numericAndStringTypes.Contains(maxBSONType) {
		return []string{}, nil
	}

	if numericTypes.Contains(minBSONType) {
		minBSONType = maxNumericType
	} else if stringTypes.Contains(minBSONType) {
		minBSONType = maxStringType
	}

	if numericTypes.Contains(maxBSONType) {
		maxBSONType = minNumericType
	} else if stringTypes.Contains(maxBSONType) {
		maxBSONType = minStringType
	}

	minSortOrder := slices.Index(bsonTypeSortOrder, minBSONType)
	if minSortOrder < 0 {
		panic(fmt.Sprintf("Bad min BSON type: %T", minVal))
	}

	maxSortOrder := slices.Index(bsonTypeSortOrder, maxBSONType)
	if maxSortOrder < 0 {
		panic(fmt.Sprintf("Bad max BSON type: %T", maxVal))
	}

	if maxSortOrder < minSortOrder {
		panic(fmt.Sprintf(
			"internal error: max (%s: %v) sorts before min (%s: %v)",
			maxBSONType,
			maxVal,
			minBSONType,
			minVal,
		))
	}

	if minSortOrder == maxSortOrder {
		return []string{}, nil
	}

	return lo.Map(
		bsonTypeSortOrder[1+minSortOrder:maxSortOrder],
		func(t bsontype.Type, _ int) string {
			return bsonTypeString[t]
		},
	), nil
}
