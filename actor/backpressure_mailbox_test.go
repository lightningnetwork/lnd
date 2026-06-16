package actor

import (
	"context"
	"sync"
	"testing"

	"github.com/lightningnetwork/lnd/queue"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that BackpressureMailbox satisfies the Mailbox
// interface.
var _ Mailbox[TestMessage, int] = (*BackpressureMailbox[TestMessage, int])(nil)

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
	for i := range dropThreshold {
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
	for i := range dropThreshold {
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
	for i := range capacity {
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
	for i := range 2 {
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
	for i := range 3 {
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

// TestBackpressureMailboxReceiveAfterClose verifies that calling Receive after
// Close does not panic and yields no messages (the channel is already drained).
func TestBackpressureMailboxReceiveAfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 5
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})
	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	mbox.Close()

	// First Receive after close should return immediately (closed channel).
	var count int
	for range mbox.Receive(ctx) {
		count++
	}
	require.Equal(t, 0, count, "no messages expected")

	// Second Receive must not panic.
	for range mbox.Receive(ctx) {
		count++
	}
	require.Equal(t, 0, count, "still no messages expected")
}

// TestBackpressureMailboxDrainAfterDrain verifies that calling Drain twice
// after Close does not panic.
func TestBackpressureMailboxDrainAfterDrain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const capacity = 5
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})
	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	// Send one message and close.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: 1},
	}
	mbox.Send(ctx, env)
	mbox.Close()

	// First drain should yield the message.
	var count int
	for range mbox.Drain() {
		count++
	}
	require.Equal(t, 1, count, "should drain 1 message")

	// Second drain must not panic and should yield nothing.
	count = 0
	for range mbox.Drain() {
		count++
	}
	require.Equal(t, 0, count, "second drain should yield nothing")
}

// TestBackpressureMailboxConcurrentSendClose tests concurrent Send/TrySend and
// Close operations to ensure no race conditions or panics occur.
func TestBackpressureMailboxConcurrentSendClose(t *testing.T) {
	t.Parallel()

	const (
		numSenders = 50
		capacity   = 20
	)

	ctx := context.Background()
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})

	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, capacity, neverDrop,
	)

	var wg sync.WaitGroup

	// Launch many goroutines that continuously call Send/TrySend.
	for i := range numSenders {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := range 100 {
				env := envelope[TestMessage, int]{
					message: TestMessage{
						Value: i*100 + j,
					},
				}
				// Send must not panic regardless of
				// whether Close has been called.
				mbox.Send(ctx, env)
			}
		}()

		// Launch a goroutine that also calls TrySend
		// concurrently.
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := range 500 {
				env := envelope[TestMessage, int]{
					message: TestMessage{Value: j},
				}
				mbox.TrySend(env)
			}
		}()
	}

	// Drain messages concurrently to free buffer space so Send
	// goroutines make progress and don't all block.
	wg.Add(1)
	go func() {
		defer wg.Done()

		ch := mbox.queue.ReceiveChan()
		for range ch {
		}
	}()

	// Close the mailbox while senders are still active.
	mbox.Close()

	// Wait for all goroutines to finish. If the RWMutex protocol
	// is broken, this test will panic with "send on closed channel"
	// or the race detector will flag a data race.
	wg.Wait()

	require.True(t, mbox.IsClosed())

	// After Close, all subsequent sends must return false.
	env := envelope[TestMessage, int]{
		message: TestMessage{Value: -1},
	}
	require.False(t, mbox.Send(ctx, env))
	require.False(t, mbox.TrySend(env))
}

// TestBackpressureMailboxConcurrentMultiClose verifies that calling Close
// from multiple goroutines simultaneously does not panic.
func TestBackpressureMailboxConcurrentMultiClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	neverDrop := queue.DropCheckFunc(func(queueLen int) bool {
		return false
	})

	mbox := NewBackpressureMailbox[TestMessage, int](
		ctx, 10, neverDrop,
	)

	// Send a few messages first.
	for i := range 5 {
		env := envelope[TestMessage, int]{
			message: TestMessage{Value: i},
		}
		mbox.Send(ctx, env)
	}

	// Close from many goroutines simultaneously.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mbox.Close()
		}()
	}
	wg.Wait()

	require.True(t, mbox.IsClosed())
}
