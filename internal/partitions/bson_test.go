package partitions

import (
	"slices"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (suite *UnitTestSuite) Test_splitBSONTypesForId() {
	for _, tc := range []struct {
		value  any
		before []bsontype.Type
		after  []bsontype.Type
	}{
		{
			value:  primitive.MinKey{},
			before: []bsontype.Type{},
			after: lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
			),
		},

		{
			value:  primitive.Null{},
			before: mslices.Of(bson.TypeMinKey),
			after: lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
				bson.TypeNull,
			),
		},

		{
			value: int32(1),
			before: mslices.Of(
				bson.TypeMinKey,
				bson.TypeNull,
			),
			after: append(
				slices.Clone(stringTypes),
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
		},

		{
			value: int64(1),
			before: mslices.Of(
				bson.TypeMinKey,
				bson.TypeNull,
			),
			after: append(
				slices.Clone(stringTypes),
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
		},

		{
			value: float64(1),
			before: mslices.Of(
				bson.TypeMinKey,
				bson.TypeNull,
			),
			after: append(
				slices.Clone(stringTypes),
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
		},

		{
			value: primitive.Decimal128{},
			before: mslices.Of(
				bson.TypeMinKey,
				bson.TypeNull,
			),
			after: append(
				slices.Clone(stringTypes),
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
		},

		{
			value: "",
			before: append(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
				numericTypes...,
			),
			after: mslices.Of(
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
		},

		{
			value: primitive.Symbol(""),
			before: append(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
				numericTypes...,
			),
			after: mslices.Of(
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
		},

		{
			value: bson.D{},
			before: lo.Flatten(mslices.Of(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
				numericTypes,
				stringTypes,
			)),
			after: mslices.Of(
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
		},

		{
			value: []byte{},
			before: lo.Flatten(mslices.Of(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
				numericTypes,
				stringTypes,
				mslices.Of(
					bson.TypeEmbeddedDocument,
				),
			)),
			after: mslices.Of(
				bson.TypeObjectID,
				bson.TypeBoolean,
				bson.TypeDateTime,
				bson.TypeTimestamp,
				bson.TypeDBPointer,
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.ObjectID{},
			before: lo.Flatten(mslices.Of(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
				numericTypes,
				stringTypes,
				mslices.Of(
					bson.TypeEmbeddedDocument,
					bson.TypeBinary,
				),
			)),
			after: mslices.Of(
				bson.TypeBoolean,
				bson.TypeDateTime,
				bson.TypeTimestamp,
				bson.TypeDBPointer,
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: true,
			before: lo.Flatten(mslices.Of(
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
				),
			)),
			after: mslices.Of(
				bson.TypeDateTime,
				bson.TypeTimestamp,
				bson.TypeDBPointer,
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: time.Now(),
			before: lo.Flatten(mslices.Of(
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
				),
			)),
			after: mslices.Of(
				bson.TypeTimestamp,
				bson.TypeDBPointer,
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.Timestamp{},
			before: lo.Flatten(mslices.Of(
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
				),
			)),
			after: mslices.Of(
				bson.TypeDBPointer,
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.DBPointer{},
			before: lo.Flatten(mslices.Of(
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
				),
			)),

			after: mslices.Of(
				bson.TypeJavaScript,
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.JavaScript(""),
			before: lo.Flatten(mslices.Of(
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
				),
			)),

			after: mslices.Of(
				bson.TypeCodeWithScope,
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.CodeWithScope{Scope: bson.D{}},
			before: lo.Flatten(mslices.Of(
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
				),
			)),

			after: mslices.Of(
				bson.TypeMaxKey,
			),
		},

		{
			value: primitive.MaxKey{},
			before: lo.Flatten(mslices.Of(
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
				),
			)),

			after: []bsontype.Type{},
		},
	} {
		before, after, err := getTypeBracketExcludedBSONTypes(tc.value)
		suite.Require().NoError(err)

		suite.Assert().ElementsMatch(
			tc.before,
			before,
			"before %T",
			tc.value,
		)

		suite.Assert().ElementsMatch(
			tc.after,
			after,
			"before %T",
			tc.value,
		)
	}
}
