package discovery

import (
	"context"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/stretchr/testify/require"
)

// TestAwaitGossipResultSuccess verifies that AwaitGossipResult returns nil
// when the future resolves with a nil error.
func TestAwaitGossipResultSuccess(t *testing.T) {
	t.Parallel()

	promise := actor.NewPromise[error]()
	actor.CompleteWith(promise, (error)(nil))

	err := AwaitGossipResult(context.Background(), promise.Future())
	require.NoError(t, err)
}

// TestAwaitGossipResultError verifies that AwaitGossipResult returns the
// underlying gossip processing error when the future resolves with one.
func TestAwaitGossipResultError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("gossip validation failed")
	promise := actor.NewPromise[error]()
	actor.CompleteWith(promise, sentinel)

	err := AwaitGossipResult(context.Background(), promise.Future())
	require.ErrorIs(t, err, sentinel)
}

// TestAwaitGossipResultContextCancelled verifies that AwaitGossipResult
// returns the context error when the context is cancelled before the future
// resolves.
func TestAwaitGossipResultContextCancelled(t *testing.T) {
	t.Parallel()

	// A promise that is never completed simulates a gossiper that has
	// shut down before producing a result.
	promise := actor.NewPromise[error]()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := AwaitGossipResult(ctx, promise.Future())
	require.ErrorIs(t, err, context.Canceled)
}

// TestCompleteGossipResultIdempotent verifies that completeGossipResult can be
// called multiple times without blocking. The second call must be a no-op.
func TestCompleteGossipResultIdempotent(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("processing error")
	promise := actor.NewPromise[error]()

	// First completion sets the result.
	completeGossipResult(promise, sentinel)

	// Second completion with a different value must be a no-op and must
	// never block.
	completeGossipResult(promise, nil)

	// The future should still contain the original sentinel error.
	err := AwaitGossipResult(context.Background(), promise.Future())
	require.ErrorIs(t, err, sentinel)
}
