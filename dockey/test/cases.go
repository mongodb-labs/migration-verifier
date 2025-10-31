package test

import (
	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type TestCase struct {
	Doc    bson.D
	DocKey bson.D
}

var FieldNames = mslices.Of("_id", "foo.bar.baz")

var TestCases = []TestCase{
	{
		Doc: bson.D{
			{"_id", "abc"},
			{"foo", bson.D{
				{"bar", bson.D{{"baz", 1}}},
				{"bar.baz", 2},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
			{"foo.bar.baz", 4},
		},
		DocKey: bson.D{
			{"_id", "abc"},
			{"foo.bar.baz", int32(1)},
		},
	},
	{
		Doc: bson.D{
			{"_id", "bbb"},
			{"foo", bson.D{
				{"bar", bson.D{{"baz", 1}}},
				{"bar.baz", 2},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
		},
		DocKey: bson.D{
			{"_id", "bbb"},
			{"foo.bar.baz", int32(1)},
		},
	},
	{
		Doc: bson.D{
			{"_id", "ccc"},
			{"foo", bson.D{
				{"bar.baz", 2},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
		},
		DocKey: bson.D{
			{"_id", "ccc"},
			{"foo.bar.baz", nil},
		},
	},
	{
		Doc: bson.D{
			{"_id", "ddd"},
			{"foo", bson.D{
				{"bar", bson.D{{"baz", nil}}},
			}},
		},
		DocKey: bson.D{
			{"_id", "ddd"},
			{"foo.bar.baz", nil},
		},
	},
	{
		Doc: bson.D{
			{"_id", "eee"},
			{"foo", "bar"},
		},
		DocKey: bson.D{
			{"_id", "eee"},
			{"foo.bar.baz", nil},
		},
	},
}
