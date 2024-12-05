package msync

import (
	"sync"
)

func (s *unitTestSuite) TestTypedAtomic() {
	ta := NewTypedAtomic(42)

	s.Require().Equal(42, ta.Load())
	s.Require().False(ta.CompareAndSwap(17, 99))
	s.Require().True(ta.CompareAndSwap(42, 99))
	s.Require().Equal(99, ta.Load())
	s.Require().Equal(99, ta.Swap(42))
	s.Require().Equal(42, ta.Load())

	ta.Store(17)
	s.Require().Equal(17, ta.Load())

	// This block is for race detection under -race.
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ta.Load()
			ta.Store(i)
		}()
	}
	wg.Wait()
}

func (s *unitTestSuite) TestAtomicZeroValues() {
	s.Run("string", func() {
		var ta TypedAtomic[string]
		s.Require().Equal("", ta.Load())
		s.Require().Equal("", ta.Swap("foo"))
		s.Require().Equal("foo", ta.Load())
	})

	s.Run("int", func() {
		var ta TypedAtomic[int]
		s.Require().Equal(0, ta.Load())
		s.Require().Equal(0, ta.Swap(42))
		s.Require().Equal(42, ta.Load())
	})

	s.Run("arbitrary data", func() {
		type data struct {
			I int
			S string
		}

		var ta TypedAtomic[data]
		s.Require().Equal(data{}, ta.Load())
		s.Require().Equal(data{}, ta.Swap(data{76, "trombones"}))
		s.Require().Equal(data{76, "trombones"}, ta.Load())
	})
}
