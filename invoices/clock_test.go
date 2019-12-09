package invoices

import (
	"sync"
	"time"
)

// testClock can be used in tests to mock time.
type testClock struct {
	currentTime time.Time
	timeChanMap map[time.Time][]chan time.Time
	timeLock    sync.Mutex
}

// newTestClock returns a new test clock.
func newTestClock(startTime time.Time) *testClock {
	return &testClock{
		currentTime: startTime,
		timeChanMap: make(map[time.Time][]chan time.Time),
	}
}

// now returns the current (test) time.
func (c *testClock) now() time.Time {
	c.timeLock.Lock()
	defer c.timeLock.Unlock()

	return c.currentTime
}

// tickAfter returns a channel that will receive a tick at the specified time.
func (c *testClock) tickAfter(duration time.Duration) <-chan time.Time {
	c.timeLock.Lock()
	defer c.timeLock.Unlock()

	triggerTime := c.currentTime.Add(duration)
	log.Debugf("tickAfter called: duration=%v, trigger_time=%v",
		duration, triggerTime)

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

// setTime sets the (test) time and triggers tick channels when they expire.
func (c *testClock) setTime(now time.Time) {
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
