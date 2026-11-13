package msignal

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/10gen/migration-verifier/contextplus"
)

// NotifyContext is like the standard library’s signal.NotifyContext,
// but if the signal is received, the context is canceled with a SignalError
// that contains the signal.
//
// When/if https://github.com/golang/go/issues/60756 is resolved,
// replace any callers of this function with the standard library’s function.
func NotifyContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := contextplus.WithCancelCause(parent)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)

	if ctx.Err() == nil {
		go func() {
			select {
			case s := <-ch:
				cancel(SignalError{s})
			case <-ctx.Done():
			}
		}()
	}

	stop := func() {
		cancel(nil)
		signal.Stop(ch)
	}

	return ctx, stop
}

// SignalError represents an error caused by receiving an OS signal.
type SignalError struct{ os.Signal }

func (s SignalError) Error() string {
	return fmt.Sprintf("%s signal received", s.Signal)
}
