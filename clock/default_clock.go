package clock

import (
	"time"
)

// DefaultClock implements Clock interface by simply calling the appropriate
// time functions.
type DefaultClock struct{}

// NewDefaultClock constructs a new DefaultClock.
func NewDefaultClock() Clock {
	return &DefaultClock{}
}

// Now simply returns time.Now().
func (DefaultClock) Now() time.Time {
	return time.Now()
}

// TickAfter simply wraps time.After().
func (DefaultClock) TickAfter(duration time.Duration) <-chan time.Time {
	return time.After(duration)
}

// TickAt simply wraps time.After() with the duration until the passed time.
func (DefaultClock) TickAt(t time.Time) <-chan time.Time {
	return time.After(time.Until(t))
}
