package clock

import (
	"testing"
	"time"
)

var (
	testTime = time.Date(2009, time.January, 3, 12, 0, 0, 0, time.UTC)
)

func TestNow(t *testing.T) {
	c := NewTestClock(testTime)
	now := c.Now()

	if now != testTime {
		t.Fatalf("expected: %v, got: %v", testTime, now)
	}

	now = now.Add(time.Hour)
	c.SetTime(now)
	if c.Now() != now {
		t.Fatalf("epected: %v, got: %v", now, c.Now())
	}
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

		if tick != expectTick {
			t.Fatalf("expected tick: %v, ticked: %v", expectTick, tick)
		}
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
