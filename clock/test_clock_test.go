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
