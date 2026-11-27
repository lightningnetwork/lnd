package clock

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	testTime = time.Date(2009, time.January, 3, 12, 0, 0, 0, time.UTC)
)

func TestNow(t *testing.T) {
	c := NewTestClock(testTime)
	now := c.Now()
	require.Equal(t, testTime, now)

	now = now.Add(time.Hour)
	c.SetTime(now)
	require.Equal(t, now, c.Now())
}

func TestTickAfter(t *testing.T) {
	c := NewTestClock(testTime)

	// Should be ticking immediately.
	ticker0 := c.TickAfter(0)

	// Both should be ticking after SetTime
	ticker1 := c.TickAfter(time.Hour)
	ticker2 := c.TickAfter(time.Hour)

	// We don't expect this one to tick.
	ticker3 := c.TickAfter(2 * time.Hour)

	tickOrTimeOut := func(ticker <-chan time.Time, expectTick bool) {
		tick := false
		select {
		case <-ticker:
			tick = true

		case <-time.After(time.Millisecond):
		}

		require.Equal(t, expectTick, tick)
	}

	tickOrTimeOut(ticker0, true)
	tickOrTimeOut(ticker1, false)
	tickOrTimeOut(ticker2, false)
	tickOrTimeOut(ticker3, false)

	c.SetTime(c.Now().Add(time.Hour))

	tickOrTimeOut(ticker1, true)
	tickOrTimeOut(ticker2, true)
	tickOrTimeOut(ticker3, false)
}

// TestTickAt tests that TickAt registers a tick at the absolute time,
// firing immediately for past times and exactly at the trigger time
// otherwise.
func TestTickAt(t *testing.T) {
	c := NewTestClock(testTime)

	// Should be ticking immediately, the time has already passed.
	ticker0 := c.TickAt(testTime)

	// Both should tick after SetTime reaches the trigger time.
	ticker1 := c.TickAt(testTime.Add(time.Hour))
	ticker2 := c.TickAt(testTime.Add(time.Hour))

	// We don't expect this one to tick.
	ticker3 := c.TickAt(testTime.Add(2 * time.Hour))

	tickOrTimeOut := func(ticker <-chan time.Time, expectTick bool) {
		tick := false
		select {
		case <-ticker:
			tick = true

		case <-time.After(time.Millisecond):
		}

		require.Equal(t, expectTick, tick)
	}

	tickOrTimeOut(ticker0, true)
	tickOrTimeOut(ticker1, false)
	tickOrTimeOut(ticker2, false)
	tickOrTimeOut(ticker3, false)

	// Advance past the trigger time of the first two tickers, all of
	// them should tick even though the time is advanced beyond the
	// trigger time.
	c.SetTime(testTime.Add(90 * time.Minute))

	tickOrTimeOut(ticker1, true)
	tickOrTimeOut(ticker2, true)
	tickOrTimeOut(ticker3, false)
}

// TestTickAtRaceSetTime demonstrates the lost-tick race that TickAt
// prevents: with the TickAfter(t.Sub(Now())) composition, advancing the
// clock between reading Now() and registering the tick schedules the
// trigger at deadline+advancement, so a subsequent SetTime that already
// passed the intended deadline never fires the tick. TickAt registers the
// absolute trigger time atomically and is immune.
func TestTickAtRaceSetTime(t *testing.T) {
	c := NewTestClock(testTime)

	deadline := testTime.Add(time.Hour)

	// Simulate the racy composition TickAfter(t.Sub(Now())) with a
	// clock advancement between the two calls, as can happen when the
	// registering goroutine is preempted.
	duration := deadline.Sub(c.Now())
	c.SetTime(testTime.Add(90 * time.Minute))
	racyTicker := c.TickAfter(duration)

	// The racy registration scheduled the trigger at 2.5h instead of
	// 1h, so advancing the clock past the deadline does not fire it.
	select {
	case <-racyTicker:
		require.FailNow(t, "racy tick unexpectedly fired")
	case <-time.After(time.Millisecond):
	}

	// The atomic registration fires because the clock already passed
	// the deadline.
	atomicTicker := c.TickAt(deadline)
	select {
	case <-atomicTicker:
	case <-time.After(time.Second):
		require.FailNow(t, "atomic tick did not fire")
	}
}

// TestTickSignal tests that TickAfter signals registration allowing
// safe time advancement.
func TestTickSignal(t *testing.T) {
	const interval = time.Second

	ch := make(chan time.Duration)
	c := NewTestClockWithTickSignal(testTime, ch)
	err := make(chan error, 1)

	go func() {
		select {
		// TickAfter will signal registration but will not
		// tick, unless we read the signal and set the time.
		case <-c.TickAfter(interval):
			err <- nil

		// Signal timeout if tick didn't happen.
		case <-time.After(time.Second):
			err <- fmt.Errorf("timeout")
		}
	}()

	tick := <-ch
	// Expect that the interval is correctly passed over the channel.
	require.Equal(t, interval, tick)

	// Once the ticker is registered, set the time to make it fire.
	c.SetTime(testTime.Add(time.Second))
	require.NoError(t, <-err)
}
