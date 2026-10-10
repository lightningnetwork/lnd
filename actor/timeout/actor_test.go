package timeout

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/stretchr/testify/require"
)

// mockCallbackRef implements actor.TellOnlyRef[*ExpiredMsg] for testing.
// It captures all ExpiredMsg messages sent to it.
type mockCallbackRef struct {
	t        *testing.T
	id       string
	messages []ExpiredMsg
	mu       sync.Mutex
}

// newMockCallbackRef returns an empty recording expiry callback.
func newMockCallbackRef(t *testing.T, id string) *mockCallbackRef {
	return &mockCallbackRef{
		t:        t,
		id:       id,
		messages: make([]ExpiredMsg, 0),
	}
}

// ID returns the callback identifier.
func (m *mockCallbackRef) ID() string {
	return m.id
}

// Tell records the expiry.
func (m *mockCallbackRef) Tell(_ context.Context, msg *ExpiredMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if msg == nil {
		return
	}

	m.messages = append(m.messages, *msg)
}

// TryTell delegates to Tell, which is all this double needs: no test
// drives the non-blocking path through it.
func (m *mockCallbackRef) TryTell(ctx context.Context, msg *ExpiredMsg) error {
	m.Tell(ctx, msg)

	return nil
}

// getMessages returns a copy of all received messages.
func (m *mockCallbackRef) getMessages() []ExpiredMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ExpiredMsg{}, m.messages...)
}

// assertNoMessages verifies that no messages have been received.
func (m *mockCallbackRef) assertNoMessages(t *testing.T) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.Empty(
		t, m.messages, "expected no messages, got %d", len(m.messages),
	)
}

// TestActorScheduleWithoutStartDropsTimerFire verifies direct tests that forget
// to inject the self-ref do not panic from timer goroutines.
func TestActorScheduleWithoutStartDropsTimerFire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := NewActorWithClock(clock)
	callback := newMockCallbackRef(t, "callback")

	result := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "missing-self",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	})
	require.True(t, result.IsOk(), "schedule should succeed")

	require.NotPanics(t, func() {
		clock.Advance(50 * time.Millisecond)
	})
	callback.assertNoMessages(t)
}

// TestActorScheduleAndExpire tests basic timeout scheduling and expiration.
func TestActorScheduleAndExpire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	req := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := a.Receive(ctx, req)
	require.True(t, result.IsOk(), "schedule should succeed")

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)

	// Drive the clock past the deadline; the sync self-ref delivers
	// the internal fire and the user-facing ExpiredMsg in the same
	// call chain, so cb is fully populated when Advance returns.
	clock.Advance(50 * time.Millisecond)

	msgs := callback.getMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, ID("test-timeout"), msgs[0].ID)
}

// TestActorCancelBeforeExpiry tests cancelling a timeout before it fires.
func TestActorCancelBeforeExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	scheduleReq := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 500 * time.Millisecond,
		Callback: callback,
	}

	result := a.Receive(ctx, scheduleReq)
	require.True(t, result.IsOk())

	cancelReq := &CancelTimeoutRequest{
		ID: "test-timeout",
	}

	result = a.Receive(ctx, cancelReq)
	require.True(t, result.IsOk())

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)

	// Push past the original deadline; the cancelled timer must not
	// fire.
	clock.Advance(time.Second)

	callback.assertNoMessages(t)
}

// TestActorCancelNonExistent tests cancelling a timeout that doesn't exist.
func TestActorCancelNonExistent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)

	cancelReq := &CancelTimeoutRequest{
		ID: "non-existent",
	}

	result := a.Receive(ctx, cancelReq)
	require.True(t, result.IsOk(), "cancel non-existent should succeed")

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)
}

// TestActorMultipleConcurrentTimeouts tests scheduling multiple timeouts that
// fire concurrently.
func TestActorMultipleConcurrentTimeouts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)

	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")
	callback3 := newMockCallbackRef(t, "callback3")

	timeouts := []struct {
		id       ID
		duration time.Duration
		callback *mockCallbackRef
	}{
		{
			"timeout-1",
			50 * time.Millisecond,
			callback1,
		},
		{
			"timeout-2",
			75 * time.Millisecond,
			callback2,
		},
		{
			"timeout-3",
			100 * time.Millisecond,
			callback3,
		},
	}

	for _, tc := range timeouts {
		req := &ScheduleTimeoutRequest{
			ID:       tc.id,
			Duration: tc.duration,
			Callback: tc.callback,
		}

		result := a.Receive(ctx, req)
		require.True(t, result.IsOk())
	}

	// Advance past the longest deadline.
	clock.Advance(100 * time.Millisecond)

	require.Len(t, callback1.getMessages(), 1)
	require.Equal(t, ID("timeout-1"), callback1.getMessages()[0].ID)
	require.Len(t, callback2.getMessages(), 1)
	require.Equal(t, ID("timeout-2"), callback2.getMessages()[0].ID)
	require.Len(t, callback3.getMessages(), 1)
	require.Equal(t, ID("timeout-3"), callback3.getMessages()[0].ID)
}

