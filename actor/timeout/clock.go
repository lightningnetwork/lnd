package timeout

import "time"

// Clock abstracts wall-clock time so tests can deterministically advance it.
// Production callers receive a RealClock implicitly via NewActor; tests
// instead pass a fake clock through NewActorWithClock and drive time forward
// step-by-step.
//
// AfterFunc is the only scheduling primitive: recurring ticks are built on top
// of it as a chain of one-shots that re-arm from inside the actor's Receive
// method, so there is no need for a separate Ticker abstraction.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// AfterFunc schedules f to run after d. The returned Stoppable can be
	// used to cancel the pending call.
	AfterFunc(d time.Duration, f func()) Stoppable
}

// Stoppable is a one-shot timer handle. *time.Timer satisfies this interface
// directly.
type Stoppable interface {
	// Stop prevents the timer from firing. Returns true if the call stops
	// the timer, false if the timer has already fired or been stopped.
	Stop() bool
}

// RealClock is the production Clock implementation. It delegates to the
// standard library time package, which also makes it the clock to use under
// testing/synctest: inside a bubble, time.Now and time.AfterFunc run on the
// bubble's fake clock.
type RealClock struct{}

// Now returns time.Now().
func (RealClock) Now() time.Time { return time.Now() }

// AfterFunc returns a *time.Timer wrapped as a Stoppable.
func (RealClock) AfterFunc(d time.Duration, f func()) Stoppable {
	return time.AfterFunc(d, f)
}
