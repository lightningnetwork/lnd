package timeout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestActorRecurringWithoutStartDropsTimerFire verifies direct tests that
// forget to inject the self-ref do not panic from recurring timer goroutines.
func TestActorRecurringWithoutStartDropsTimerFire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := NewActorWithClock(clock)
	callback := newMockTickCallback(t, "callback")

	result := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "missing-self",
		Interval: 50 * time.Millisecond,
		Callback: callback,
	})
	require.True(t, result.IsOk(), "schedule should succeed")

	require.NotPanics(t, func() {
		clock.Advance(50 * time.Millisecond)
	})
	require.Equal(t, 0, callback.count())
}

// TestActorScheduleRecurringTick verifies that a fixed interval produces
// exactly the expected number of ticks over a fixed advance window.
func TestActorScheduleRecurringTick(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newMockTickCallback(t, "cb")

	req := &ScheduleRecurringTickRequest{
		ID:       "tick-1",
		Interval: 100 * time.Millisecond,
		Callback: cb,
	}
	res := a.Receive(ctx, req)
	require.True(t, res.IsOk())

	// Advance 350ms, and expect exactly 3 ticks (at +100, +200, +300).
	// The recurring chain re-arms inside Receive, so each Advance
	// only sees the AfterFuncs that existed at its start.
	clock.Advance(100 * time.Millisecond)
	clock.Advance(100 * time.Millisecond)
	clock.Advance(100 * time.Millisecond)
	clock.Advance(50 * time.Millisecond)

	got := cb.snapshot()
	require.Len(t, got, 3)
	require.Equal(t, ID("tick-1"), got[0].ID)

	// Tick timestamps must be monotonically increasing at the
	// configured interval.
	require.Equal(t, startEpoch.Add(100*time.Millisecond), got[0].FiredAt)
	require.Equal(t, startEpoch.Add(200*time.Millisecond), got[1].FiredAt)
	require.Equal(t, startEpoch.Add(300*time.Millisecond), got[2].FiredAt)
}

// TestActorCancelRecurringTick verifies that Cancel stops further ticks
// from arriving.
func TestActorCancelRecurringTick(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newMockTickCallback(t, "cb")

	req := &ScheduleRecurringTickRequest{
		ID:       "tick-cancel",
		Interval: 100 * time.Millisecond,
		Callback: cb,
	}
	require.True(t, a.Receive(ctx, req).IsOk())

	clock.Advance(100 * time.Millisecond)
	clock.Advance(50 * time.Millisecond)
	require.Equal(t, 1, cb.count())

	cancel := &CancelTimeoutRequest{ID: "tick-cancel"}
	require.True(t, a.Receive(ctx, cancel).IsOk())

	// After Cancel, the most recent re-arm timer was Stop'd. No
	// additional fires can be produced, even across long advances.
	clock.Advance(time.Second)

	require.Equal(
		t, 1, cb.count(),
		"no further ticks should arrive after cancel",
	)
}

// TestActorReplaceRecurringTickWithSameID confirms that re-scheduling
// against an existing recurring ID stops the previous ticker.
func TestActorReplaceRecurringTickWithSameID(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newMockTickCallback(t, "cb")

	first := &ScheduleRecurringTickRequest{
		ID:       "shared",
		Interval: 100 * time.Millisecond,
		Callback: cb,
	}
	require.True(t, a.Receive(ctx, first).IsOk())

	clock.Advance(100 * time.Millisecond)
	require.Equal(t, 1, cb.count())

	// Replace with a faster interval. The old entry's most recent
	// re-arm is Stop'd by cancelExisting; the new entry starts its
	// own chain.
	second := &ScheduleRecurringTickRequest{
		ID:       "shared",
		Interval: 50 * time.Millisecond,
		Callback: cb,
	}
	require.True(t, a.Receive(ctx, second).IsOk())

	clock.Advance(50 * time.Millisecond)
	clock.Advance(50 * time.Millisecond)
	clock.Advance(50 * time.Millisecond)

	// 1 (from first) + 3 ticks at 50ms each = 4 total.
	require.Equal(t, 4, cb.count())
}

