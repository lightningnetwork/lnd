package timeout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestPropertyRecurringTickCount asserts the central invariant of the
// recurring-tick scheduler: for any (interval, totalAdvance, cancelAt),
// the number of TickFiredMsg delivered to the callback equals
// floor(min(cancelAt, totalAdvance) / interval).
//
// Under the self-tell model, fakeClock.Advance drives each fire
// synchronously and the actor's chain re-arms inside Receive on the
// same goroutine, so cb.count is fully consistent the moment Advance
// returns, with no eventual polling needed.
func TestPropertyRecurringTickCount(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		intervalMs := rapid.IntRange(10, 200).Draw(rt, "intervalMs")
		totalMs := rapid.IntRange(0, 2_000).Draw(rt, "totalMs")
		cancelMs := rapid.IntRange(0, 2_000).Draw(rt, "cancelMs")

		interval := time.Duration(intervalMs) * time.Millisecond
		total := time.Duration(totalMs) * time.Millisecond
		cancelAt := time.Duration(cancelMs) * time.Millisecond

		ctx := rt.Context()
		clock := newFakeClock(startEpoch)
		a := newTestActor(clock)

		cb := newMockTickCallback(t, "rapid")

		require.True(
			rt, a.Receive(ctx, &ScheduleRecurringTickRequest{
				ID:       "rapid-tick",
				Interval: interval,
				Callback: cb,
			}).IsOk(),
		)

		// Decide whether cancel happens during the run window or
		// after it. If cancelAt < total, advance to cancelAt,
		// cancel, then advance the rest. Otherwise advance the
		// whole window without cancelling.
		var effectiveWindow time.Duration
		if cancelAt < total {
			effectiveWindow = cancelAt
			clock.Advance(cancelAt)

			require.True(
				rt, a.Receive(ctx, &CancelTimeoutRequest{
					ID: "rapid-tick",
				}).IsOk(),
			)

			clock.Advance(total - cancelAt)
		} else {
			effectiveWindow = total
			clock.Advance(total)
		}

		expected := int(effectiveWindow / interval)

		require.Equal(
			rt, expected, cb.count(),
			"interval=%s total=%s cancelAt=%s expected %d ticks",
			interval, total, cancelAt, expected,
		)
	})
}
