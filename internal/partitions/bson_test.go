package partitions

import (
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (suite *UnitTestSuite) Test_getBSONTypesBetweenValues() {
	for _, tc := range []struct {
		lower  any
		upper  any
		expect []string
	}{
		// Same type
		{
			lower:  primitive.ObjectID{},
			upper:  primitive.ObjectID{},
			expect: []string{},
		},
		// Both numeric types
		{
			lower:  int32(0),
			upper:  primitive.Decimal128{},
			expect: []string{},
		},
		// A numeric and a string type (far apart)
		{
			lower:  int32(0),
			upper:  primitive.Symbol(""),
			expect: []string{},
		},
		// The original case that turned this up:
		{
			lower: primitive.MinKey{},
			upper: primitive.ObjectID{},
			expect: typesToStrings(
				mslices.Of(
					bson.TypeNull,
					bson.TypeInt32,
					bson.TypeInt64,
					bson.TypeDouble,
					bson.TypeDecimal128,
					bson.TypeString,
					bson.TypeSymbol,
					bson.TypeEmbeddedDocument,
					bson.TypeBinary,
				),
			),
		},
		{
			lower: int32(1),
			upper: primitive.MaxKey{},
			expect: typesToStrings(
				mslices.Of(
					bson.TypeString,
					bson.TypeSymbol,
					bson.TypeEmbeddedDocument,
					bson.TypeBinary,
					bson.TypeObjectID,
					bson.TypeBoolean,
					bson.TypeDateTime,
					bson.TypeTimestamp,
					bson.TypeDBPointer,
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
				),
			),
		},
		{
			lower: "",
			upper: primitive.MaxKey{},
			expect: typesToStrings(
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
				),
			),
		},
		{
			lower: primitive.MinKey{},
			upper: "",
			expect: typesToStrings(
				mslices.Of(
					bson.TypeNull,
					bson.TypeInt32,
					bson.TypeInt64,
					bson.TypeDouble,
					bson.TypeDecimal128,
				),
			),
		},
		{
			lower: primitive.MinKey{},
			upper: primitive.Decimal128{},
			expect: typesToStrings(
				mslices.Of(
					bson.TypeNull,
				),
			),
		},
	} {
		got, err := getIdBSONTypesBetweenValues(tc.lower, tc.upper)
		suite.Require().NoError(err)

		suite.Assert().ElementsMatch(
			tc.expect,
			got,
			"between %T and %T",
			tc.lower,
			tc.upper,
		)
	}
}

func typesToStrings(types []bsontype.Type) []string {
	return lo.Map(
		types,
		func(t bsontype.Type, _ int) string {
			return bsonTypeString[t]
		},
	)
}
