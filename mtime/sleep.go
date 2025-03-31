package mtime

import (
	"context"
	"time"
)

// Sleep is like the standard library’s time.Sleep() but will stop
// waiting if its context is canceled. The return is ctx.Err(),
// or nil if the full duration was reached.
func Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
