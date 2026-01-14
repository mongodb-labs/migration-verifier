package partitions

import (
	"slices"
	"time"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (suite *UnitTestSuite) Test_splitBSONTypesForId() {
	for _, tc := range []struct {
		value  bson.RawValue
		before []bson.Type
		after  []bson.Type
	}{
		{
			value:  bsontools.ToRawValue(bson.MinKey{}),
			before: []bson.Type{},
			after: lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
			),
		},

		{
			value:  bsontools.ToRawValue(bson.Null{}),
			before: mslices.Of(bson.TypeMinKey),
			after: lo.Without(
				bsonTypeSortOrder,
				bson.TypeMinKey,
				bson.TypeNull,
			),
		},

		{
			value: bsontools.ToRawValue(int32(1)),
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
			value: bsontools.ToRawValue(int64(1)),
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
			value: bsontools.ToRawValue(float64(1)),
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
			value: bsontools.ToRawValue(bson.Decimal128{}),
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
			value: bsontools.ToRawValue(""),
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
			value: bsontools.ToRawValue(bson.Symbol("")),
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
			value: bsontools.ToRawValue(lo.Must(bsontools.MarshalD(nil, bson.D{}))),
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
			value: bsontools.ToRawValue(bson.Binary{}),
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
			value: bsontools.ToRawValue(bson.ObjectID{}),
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
			value: bsontools.ToRawValue(true),
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
			value: bsontools.ToRawValue(bson.NewDateTimeFromTime(time.Now())),
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
			value: bsontools.ToRawValue(bson.Timestamp{}),
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
			value: bsontools.ToRawValue(bson.DBPointer{}),
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
			value: bsontools.ToRawValue(bson.JavaScript("")),
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
			// ToRawValue() doesn’t work here.
			value: mbson.MustConvertToRawValue(bson.CodeWithScope{Scope: bson.D{}}),
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
			value: bsontools.ToRawValue(bson.MaxKey{}),
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

			after: []bson.Type{},
		},
	} {
		before, after := getTypeBracketExcludedBSONTypes(tc.value)

		suite.Assert().ElementsMatch(
			tc.before,
			before,
			"types that precede %T",
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
