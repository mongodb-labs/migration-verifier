package mbson

import (
	"testing"

	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

type UnitTestSuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}

func (s *UnitTestSuite) Test_RawLookup() {
	myDoc := bson.D{
		{"foo", 1},
		{"bar", "baz"},
		{"quux", bson.A{
			123,
			bson.D{{"hey", "ho"}},
		}},
	}

	myRaw, err := bson.Marshal(myDoc)
	s.Require().NoError(err)

	var myInt int
	var myStr string

	found, err := RawLookup(myRaw, &myInt, "foo")
	s.Require().True(found)
	s.Require().NoError(err)
	s.Assert().EqualValues(1, myInt)

	found, err = RawLookup(myRaw, &myInt, "quux", "0")
	s.Require().True(found)
	s.Require().NoError(err)
	s.Assert().EqualValues(123, myInt)

	found, err = RawLookup(myRaw, &myStr, "quux", "1", "hey")
	s.Require().True(found)
	s.Require().NoError(err)
	s.Assert().EqualValues("ho", myStr)

	found, err = RawLookup(myRaw, &myStr, "not there")
	s.Require().NoError(err)
	s.Assert().False(found)

	myRaw = myRaw[:len(myRaw)-2]
	_, err = RawLookup(myRaw, &myStr, "not there")
	s.Assert().ErrorAs(err, &bsoncore.InsufficientBytesError{})
}

func (s *UnitTestSuite) Test_RawContains() {
	myDoc := bson.D{
		{"foo", 1},
		{"bar", "baz"},
		{"quux", bson.A{
			123,
			bson.D{{"hey", "ho"}},
		}},
	}

	myRaw, err := bson.Marshal(myDoc)
	s.Require().NoError(err)

	has, err := RawContains(myRaw, "foo")
	s.Require().NoError(err)
	s.Assert().True(has, "`foo` should exist")

	has, err = RawContains(myRaw, "quux", "1", "hey")
	s.Require().NoError(err)
	s.Assert().True(has, "deep lookup should work")

	has, err = RawContains(myRaw, "not there")
	s.Require().NoError(err)
	s.Assert().False(has, "missing element should not exist")

	myRaw = myRaw[:len(myRaw)-2]
	_, err = RawContains(myRaw, "not there")
	s.Assert().ErrorAs(err, &bsoncore.InsufficientBytesError{})
}

func (s *UnitTestSuite) Test_ConvertToRawValue() {
	rv, err := ConvertToRawValue(nil)
	s.Require().NoError(err, "nil should convert")

	s.Assert().Equal(bson.TypeNull, rv.Type, "type should be correct")
}
