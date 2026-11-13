//go:build unix

package msignal

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotifyContext_SignalCancels(t *testing.T) {
	ctx, stop := NotifyContext(t.Context(), syscall.SIGUSR1)
	defer stop()

	require.NoError(t, ctx.Err(), "no signal yet")

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGUSR1))

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		require.Fail(t, "the signal should cancel the context")
	}

	var sigErr SignalError
	require.ErrorAs(t, context.Cause(ctx), &sigErr)
	require.ErrorIs(t, ctx.Err(), context.Cause(ctx), "ctx err should match the cause")
	require.ErrorIs(t, ctx.Err(), context.Canceled, "ctx err should be canceled")

	assert.Equal(t, syscall.SIGUSR1, sigErr.Signal)
}

// After stop, the signal is no longer diverted, so it has its default effect
// again. For SIGTERM that ends the process, so this runs in a child process.
// (A Go program ignores SIGUSR1 by default, so it can’t show this.)
func TestNotifyContext_StopRestoresDefault(t *testing.T) {
	if os.Getenv("MSIGNAL_TEST_CHILD") == "1" {
		_, stop := NotifyContext(context.Background(), syscall.SIGTERM)
		stop()

		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		time.Sleep(10 * time.Second)

		// Reaching here means the signal was still diverted.
		os.Exit(0)
	}

	cmd := exec.CommandContext(
		t.Context(),
		os.Args[0],
		"-test.run=^TestNotifyContext_StopRestoresDefault$",
	)
	cmd.Env = append(os.Environ(), "MSIGNAL_TEST_CHILD=1")

	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the child should not exit normally")

	status, isWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, isWaitStatus)
	assert.True(t, status.Signaled(), "the child should die by a signal: %v", status)
	assert.Equal(t, syscall.SIGTERM, status.Signal())
}
