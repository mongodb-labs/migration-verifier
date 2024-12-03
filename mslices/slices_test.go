package mslices

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type mySuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	suite.Run(t, &mySuite{})
}

func (s *mySuite) Test_Of() {
	slc := Of(12, 23, 34)

	s.Assert().IsType([]int{}, slc, "expected type")

	a := []int{1, 2, 3}
	b := Of(a...)
	a[0] = 4

	s.Assert().Equal(1, b[0], "should copy slice")
}

func (s *mySuite) Test_ToMap() {
	type stuff struct {
		Name string
	}

	mySlice := []stuff{{"foo"}, {"bar"}, {"bar"}}

	myMap := ToMap(
		mySlice,
		func(el stuff) string {
			return el.Name
		},
	)

	s.Assert().Equal(
		map[string]stuff{
			"foo": {"foo"},
			"bar": {"bar"},
		},
		myMap,
	)
}
