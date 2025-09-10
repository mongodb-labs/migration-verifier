package mslices

import (
	"slices"
	"testing"

	"github.com/samber/lo"
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

func (s *mySuite) Test_Reorder_Static() {
	values := Of("a", "b", "c")
	indices := Of(1, 2, 0)

	Reorder(values, indices)

	s.Assert().Equal(
		Of("b", "c", "a"),
		values,
		"values should be reordered as we expect",
	)
}

func (s *mySuite) Test_Reorder_Random() {
	values := lo.Range(10_000)

	indices := lo.Shuffle(slices.Clone(values))

	// This would be a much simpler implementation of Reorder, but it
	// copies the array, which is unideal.
	expect := make([]int, len(values))
	for i, o := range indices {
		expect[i] = values[o]
	}

	Reorder(values, indices)

	s.Assert().Equal(expect, values)
}
