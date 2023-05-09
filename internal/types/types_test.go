package types

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/suite"
)

type UnitTestSuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}

func (s *UnitTestSuite) TestToNumericTypeOf() {
	foo := int16(234)

	bar := ToNumericTypeOf(int64(1), foo)

	s.Assert().Equal(reflect.TypeOf(foo), reflect.TypeOf(bar), "it works on int16")

	fooFloat := 234.467
	barFloat := ToNumericTypeOf(int64(1), fooFloat)

	s.Assert().Equal(reflect.TypeOf(fooFloat), reflect.TypeOf(barFloat), "it works on float-y")
}
