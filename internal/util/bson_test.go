package util

import (
	"github.com/10gen/migration-verifier/mbson"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (s *UnitTestSuite) TestBSONArraySizer() {
	stuff := []any{
		true,
		123,
		int64(123),
		primitive.Null{},
		primitive.NewObjectID(),
		bson.M{},
		[]any{},
		primitive.Timestamp{},
		primitive.MinKey{},
		primitive.MaxKey{},
		primitive.Binary{},
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
