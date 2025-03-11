package contextplus

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
)

// TestCancelCause verifies a number of things:
// - Err() includes the cancellation cause.
// - Err() includes Canceled.
// - context.Cause()’s return includes the cancellation cause.
//
// … and all of the above pertain both for “normal” cancellation
// causes as well as a cause that includes Canceled.
func (s *UnitTestSuite) TestCancelCause() {
	for _, cause := range []error{
		fmt.Errorf("just because"),
		errors.Wrap(context.Canceled, "just because"),
	} {
		ctx, canceller := WithCancelCause(context.Background())

		canceller(cause)

		canceledErr := ctx.Err()

		s.Assert().ErrorIs(canceledErr, context.Canceled)
		s.Assert().ErrorIs(canceledErr, cause)

		fromCause := context.Cause(ctx)
		s.Assert().ErrorIs(fromCause, cause)
	}
}

func (s *UnitTestSuite) TestUncanceled() {
	ctx, cancel := WithCancelCause(context.Background())

	s.Assert().Nil(ctx.Err())

	cancel(errors.New(""))
}

func (s *UnitTestSuite) TestTimeoutCause() {
	for _, cause := range []error{
		fmt.Errorf("just because"),
		errors.Wrap(context.DeadlineExceeded, "just because"),
	} {
		negativeDuration := -1 * time.Nanosecond

		ctx, canceller := WithTimeoutCause(
			context.Background(),
			negativeDuration,
			cause,
		)
		defer canceller()

		ctxErr := ctx.Err()

		s.Assert().ErrorIs(ctxErr, context.DeadlineExceeded)
		s.Assert().ErrorIs(ctxErr, cause)
		s.Assert().ErrorContains(
			ctxErr,
			negativeDuration.String(),
		)

		fromCause := context.Cause(ctx)
		s.Assert().ErrorIs(fromCause, cause)
	}
}

func (s *UnitTestSuite) TestDeadlineCause() {
	for _, cause := range []error{
		fmt.Errorf("just because"),
		errors.Wrap(context.DeadlineExceeded, "just because"),
	} {
		pastTime := time.Now().Add(-1 * time.Minute)

		ctx, canceller := WithDeadlineCause(
			context.Background(),
			pastTime,
			cause,
		)
		defer canceller()

		ctxErr := ctx.Err()

		s.Assert().ErrorIs(ctxErr, context.DeadlineExceeded)
		s.Assert().ErrorIs(ctxErr, cause)
		s.Assert().ErrorContains(
			ctxErr,
			pastTime.String(),
		)

		fromCause := context.Cause(ctx)
		s.Assert().ErrorIs(fromCause, cause)
	}
}
