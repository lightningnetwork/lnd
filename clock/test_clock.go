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
	tickSignal  chan time.Duration
}

// NewTestClock returns a new test clock.
func NewTestClock(startTime time.Time) *TestClock {
	return &TestClock{
		currentTime: startTime,
		timeChanMap: make(map[time.Time][]chan time.Time),
	}
}

// NewTestClockWithTickSignal will create a new test clock with an added
// channel which will be used to signal when a new ticker is registered.
// This is useful when creating a ticker on a separate goroutine and we'd
// like to wait for that to happen before advancing the test case.
func NewTestClockWithTickSignal(startTime time.Time,
	tickSignal chan time.Duration) *TestClock {

	testClock := NewTestClock(startTime)
	testClock.tickSignal = tickSignal

	return testClock
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
	ch := c.tickAtLocked(c.currentTime.Add(duration))
	c.timeLock.Unlock()

	c.signalTick(duration)

	return ch
}

// TickAt implements Clock.TickAt for the test clock.
func (c *TestClock) TickAt(t time.Time) <-chan time.Time {
	c.timeLock.Lock()
	duration := t.Sub(c.currentTime)
	ch := c.tickAtLocked(t)
	c.timeLock.Unlock()

	c.signalTick(duration)

	return ch
}

// signalTick notifies the test that a tick was registered. The clock lock
// must be released before calling this method.
func (c *TestClock) signalTick(duration time.Duration) {
	if c.tickSignal != nil {
		c.tickSignal <- duration
	}
}

// tickAtLocked registers a tick for the passed absolute time. The caller must
// hold the clock lock. Tick registration is not signaled here; callers signal
// themselves after releasing the lock.
func (c *TestClock) tickAtLocked(triggerTime time.Time) <-chan time.Time {
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
