package actor

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/queue"
	"github.com/stretchr/testify/require"
)

// TestBackpressureMailboxDropsWhenThresholdReached verifies that
// BackpressureMailbox drops messages when shouldDrop returns true.
func TestBackpressureMailboxDropsWhenThresholdReached(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 10
	const dropThreshold = 5

	shouldDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return queueLen >= dropThreshold
	})

	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, shouldDrop,
	)

	// Fill up to the drop threshold — these should all succeed.
	for i := 0; i < dropThreshold; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		ok := mbox.Send(ctx, env)
		require.True(t, ok, "message %d should be accepted", i)
	}

	// Next message should be dropped by the predicate.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 99},
	}
	ok := mbox.Send(ctx, env)
	require.False(t, ok, "message at threshold should be dropped")
}

// TestBackpressureMailboxTrySendDrops verifies TrySend also respects the drop
// predicate.
func TestBackpressureMailboxTrySendDrops(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 10
	const dropThreshold = 3

	shouldDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return queueLen >= dropThreshold
	})

	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, shouldDrop,
	)

	// Fill to threshold.
	for i := 0; i < dropThreshold; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		ok := mbox.TrySend(env)
		require.True(t, ok, "message %d should be accepted", i)
	}

	// TrySend should now be rejected.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 99},
	}
	ok := mbox.TrySend(env)
	require.False(t, ok, "TrySend at threshold should be dropped")
}

// TestBackpressureMailboxNeverDropPassesThrough verifies that a never-drop
// predicate lets all messages through (up to channel capacity).
func TestBackpressureMailboxNeverDropPassesThrough(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 5

	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})

	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	// Fill the entire capacity.
	for i := 0; i < capacity; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		ok := mbox.Send(ctx, env)
		require.True(t, ok, "message %d should be accepted", i)
	}
}

// TestBackpressureMailboxDelegatesReceive verifies that Receive yields messages
// from the underlying BackpressureQueue.
func TestBackpressureMailboxDelegatesReceive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const capacity = 5
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})
	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	// Send two messages.
	for i := 0; i < 2; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		mbox.Send(ctx, env)
	}

	// Close so Receive iterator terminates after draining.
	mbox.Close()

	var count int
	for range mbox.Receive(ctx) {
		count++
	}

	require.Equal(t, 2, count, "should receive 2 messages")
}

// TestBackpressureMailboxDelegatesDrain verifies that Drain yields remaining
// messages after close.
func TestBackpressureMailboxDelegatesDrain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 5
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})
	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	// Send messages and close.
	for i := 0; i < 3; i++ {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		mbox.Send(ctx, env)
	}
	mbox.Close()

	require.True(t, mbox.IsClosed())

	var count int
	for range mbox.Drain() {
		count++
	}

	require.Equal(t, 3, count, "should drain 3 messages")
}

// TestBackpressureMailboxSendRespectsActorCtx verifies that Send returns false
// when the actor context is cancelled.
func TestBackpressureMailboxSendRespectsActorCtx(t *testing.T) {
	t.Parallel()

	actorCtx, actorCancel := context.WithCancel(context.Background())

	const capacity = 1
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})
	mbox := NewBackpressureMailbox[TestMessage, int](
		actorCtx, capacity, neverDrop,
	)

	// Fill the mailbox to capacity.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 1},
	}
	ok := mbox.Send(context.Background(), env)
	require.True(t, ok)

	// Cancel the actor context. The next blocking send should fail.
	actorCancel()

	env2 := envelope[TestMessage, int]{
		message: TestMessage{Value: 2},
	}
	ok = mbox.Send(context.Background(), env2)
	require.False(t, ok, "send should fail when actor context is cancelled")
}
