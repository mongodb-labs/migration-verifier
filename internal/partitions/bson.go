package partitions

import (
	"slices"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// -------------------------------------------------------------------------
// The sort order defined here derives from:
//
// https://www.mongodb.com/docs/manual/reference/bson-type-comparison-order/
//
// Everything there was verified empirically as part of writing this code.
// Note that, at least as of this writing, that page erroneously omits
// JS Code with Scope in the sort order. The observed behavior in MongoDB 4.4
// is that that type sorts between JavaScript Code and MaxKey.
// ------------------------------------------------------------------------

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

// NB: The server forbids arrays, undefined, and regexes as _id values.
// That simplifies the following greatly because all of those behave
// weirdly at various times:
// - 0-element arrays sort as null.
// - 1-element arrays sort as their contained element.
// - undefined & null sometimes match strangely. (This changed in 8.0.)
// - simple matches against regex actually run the regex.
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

var bsonTypeString = map[bson.Type]string{
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

// This returns BSON types that the server’s type bracketing excludes from
// query results when matching against the given value.
//
// The returned slices are types before & after, respectively. They require
// transformation (e.g., via typesToStrings()) before insertion into queries.
//
// This is kind of like strings.Cut() but against the sort-ordered list of BSON
// types, except that if the given value is a number or string-like, then other
// “like” types will not be in the returned slices.
func getTypeBracketExcludedBSONTypes(val any) ([]bson.Type, []bson.Type, error) {
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

	// If the given value is, e.g., an int, then we need to omit
	// other numeric types from the returned slices. If we don’t, then we’re
	// telling the caller to query on, e.g., [_id >= 123 OR type is double],
	// which would match something like float64(12), which, of course, we
	// don’t want.
	//
	// For the same reason, we need the same exclusion for string vs. symbol.
	if slices.Contains(numericTypes, bsonType) {
		earlier = lo.Without(earlier, numericTypes...)
		later = lo.Without(later, numericTypes...)
	} else if slices.Contains(stringTypes, bsonType) {
		earlier = lo.Without(earlier, stringTypes...)
		later = lo.Without(later, stringTypes...)
	}

	return earlier, later, nil
}

func typesToStrings(in []bson.Type) []string {
	return lo.Map(
		in,
		func(t bson.Type, _ int) string {
			return bsonTypeString[t]
		},
	)
}
