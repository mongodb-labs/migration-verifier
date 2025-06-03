package partitions

import (
	"slices"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (suite *UnitTestSuite) Test_splitBSONTypesForId() {
	for _, tc := range []struct {
		value  any
		before []string
		after  []string
	}{
		{
			value:  primitive.MinKey{},
			before: []string{},
			after: typesToStrings(lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
			)),
		},

		{
			value:  primitive.Null{},
			before: typesToStrings(mslices.Of(bson.TypeMinKey)),
			after: typesToStrings(lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
				bson.TypeNull,
			)),
		},

		{
			value: int32(1),
			before: typesToStrings(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
			),
			after: typesToStrings(
				append(
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
			),
		},

		{
			value: int64(1),
			before: typesToStrings(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
			),
			after: typesToStrings(
				append(
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
			),
		},

		{
			value: float64(1),
			before: typesToStrings(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
			),
			after: typesToStrings(
				append(
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
			),
		},

		{
			value: primitive.Decimal128{},
			before: typesToStrings(
				mslices.Of(
					bson.TypeMinKey,
					bson.TypeNull,
				),
			),
			after: typesToStrings(
				append(
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
			),
		},

		{
			value: "",
			before: typesToStrings(
				append(
					mslices.Of(
						bson.TypeMinKey,
						bson.TypeNull,
					),
					numericTypes...,
				),
			),
			after: typesToStrings(
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
		},

		{
			value: primitive.Symbol(""),
			before: typesToStrings(
				append(
					mslices.Of(
						bson.TypeMinKey,
						bson.TypeNull,
					),
					numericTypes...,
				),
			),
			after: typesToStrings(
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
		},

		{
			value: bson.D{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
					mslices.Of(
						bson.TypeMinKey,
						bson.TypeNull,
					),
					numericTypes,
					stringTypes,
				)),
			),
			after: typesToStrings(
				mslices.Of(
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
		},

		{
			value: []byte{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
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
		},

		{
			value: primitive.ObjectID{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeBoolean,
					bson.TypeDateTime,
					bson.TypeTimestamp,
					bson.TypeDBPointer,
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: true,
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeDateTime,
					bson.TypeTimestamp,
					bson.TypeDBPointer,
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: time.Now(),
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeTimestamp,
					bson.TypeDBPointer,
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: primitive.Timestamp{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeDBPointer,
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: primitive.DBPointer{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeJavaScript,
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: primitive.JavaScript(""),
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeCodeWithScope,
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: primitive.CodeWithScope{Scope: bson.D{}},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: typesToStrings(
				mslices.Of(
					bson.TypeMaxKey,
				),
			),
		},

		{
			value: primitive.MaxKey{},
			before: typesToStrings(
				lo.Flatten(mslices.Of(
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
			),
			after: []string{},
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
