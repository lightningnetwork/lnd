package clock

import (
	"sync"
	"time"
)

// TestClock can be used in tests to mock time.
type TestClock struct {
	currentTime time.Time
	timeChanMap map[time.Time][]chan time.Time
	timeLock    sync.Mutex
}

// NewTestClock returns a new test clock.
func NewTestClock(startTime time.Time) *TestClock {
	return &TestClock{
		currentTime: startTime,
		timeChanMap: make(map[time.Time][]chan time.Time),
	}
}

// Now returns the current (test) time.
func (c *TestClock) Now() time.Time {
	c.timeLock.Lock()
	defer c.timeLock.Unlock()

	return c.currentTime
}

// TickAfter returns a channel that will receive a tick after the specified
// duration has passed passed by the user set test time.
func (c *TestClock) TickAfter(duration time.Duration) <-chan time.Time {
	c.timeLock.Lock()
	defer c.timeLock.Unlock()

	triggerTime := c.currentTime.Add(duration)
	ch := make(chan time.Time, 1)

	// If already expired, tick immediately.
	if !triggerTime.After(c.currentTime) {
		ch <- c.currentTime
		return ch
	}

	// Otherwise store the channel until the trigger time is there.
	chans := c.timeChanMap[triggerTime]
	chans = append(chans, ch)
	c.timeChanMap[triggerTime] = chans

	return ch
}

// SetTime sets the (test) time and triggers tick channels when they expire.
func (c *TestClock) SetTime(now time.Time) {
	c.timeLock.Lock()
	defer c.timeLock.Unlock()

	c.currentTime = now
	remainingChans := make(map[time.Time][]chan time.Time)
	for triggerTime, chans := range c.timeChanMap {
		// If the trigger time is still in the future, keep this channel
		// in the channel map for later.
		if triggerTime.After(now) {
			remainingChans[triggerTime] = chans
			continue
		}

		for _, c := range chans {
			c <- now
		}
	}

	c.timeChanMap = remainingChans
}
