package util

import (
	"strings"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *UnitTestSuite) TestSplitArrayByBSONMaxSize() {
	oids := lo.RepeatBy(
		1_000_000,
		func(_ int) any {
			return bson.NewObjectID()
		},
	)

	groups, err := SplitArrayByBSONMaxSize(oids, 100_000)
	s.Require().NoError(err, "should not fail split")
	s.Assert().Greater(len(groups), 2, "should split appreciably")

	reconstituted := lo.Flatten(groups)
	s.Assert().Equal(oids, reconstituted, "groups should flatten back to original")
}

func (s *UnitTestSuite) TestSplitArrayByBSONMaxSize_OverLarge_Members() {
	// This test verifies behavior when a single array member exceeds
	// the maximum length.

	longString := strings.Repeat("x", 100000)

	groups, err := SplitArrayByBSONMaxSize([]any{longString}, 100)
	s.Require().NoError(err, "should split")
	s.Assert().Len(groups, 1, "only 1 “group”")
	s.Assert().Equal([][]any{{longString}}, groups)

	groups, err = SplitArrayByBSONMaxSize(
		[]any{
			"foo",
			"bar",
			longString,
			"baz",
		},
		100,
	)
	s.Require().NoError(err, "should split")
	s.Assert().Len(groups, 2, "expected groups")
	s.Assert().Equal([][]any{{"foo", "bar", longString}, {"baz"}}, groups)
}

func (s *UnitTestSuite) TestBSONArraySizer() {
	stuff := []any{
		true,
		123,
		int64(123),
		bson.Null{},
		bson.NewObjectID(),
		bson.M{},
		[]any{},
		bson.Timestamp{},
		bson.MinKey{},
		bson.MaxKey{},
		bson.Binary{},
		float32(123),
		float64(234),
	}

	doc, err := bson.Marshal(bson.M{"stuff": stuff})
	s.Require().NoError(err, "should marshal")

	stuffRawVal, err := bson.Raw(doc).LookupErr("stuff")
	s.Require().NoError(err, "should marshal")

	stuffRawDoc := stuffRawVal.Array()

	sizer := &BSONArraySizer{}
	for _, thing := range stuff {
		sizer.Add(mbson.MustConvertToRawValue(thing))
	}

	s.Assert().Equal(
		len(stuffRawDoc),
		sizer.Len(),
		"should compute the real length",
	)
}
