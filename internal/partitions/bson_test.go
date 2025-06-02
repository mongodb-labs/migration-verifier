package partitions

import (
	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/bson"
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
			expect: mslices.Of(
				bson.TypeNull.String(),
				bson.TypeInt32.String(),
				bson.TypeInt64.String(),
				bson.TypeDouble.String(),
				bson.TypeDecimal128.String(),
				bson.TypeString.String(),
				bson.TypeSymbol.String(),
				bson.TypeEmbeddedDocument.String(),
				bson.TypeBinary.String(),
			),
		},
		{
			lower: int32(1),
			upper: primitive.MaxKey{},
			expect: mslices.Of(
				bson.TypeString.String(),
				bson.TypeSymbol.String(),
				bson.TypeEmbeddedDocument.String(),
				bson.TypeBinary.String(),
				bson.TypeObjectID.String(),
				bson.TypeBoolean.String(),
				bson.TypeDateTime.String(),
				bson.TypeTimestamp.String(),
				bson.TypeDBPointer.String(),
				bson.TypeJavaScript.String(),
				bson.TypeCodeWithScope.String(),
			),
		},
		{
			lower: "",
			upper: primitive.MaxKey{},
			expect: mslices.Of(
				bson.TypeEmbeddedDocument.String(),
				bson.TypeBinary.String(),
				bson.TypeObjectID.String(),
				bson.TypeBoolean.String(),
				bson.TypeDateTime.String(),
				bson.TypeTimestamp.String(),
				bson.TypeDBPointer.String(),
				bson.TypeJavaScript.String(),
				bson.TypeCodeWithScope.String(),
			),
		},
		{
			lower: primitive.MinKey{},
			upper: "",
			expect: mslices.Of(
				bson.TypeNull.String(),
				bson.TypeInt32.String(),
				bson.TypeInt64.String(),
				bson.TypeDouble.String(),
				bson.TypeDecimal128.String(),
			),
		},
		{
			lower: primitive.MinKey{},
			upper: primitive.Decimal128{},
			expect: mslices.Of(
				bson.TypeNull.String(),
			),
		},
	} {
		got, err := getBSONTypesBetweenValues(tc.lower, tc.upper)
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
