package timeout

import (
	"context"
	"sync"
	"testing"
	"time"
)

// startEpoch is the logical time at which fakeClock-driven tests start.
// It is arbitrary but stable across tests so timestamps are easy to
// reason about.
var startEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeClock is a deterministic Clock implementation for tests. Time only
// moves forward when Advance is called; AfterFunc callbacks fire in
// time-ordered fashion when their fire time has been reached.
type fakeClock struct {
	mu sync.Mutex

	now    time.Time
	afters []*fakeAfter
}

// newFakeClock returns a fakeClock pinned at start.
func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

// Now returns the current logical time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// AfterFunc registers f to run after d. The returned Stoppable cancels the
// scheduled call.
func (c *fakeClock) AfterFunc(d time.Duration, f func()) Stoppable {
	c.mu.Lock()
	defer c.mu.Unlock()

	a := &fakeAfter{
		fireAt: c.now.Add(d),
		f:      f,
	}
	c.afters = append(c.afters, a)

	return a
}

// Advance moves logical time forward by d. Due AfterFunc callbacks are
// invoked synchronously, in fire-time order; callbacks may register
// further AfterFunc entries (this is exactly what the recurring-tick
// chain does on each fire) and any new entries with a fireAt inside
// the [start, end] window are picked up in the same Advance call.
//
// Each fire advances the clock to that fire's fireAt before the
// callback runs, so a callback that consults Now() observes the
// timestamp at which its timer was supposed to fire, not the eventual
// end time. This keeps tick timestamps interval-aligned for tests.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()

		var next *fakeAfter
		for _, a := range c.afters {
			if a.isCancelledOrFired() {
				continue
			}

			if a.fireAt.After(end) {
				continue
			}

			if next == nil || a.fireAt.Before(next.fireAt) {
				next = a
			}
		}

		if next == nil {
			c.now = end
			c.mu.Unlock()

			return
		}

		c.now = next.fireAt
		c.mu.Unlock()

		if next.markFired() {
			next.f()
		}
	}
}

// fakeAfter is a one-shot Stoppable handle managed by fakeClock.
type fakeAfter struct {
	mu sync.Mutex

	fireAt    time.Time
	f         func()
	cancelled bool
	fired     bool
}

// Stop returns true if the call cancelled a pending fire.
func (a *fakeAfter) Stop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cancelled || a.fired {
		return false
	}

	a.cancelled = true

	return true
}

// isCancelledOrFired reports whether the handle can no longer fire.
func (a *fakeAfter) isCancelledOrFired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.cancelled || a.fired
}

// markFired claims the fire for the caller, returning false if the handle
// was already stopped or fired.
func (a *fakeAfter) markFired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cancelled || a.fired {
		return false
	}

	a.fired = true

	return true
}

// syncSelfRef is an actor.TellOnlyRef[Msg] that synchronously re-enters
// the actor's Receive method. Tests use this instead of an ActorSystem
// mailbox: the fake clock fires AfterFunc callbacks inline inside
// Advance, so when those callbacks Tell self the resulting Receive
// runs on the test's own goroutine and we keep deterministic ordering
// without needing real concurrency.
//
// This adapter is unsafe under genuine concurrent Receive: it is
// strictly for fake-clock-driven tests where the test serializes
// access. Real production wiring uses the ActorRef returned by
// actor.RegisterWithSystem.
type syncSelfRef struct {
	a  *Actor
	id string
}

// newSyncSelfRef returns a synchronous self-ref for a.
func newSyncSelfRef(a *Actor) *syncSelfRef {
	return &syncSelfRef{
		a:  a,
		id: "timeout-test-self",
	}
}

// ID returns the self-ref identifier.
func (s *syncSelfRef) ID() string { return s.id }

// Tell runs Receive inline on the caller's goroutine.
func (s *syncSelfRef) Tell(ctx context.Context, msg Msg) {
	s.a.Receive(ctx, msg)
}

// TryTell delegates to Tell, which is all this double needs: no test
// drives the non-blocking path through it.
func (s *syncSelfRef) TryTell(ctx context.Context, msg Msg) error {
	s.Tell(ctx, msg)

	return nil
}

// newTestActor constructs an Actor wired to a synchronous self-ref so
// internal AfterFunc callbacks loop back into Receive on the same
// goroutine that drove the schedule.
func newTestActor(clock Clock) *Actor {
	a := NewActorWithClock(clock)
	a.Start(newSyncSelfRef(a))

	return a
}

// mockTickCallback implements actor.TellOnlyRef[*TickFiredMsg] for tests.
type mockTickCallback struct {
	mu sync.Mutex

	t        *testing.T
	id       string
	messages []TickFiredMsg
}

// newMockTickCallback returns an empty recording tick callback.
func newMockTickCallback(t *testing.T, id string) *mockTickCallback {
	return &mockTickCallback{
		t:  t,
		id: id,
	}
}

// ID returns the callback identifier.
func (m *mockTickCallback) ID() string { return m.id }

// Tell records the tick.
func (m *mockTickCallback) Tell(_ context.Context, msg *TickFiredMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if msg == nil {
		return
	}

	m.messages = append(m.messages, *msg)
}

// TryTell delegates to Tell, which is all this double needs: no test
// drives the non-blocking path through it.
func (m *mockTickCallback) TryTell(ctx context.Context,
	msg *TickFiredMsg) error {

	m.Tell(ctx, msg)

	return nil
}

// count returns how many tick messages have arrived.
func (m *mockTickCallback) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.messages)
}

// snapshot returns a copy of the currently received messages.
func (m *mockTickCallback) snapshot() []TickFiredMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]TickFiredMsg{}, m.messages...)
}
