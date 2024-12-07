package util

import (
	"time"

	"github.com/10gen/migration-verifier/option"
)

func (s *UnitTestSuite) TestEventual() {
	eventual := NewEventual[int]()

	s.Assert().Equal(
		option.None[int](),
		eventual.Get(),
		"Get() should return empty",
	)

	select {
	case <-eventual.Ready():
		s.Require().Fail("should not be ready")
	case <-time.NewTimer(time.Second).C:
	}

	eventual.Set(123)

	select {
	case <-eventual.Ready():
	case <-time.NewTimer(time.Second).C:
		s.Require().Fail("should be ready")
	}

	s.Assert().Equal(
		option.Some(123),
		eventual.Get(),
		"Get() should return the value",
	)
}
