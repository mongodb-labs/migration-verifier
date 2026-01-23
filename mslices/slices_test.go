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

func (s *mySuite) Test_Chunk() {
	chunkSize := 7

	nums := lo.Range(97)

	seq := Chunk(nums, chunkSize)

	chunks := slices.Collect(seq)

	s.Assert().Equal(
		nums,
		lo.Flatten(chunks),
		"values preserved",
	)

	for _, nonfinal := range chunks[:len(chunks)-1] {
		s.Assert().Len(nonfinal, chunkSize)
	}

	s.Assert().LessOrEqual(len(lo.LastOrEmpty(chunks)), chunkSize)
}
