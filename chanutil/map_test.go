package chanutil

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestMap(t *testing.T) {
	ctx := t.Context()

	strChan := make(chan string, 100)

	intChan, canceler := StartIngestMap(ctx, strChan, strconv.Itoa)

	func() {
		for i := range 90 {
			intChan <- i
		}
	}()

	require.NoError(t, canceler(ctx))

	close(strChan)
	strs := lo.ChannelToSlice(strChan)

	assert.Equal(
		t,
		mslices.Map1(lo.Range(90), strconv.Itoa),
		strs,
		"should have expected strings",
	)
}

// TestIngestMap_cancelUnblocksGoroutineWhenOutIsFull mirrors the filter
// equivalent: if the goroutine is blocked writing to out and the canceler's
// context expires, it must not deadlock.
func TestIngestMap_cancelUnblocksGoroutineWhenOutIsFull(t *testing.T) {
	ctx := t.Context()

	out := make(chan string) // unbuffered — goroutine blocks on first write

	in, cancel := StartIngestMap(ctx, out, strconv.Itoa)

	in <- 1
	time.Sleep(10 * time.Millisecond)

	cancelCtx, cancelCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cancel(cancelCtx)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel() deadlocked: goroutine blocked on out <- val was never unblocked")
	}
}

// TestIngestMap_parentContextCancels verifies that cancelling the parent
// context unblocks a goroutine stuck writing to out.
func TestIngestMap_parentContextCancels(t *testing.T) {
	ctx, cancelCtx := context.WithCancel(context.Background())

	out := make(chan string) // unbuffered — goroutine blocks on first write

	in, cancel := StartIngestMap(ctx, out, strconv.Itoa)

	in <- 1
	time.Sleep(10 * time.Millisecond)

	cancelCtx()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cancel(ctx)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel() deadlocked after parent context was cancelled")
	}
}
