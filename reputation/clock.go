package reputation

import "time"

// Clock is a minimal time source, injected so that tests can drive the
// subsystem deterministically. In production it is backed by the system clock.
type Clock interface {
	// Now returns the current wall-clock time.
	Now() time.Time
}

// systemClock is the production Clock backed by time.Now.
type systemClock struct{}

// NewSystemClock returns a Clock backed by the system wall clock.
func NewSystemClock() Clock {
	return systemClock{}
}

// Now returns the current system time.
func (systemClock) Now() time.Time {
	return time.Now()
}

// unixSeconds is a small helper to express a time as whole unix seconds, the
// granularity used throughout the reputation math (matching the LDK reference,
// which works in unix seconds).
func unixSeconds(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}

	return uint64(s)
}
