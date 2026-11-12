package msignal

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

// NotifyContext is like the standard library’s signal.NotifyContext,
// but if the signal is received, the context is canceled with a SignalError
// that contains the signal.
//
// When/if https://github.com/golang/go/issues/60756 is resolved,
// replace any callers of this function with the standard library’s function.
func NotifyContext(parent context.Context, signals ...os.Signal) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	c := &signalCtx{
		Context: ctx,
		cancel:  cancel,
		signals: signals,
	}
	c.ch = make(chan os.Signal, 1)
	signal.Notify(c.ch, c.signals...)
	if ctx.Err() == nil {
		go func() {
			select {
			case s := <-c.ch:
				c.cancel(SignalError{s})
			case <-c.Done():
			}
		}()
	}
	return c, c.stop
}

type signalCtx struct {
	context.Context

	cancel  context.CancelCauseFunc
	signals []os.Signal
	ch      chan os.Signal
}

func (c *signalCtx) stop() {
	c.cancel(nil)
	signal.Stop(c.ch)
}

type stringer interface {
	String() string
}

func (c *signalCtx) String() string {
	var buf []byte
	// We know that the type of c.Context is context.cancelCtx, and we know that the
	// String method of cancelCtx returns a string that ends with ".WithCancel".
	name := c.Context.(stringer).String()
	name = name[:len(name)-len(".WithCancel")]
	buf = append(buf, "signal.NotifyContext("+name...)
	if len(c.signals) != 0 {
		buf = append(buf, ", ["...)
		for i, s := range c.signals {
			buf = append(buf, s.String()...)
			if i != len(c.signals)-1 {
				buf = append(buf, ' ')
			}
		}
		buf = append(buf, ']')
	}
	buf = append(buf, ')')
	return string(buf)
}

type SignalError struct{ os.Signal }

func (s SignalError) Error() string {
	return fmt.Sprintf("%s signal received", s.Signal)
}
