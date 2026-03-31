package chanutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestFilter_keepsMatchingItems(t *testing.T) {
	ctx := t.Context()

	out := make(chan int, 100)

	in, cancel := StartIngestFilter(ctx, out, func(i int) bool {
		return i%2 == 0
	})

	for i := range 10 {
		in <- i
	}

	require.NoError(t, cancel(ctx))
	close(out)

	var got []int
	for v := range out {
		got = append(got, v)
	}

	assert.Equal(t, []int{0, 2, 4, 6, 8}, got)
}

func TestIngestFilter_emptyInput(t *testing.T) {
	ctx := t.Context()

	out := make(chan int, 10)
	_, cancel := StartIngestFilter(ctx, out, func(i int) bool { return true })

	require.NoError(t, cancel(ctx))
	close(out)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	assert.Empty(t, got)
}

func TestIngestFilter_allFiltered(t *testing.T) {
	ctx := t.Context()

	out := make(chan int, 100)
	in, cancel := StartIngestFilter(ctx, out, func(i int) bool { return false })

	for i := range 10 {
		in <- i
	}

	require.NoError(t, cancel(ctx))
	close(out)

	var got []int
	for v := range out {
		got = append(got, v)
	}
	assert.Empty(t, got)
}

// TestIngestFilter_cancelUnblocksGoroutineWhenOutIsFull is the regression test
// for the deadlock bug: if the goroutine is blocked writing to a full `out`
// channel and the canceler is called, the canceler must not hang.
//
// The two-phase cancel contract: close(in) is tried first (graceful); if the
// goroutine is stuck on `out <- val`, the canceler's context must expire to
// trigger the force-cancel path. This mirrors real usage where the caller's
// context is the request context and gets cancelled when the connection drops.
func TestIngestFilter_cancelUnblocksGoroutineWhenOutIsFull(t *testing.T) {
	ctx := t.Context()

	// Unbuffered out so the goroutine will block as soon as it tries to write.
	out := make(chan int)

	in, cancel := StartIngestFilter(ctx, out, func(i int) bool { return true })

	// Send a value; the goroutine accepts it from `in` and immediately blocks
	// trying to write to the unbuffered `out` (nobody is reading).
	in <- 1

	// Give the goroutine a moment to reach the `out <- val` select.
	time.Sleep(10 * time.Millisecond)

	// Pass a context with a short deadline to simulate the "consumer stopped
	// reading" scenario. The canceler waits on done; when this deadline fires
	// it force-cancels innerCtx, unblocking the stuck goroutine.
	cancelCtx, cancelCancel := contextplus.WithTimeoutCause(ctx, 100*time.Millisecond, fmt.Errorf("%s", t.Name()))
	defer cancelCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cancel(cancelCtx) // context deadline exceeded — expected
	}()

	select {
	case <-done:
		// good — canceler returned without deadlocking
	case <-time.After(time.Second):
		t.Fatal("cancel() deadlocked: goroutine blocked on out <- val was never unblocked")
	}
}

// TestIngestFilter_parentContextCancels verifies that cancelling the parent
// context also unblocks the goroutine (existing behaviour, not regressed).
func TestIngestFilter_parentContextCancels(t *testing.T) {
	ctx, cancelCtx := contextplus.WithCancelCause(t.Context())

	out := make(chan int) // unbuffered — goroutine will block writing

	in, cancel := StartIngestFilter(ctx, out, func(i int) bool { return true })

	in <- 1
	time.Sleep(10 * time.Millisecond)

	// Cancelling the parent ctx cancels innerCtx (derived from it), which
	// unblocks the goroutine. It also satisfies cancelCtx.Done() in the
	// canceler's select, triggering the force-cancel path.
	cancelCtx(fmt.Errorf("%s", t.Name()))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cancel(ctx) // ctx is already done; error expected and ignored
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel() deadlocked after parent context was cancelled")
	}
}
