package dockey

import (
	"testing"

	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
)

type UnitTestSuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}

var testCases = []struct {
	doc    bson.D
	docKey bson.D
}{
	{
		doc: bson.D{
			{"foo", bson.D{
				{"bar", bson.D{{"baz", 1}}},
				{"bar.baz", 2},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
			{"foo.bar.baz", 4},
		},
		docKey: bson.D{
			{"foo.bar.baz", int32(4)},
		},
	},
	{
		doc: bson.D{
			{"foo", bson.D{
				{"bar", bson.D{{"baz", 1}}},
				{"bar.baz", 2},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
		},
		docKey: bson.D{
			{"foo.bar.baz", int32(2)},
		},
	},
	{
		doc: bson.D{
			{"foo", bson.D{
				{"bar", bson.D{{"baz", 1}}},
			}},
			{"foo.bar", bson.D{{"baz", 3}}},
		},
		docKey: bson.D{
			{"foo.bar.baz", int32(1)},
		},
	},
	{
		doc: bson.D{
			{"foo.bar", bson.D{{"baz", 3}}},
		},
		docKey: bson.D{}, // _id only
	},
	{
		doc: bson.D{
			{"foo", bson.D{{"bar.baz", nil}}},
		},
		docKey: bson.D{
			{"foo.bar.baz", nil},
		},
	},
}

func (s *UnitTestSuite) TestExtractDocumentKey() {
	patternRaw, err := bson.Marshal(bson.D{
		{"_id", 1},
		{"foo.bar.baz", 1},
	})
	s.Require().NoError(err)

	pattern, err := NewPattern(patternRaw)
	s.Require().NoError(err)

	for _, curCase := range testCases {
		doc, err := bson.Marshal(
			append(
				bson.D{{"_id", true}},
				curCase.doc...,
			),
		)
		s.Require().NoError(err)

		docKeyRaw, err := ExtractDocumentKey(pattern, doc)
		s.Require().NoError(err)

		var docKey bson.D
		s.Require().NoError(bson.Unmarshal(docKeyRaw, &docKey))

		s.Assert().Equal(
			append(
				bson.D{{"_id", true}},
				curCase.docKey...,
			),
			docKey,
			"doc key for %v should be %v",
			curCase.doc,
			curCase.docKey,
		)
	}

}
