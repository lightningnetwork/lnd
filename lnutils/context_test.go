package lnutils

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContextFromQuitQuitCancels verifies that closing the quit channel
// cancels the derived context.
func TestContextFromQuitQuitCancels(t *testing.T) {
	t.Parallel()

	quit := make(chan struct{})
	ctx, cancel := ContextFromQuit(quit)
	defer cancel()

	// The context should not be done yet.
	select {
	case <-ctx.Done():
		t.Fatal("context cancelled before quit was closed")
	default:
	}

	// Closing the quit channel should cancel the context.
	close(quit)

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled after quit was closed")
	}

	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestContextFromQuitCancelCleansUp verifies that calling the returned cancel
// function cancels the context and allows the internal goroutine to exit
// cleanly, preventing a goroutine leak.
func TestContextFromQuitCancelCleansUp(t *testing.T) {
	t.Parallel()

	// Use a quit channel that is never closed to ensure the goroutine
	// exits via the cancel path, not the quit path.
	quit := make(chan struct{})
	ctx, cancel := ContextFromQuit(quit)

	// The context should not be done yet.
	select {
	case <-ctx.Done():
		t.Fatal("context cancelled before cancel was called")
	default:
	}

	// Calling cancel should cancel the context. The internal goroutine
	// exits via <-ctx.Done().
	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled after cancel() was called")
	}

	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
