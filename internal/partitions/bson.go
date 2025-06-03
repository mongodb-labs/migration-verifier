package partitions

import (
	"fmt"
	"slices"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

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

var numericAndStringTypes = lo.Flatten(
	mslices.Of(
		numericTypes,
		stringTypes,
	),
)

// NB: _id can’t be BSON undefined, array, or regexp, so we don’t worry
// about those types here.
var bsonTypeSortOrder = lo.Flatten(
	mslices.Of(
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
	),
)

// NB: Some of these match their Go type’s .String(),
// but not all of them.
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

// This function returns the stringified form of _id-compatible BSON types that
// sort between the given min & max and that a normal query will not return.
// This compensates for the server’s “type bracketing”, which is where it
// (quietly!) discards values of differing type from the query parameters.
//
// For example, if you give a BSON int and a binary string, this will return
// symbol, string, and object. It does *not* return long or other numeric types;
// this is because those types, being numeric, are included in queries against
// ints. The same relationship exists between string and symbol.
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
	if slices.Contains(numericAndStringTypes, minBSONType) && slices.Contains(numericAndStringTypes, maxBSONType) {
		return []string{}, nil
	}

	if slices.Contains(numericTypes, minBSONType) {
		minBSONType = lo.Max(numericTypes)
	} else if slices.Contains(stringTypes, minBSONType) {
		minBSONType = lo.Max(stringTypes)
	}

	if slices.Contains(numericTypes, maxBSONType) {
		maxBSONType = lo.Min(numericTypes)
	} else if slices.Contains(stringTypes, maxBSONType) {
		maxBSONType = lo.Min(stringTypes)
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
