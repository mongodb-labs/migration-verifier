package contextplus

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
)

func (s *UnitTestSuite) TestCancelCause() {
	for _, cause := range mslices.Of(
		fmt.Errorf("just because"),
		errors.Wrap(context.Canceled, "just because"),
	) {
		ctx := New(context.Background())
		ctx2, canceller := ctx.WithCancel()

		canceller(cause)

		canceledErr := ctx2.Err()

		s.Assert().ErrorIs(canceledErr, context.Canceled)
		s.Assert().ErrorIs(canceledErr, cause)

		fromCause := context.Cause(ctx2)
		s.Assert().ErrorIs(fromCause, cause)
	}
}

func (s *UnitTestSuite) TestUncanceled() {
	bg := context.Background()
	ctx := New(bg)

	s.Assert().Nil(ctx.Err())
}

func (s *UnitTestSuite) TestTimeoutCause() {
	for _, cause := range mslices.Of(
		fmt.Errorf("just because"),
		errors.Wrap(context.DeadlineExceeded, "just because"),
	) {
		negativeDuration := -1 * time.Nanosecond

		ctx := New(context.Background())
		ctx2, canceller := ctx.WithTimeout(
			negativeDuration,
			cause,
		)
		defer canceller()

		ctxErr := ctx2.Err()

		s.Assert().ErrorIs(ctxErr, context.DeadlineExceeded)
		s.Assert().ErrorIs(ctxErr, cause)
		s.Assert().ErrorContains(
			ctxErr,
			negativeDuration.String(),
		)

		fromCause := context.Cause(ctx2)
		s.Assert().ErrorIs(fromCause, cause)
	}
}

func (s *UnitTestSuite) TestDeadlineCause() {
	for _, cause := range mslices.Of(
		fmt.Errorf("just because"),
		errors.Wrap(context.DeadlineExceeded, "just because"),
	) {
		pastTime := time.Now().Add(-1 * time.Minute)

		ctx := New(context.Background())
		ctx2, canceller := ctx.WithDeadline(
			pastTime,
			cause,
		)
		defer canceller()

		ctxErr := ctx2.Err()

		s.Assert().ErrorIs(ctxErr, context.DeadlineExceeded)
		s.Assert().ErrorIs(ctxErr, cause)
		s.Assert().ErrorContains(
			ctxErr,
			pastTime.String(),
		)

		fromCause := context.Cause(ctx2)
		s.Assert().ErrorIs(fromCause, cause)
	}
}
