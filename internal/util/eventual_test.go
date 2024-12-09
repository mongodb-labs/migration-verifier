package util

import (
	"time"
)

func (s *UnitTestSuite) TestEventual() {
	eventual := NewEventual[int]()

	s.Assert().Panics(
		func() { eventual.Get() },
		"Get() should panic before the value is set",
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
		123,
		eventual.Get(),
		"Get() should return the value",
	)
}