// TestActorDuplicateIDReplacesTimeout tests that scheduling a timeout with a
// duplicate ID cancels the previous timeout and replaces it.
func TestActorDuplicateIDReplacesTimeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	req1 := &ScheduleTimeoutRequest{
		ID:       "duplicate-id",
		Duration: 500 * time.Millisecond,
		Callback: callback,
	}

	result := a.Receive(ctx, req1)
	require.True(t, result.IsOk())

	// Reschedule with the same ID and a shorter duration.
	req2 := &ScheduleTimeoutRequest{
		ID:       "duplicate-id",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result = a.Receive(ctx, req2)
	require.True(t, result.IsOk())

	// Advance past both deadlines; only the second one is live.
	clock.Advance(time.Second)

	msgs := callback.getMessages()
	require.Len(t, msgs, 1, "should only receive one message")
	require.Equal(t, ID("duplicate-id"), msgs[0].ID)
}

// TestActorCancelAfterExpiry tests cancelling a timeout after it has already
// fired.
func TestActorCancelAfterExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	scheduleReq := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := a.Receive(ctx, scheduleReq)
	require.True(t, result.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Len(t, callback.getMessages(), 1)

	// Cancel after fire is a no-op success.
	cancelReq := &CancelTimeoutRequest{
		ID: "test-timeout",
	}

	result = a.Receive(ctx, cancelReq)
	require.True(t, result.IsOk())

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)
}

// TestActorConcurrentSendsViaActorSystem exercises the actor through a
// real actor.ActorSystem mailbox, with multiple goroutines Tell-ing
// schedule and cancel requests in parallel. Under the self-tell model
// the actor itself has no internal mutex; the framework's mailbox is
// the serialization point. This test fails if a regression
// reintroduces concurrent state mutation.
func TestActorConcurrentSendsViaActorSystem(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown())
	})

	behavior := NewActor()
	key := actor.NewServiceKey[Msg, Resp]("test-timeout")
	ref, err := actor.RegisterWithSystem(
		system, "test-timeout", key, behavior,
	)
	require.NoError(t, err)
	behavior.Start(ref)

	const numGoroutines = 10
	const numOperations = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			callback := newMockCallbackRef(
				t, fmt.Sprintf("callback-%d", id),
			)

			for j := 0; j < numOperations; j++ {
				timeoutID := ID(
					fmt.Sprintf("timeout-%d-%d", id, j),
				)

				ref.Tell(ctx, &ScheduleTimeoutRequest{
					ID:       timeoutID,
					Duration: 100 * time.Millisecond,
					Callback: callback,
				})

				if j%2 == 0 {
					ref.Tell(ctx, &CancelTimeoutRequest{
						ID: timeoutID,
					})
				}
			}
		}(i)
	}

	wg.Wait()

	// No assertions on callback contents. The goal is to surface
	// races in the actor's state mutation under -race. The test
	// passes if the mailbox processes every message without data
	// races and without panics.
}

// TestActorDifferentCallbacks tests that different callbacks can be used for
// different timeouts.
func TestActorDifferentCallbacks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)

	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")

	req1 := &ScheduleTimeoutRequest{
		ID:       "timeout-1",
		Duration: 50 * time.Millisecond,
		Callback: callback1,
	}

	result := a.Receive(ctx, req1)
	require.True(t, result.IsOk())

	req2 := &ScheduleTimeoutRequest{
		ID:       "timeout-2",
		Duration: 50 * time.Millisecond,
		Callback: callback2,
	}

	result = a.Receive(ctx, req2)
	require.True(t, result.IsOk())

	clock.Advance(50 * time.Millisecond)

	msgs1 := callback1.getMessages()
	require.Len(t, msgs1, 1)
	require.Equal(t, ID("timeout-1"), msgs1[0].ID)

	msgs2 := callback2.getMessages()
	require.Len(t, msgs2, 1)
	require.Equal(t, ID("timeout-2"), msgs2[0].ID)
}

// TestActorZeroDuration tests scheduling a timeout with zero duration.
func TestActorZeroDuration(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	req := &ScheduleTimeoutRequest{
		ID:       "zero-duration",
		Duration: 0,
		Callback: callback,
	}

	result := a.Receive(ctx, req)
	require.True(t, result.IsOk())

	// Even a zero-duration AfterFunc must wait for an Advance step.
	clock.Advance(0)

	msgs := callback.getMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, ID("zero-duration"), msgs[0].ID)
}

// TestActorRescheduleAfterExpiry tests rescheduling a timeout after it has
// already expired.
func TestActorRescheduleAfterExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	callback := newMockCallbackRef(t, "callback")

	req1 := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := a.Receive(ctx, req1)
	require.True(t, result.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Len(t, callback.getMessages(), 1)

	// Reschedule the same ID; the actor accepts it because the prior
	// fire deleted the entry.
	req2 := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result = a.Receive(ctx, req2)
	require.True(t, result.IsOk())

	clock.Advance(50 * time.Millisecond)

	msgs := callback.getMessages()
	require.Len(t, msgs, 2)
	require.Equal(t, ID("test-timeout"), msgs[0].ID)
	require.Equal(t, ID("test-timeout"), msgs[1].ID)
}