// TestActorMixedOneShotAndRecurring confirms that one-shot timeouts and
// recurring ticks share the same ID namespace and that CancelTimeoutRequest
// works on either.
func TestActorMixedOneShotAndRecurring(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	expiredCB := newMockCallbackRef(t, "expired-cb")
	tickCB := newMockTickCallback(t, "tick-cb")

	require.True(
		t, a.Receive(ctx, &ScheduleTimeoutRequest{
			ID:       "one-shot",
			Duration: 100 * time.Millisecond,
			Callback: expiredCB,
		}).IsOk(),
	)

	require.True(
		t, a.Receive(ctx, &ScheduleRecurringTickRequest{
			ID:       "recurring",
			Interval: 50 * time.Millisecond,
			Callback: tickCB,
		}).IsOk(),
	)

	clock.Advance(50 * time.Millisecond)
	clock.Advance(50 * time.Millisecond)
	require.Equal(t, 2, tickCB.count())
	require.Len(t, expiredCB.getMessages(), 1)

	// Cancel the recurring entry; the existing one-shot must remain
	// unaffected (already fired anyway).
	require.True(
		t, a.Receive(ctx, &CancelTimeoutRequest{
			ID: "recurring",
		}).IsOk(),
	)

	clock.Advance(time.Second)
	require.Equal(t, 2, tickCB.count())

	// Schedule a new one-shot using an ID that previously hosted a
	// recurring entry. It must not collide.
	require.True(
		t, a.Receive(ctx, &ScheduleTimeoutRequest{
			ID:       "recurring",
			Duration: 50 * time.Millisecond,
			Callback: expiredCB,
		}).IsOk(),
	)

	clock.Advance(50 * time.Millisecond)
	require.Len(t, expiredCB.getMessages(), 2)
}

// TestActorScheduleReplacesExistingRecurring verifies that scheduling a
// one-shot with an ID currently held by a recurring entry stops the
// recurring one.
func TestActorScheduleReplacesExistingRecurring(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	expiredCB := newMockCallbackRef(t, "expired")
	tickCB := newMockTickCallback(t, "tick")

	require.True(
		t, a.Receive(ctx, &ScheduleRecurringTickRequest{
			ID:       "shared-id",
			Interval: 100 * time.Millisecond,
			Callback: tickCB,
		}).IsOk(),
	)

	clock.Advance(100 * time.Millisecond)
	require.Equal(t, 1, tickCB.count())

	require.True(
		t, a.Receive(ctx, &ScheduleTimeoutRequest{
			ID:       "shared-id",
			Duration: 200 * time.Millisecond,
			Callback: expiredCB,
		}).IsOk(),
	)

	clock.Advance(200 * time.Millisecond)
	require.Len(t, expiredCB.getMessages(), 1)

	// The recurring entry must be gone, with no further ticks despite
	// repeated time advances.
	clock.Advance(500 * time.Millisecond)
	require.Equal(t, 1, tickCB.count())
}

// TestActorRejectsNonPositiveInterval verifies that ScheduleRecurring-
// TickRequest with Interval <= 0 fails fast with a structured error
// rather than entering the fire/re-arm loop that a zero or negative
// interval would otherwise produce. The actor's existing state must
// remain untouched: a previously-scheduled recurring entry under a
// different ID continues to fire normally.
func TestActorRejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newMockTickCallback(t, "cb")

	// A live recurring entry the rejected request must not disturb.
	require.True(
		t, a.Receive(ctx, &ScheduleRecurringTickRequest{
			ID:       "live",
			Interval: 100 * time.Millisecond,
			Callback: cb,
		}).IsOk(),
	)

	cases := []struct {
		name     string
		interval time.Duration
	}{
		{
			"zero",
			0,
		},
		{
			"negative",
			-time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := a.Receive(ctx, &ScheduleRecurringTickRequest{
				ID:       "bad",
				Interval: tc.interval,
				Callback: cb,
			})
			require.True(t, res.IsErr())
			require.Contains(
				t, res.Err().Error(),
				"interval must be positive",
			)
		})
	}

	// The live entry is still live: advancing produces ticks at the
	// expected cadence, none of them attributed to the rejected ID.
	clock.Advance(300 * time.Millisecond)
	got := cb.snapshot()
	require.Len(t, got, 3)
	for _, m := range got {
		require.Equal(t, ID("live"), m.ID)
	}
}
