package timeout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// wedgeableCallbackRef stands in for a requester whose mailbox has filled up.
// While wedged, every non-blocking send is refused; releasing it makes the
// next attempt succeed.
type wedgeableCallbackRef struct {
	mu sync.Mutex

	id       string
	wedged   bool
	err      error
	attempts int
	msgs     []ExpiredMsg
}

// newWedgeableCallbackRef returns a callback that refuses deliveries with err
// until release is called.
func newWedgeableCallbackRef(id string, err error) *wedgeableCallbackRef {
	return &wedgeableCallbackRef{
		id:     id,
		wedged: true,
		err:    err,
	}
}

// ID returns the callback identifier.
func (w *wedgeableCallbackRef) ID() string { return w.id }

// Tell records the message unconditionally. The timeout actor must never
// reach for this path, so a test that sees a message arrive here has caught a
// regression back to blocking delivery.
func (w *wedgeableCallbackRef) Tell(_ context.Context, msg *ExpiredMsg) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.msgs = append(w.msgs, *msg)
}

// TryTell refuses the delivery while wedged and records it once released.
func (w *wedgeableCallbackRef) TryTell(_ context.Context,
	msg *ExpiredMsg) error {

	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempts++

	if w.wedged {
		return w.err
	}

	w.msgs = append(w.msgs, *msg)

	return nil
}

// release lets subsequent deliveries through.
func (w *wedgeableCallbackRef) release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.wedged = false
}

// snapshot returns the messages delivered so far and the number of delivery
// attempts made.
func (w *wedgeableCallbackRef) snapshot() ([]ExpiredMsg, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]ExpiredMsg{}, w.msgs...), w.attempts
}

// wedgeableTickRef is the recurring-tick counterpart of
// wedgeableCallbackRef.
type wedgeableTickRef struct {
	mu sync.Mutex

	id       string
	wedged   bool
	err      error
	attempts int
	msgs     []TickFiredMsg
}

// newWedgeableTickRef returns a tick callback that refuses deliveries with a
// full mailbox until release is called. Tests override err to model a
// different failure.
func newWedgeableTickRef(id string) *wedgeableTickRef {
	return &wedgeableTickRef{
		id:     id,
		wedged: true,
		err:    actor.ErrMailboxFull,
	}
}

// ID returns the callback identifier.
func (w *wedgeableTickRef) ID() string { return w.id }

// Tell records the tick unconditionally; the actor must not use this path.
func (w *wedgeableTickRef) Tell(_ context.Context, msg *TickFiredMsg) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.msgs = append(w.msgs, *msg)
}

// TryTell refuses the tick while wedged and records it once released.
func (w *wedgeableTickRef) TryTell(_ context.Context, msg *TickFiredMsg) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempts++

	if w.wedged {
		return w.err
	}

	w.msgs = append(w.msgs, *msg)

	return nil
}

// release lets subsequent ticks through.
func (w *wedgeableTickRef) release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.wedged = false
}

// snapshot returns the ticks delivered so far and the attempt count.
func (w *wedgeableTickRef) snapshot() ([]TickFiredMsg, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]TickFiredMsg{}, w.msgs...), w.attempts
}

// TestRetryDelayBackoff verifies the backoff doubles from the base delay and
// then flattens at the cap, including for attempt counts large enough to
// overflow a naive shift.
func TestRetryDelayBackoff(t *testing.T) {
	t.Parallel()

	require.Equal(t, retryBaseDelay, retryDelay(0))
	require.Equal(t, time.Second, retryDelay(1))
	require.Equal(t, 2*time.Second, retryDelay(2))
	require.Equal(t, 4*time.Second, retryDelay(3))
	require.Equal(t, 8*time.Second, retryDelay(4))
	require.Equal(t, 16*time.Second, retryDelay(5))
	require.Equal(t, retryMaxDelay, retryDelay(6))
	require.Equal(t, retryMaxDelay, retryDelay(1000))
}

// TestTickRetryDelayStartsFromInterval verifies that a fast ticker starts its
// backoff from its own cadence rather than jumping to the one second base,
// while a ticker slower than that base keeps the standard backoff. It is the
// starting point that moves and nothing else: the doubling and the 30s
// ceiling apply to a recurring entry exactly as they do to a one-shot, so a
// consumer that stays unreachable backs off well past its own interval.
func TestTickRetryDelayStartsFromInterval(t *testing.T) {
	t.Parallel()

	const fast = 100 * time.Millisecond

	require.Equal(t, fast, tickRetryDelay(fast, 1))
	require.Equal(t, 2*fast, tickRetryDelay(fast, 2))
	require.Equal(t, 4*fast, tickRetryDelay(fast, 3))

	// The ceiling is the global one, not the interval. A ticker whose
	// consumer stays unreachable ends up far slower than its configured
	// cadence, which is the point: an attempt against a callback backed
	// by a persistent mailbox may be a bounded database write.
	require.Equal(t, retryMaxDelay, tickRetryDelay(fast, 1000))
	require.Greater(t, tickRetryDelay(fast, 1000), fast)

	// A slow ticker is already slower than the base, so nothing is
	// capped and it behaves exactly like a one-shot retry.
	const slow = time.Minute

	require.Equal(t, retryBaseDelay, tickRetryDelay(slow, 1))
	require.Equal(t, 2*retryBaseDelay, tickRetryDelay(slow, 2))

	// A nonsensical interval must not spin the doubling loop.
	require.Equal(t, retryBaseDelay, tickRetryDelay(0, 1))
}

// TestTimeoutRetriesTransientFailures is the regression test for treating
// every non-terminal delivery error as permanent. A callback behind a custom
// persistent mailbox may never report a full mailbox: its enqueue can be a
// database write that surfaces a deadline when the database is slow, and a
// Router reports that it momentarily has no actor. Discarding the reminder in
// those cases is worse than a blocking send would be, because nothing
// re-derives it.
func TestTimeoutRetriesTransientFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "mailbox full",
			err:  actor.ErrMailboxFull,
		},
		{
			// What a persistent mailbox may return when the write
			// does not finish inside its internal deadline.
			name: "slow persistent write",
			err: fmt.Errorf("enqueue message: %w",
				context.DeadlineExceeded),
		},
		{
			// What a Router returns before its target registers,
			// or while the target is being replaced.
			name: "no actors available",
			err:  actor.ErrNoActorsAvailable,
		},
		{
			name: "unknown transport failure",
			err:  errors.New("connection reset"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			clock := newFakeClock(startEpoch)
			a := newTestActor(clock)
			cb := newWedgeableCallbackRef("transient", tc.err)

			res := a.Receive(ctx, &ScheduleTimeoutRequest{
				ID:       "transient",
				Duration: 50 * time.Millisecond,
				Callback: cb,
			})
			require.True(t, res.IsOk())

			// The failure must not consume the reminder.
			clock.Advance(50 * time.Millisecond)

			msgs, attempts := cb.snapshot()
			require.Empty(t, msgs)
			require.Equal(t, 1, attempts)
			require.Len(t, a.oneshots, 1)

			// Once the requester recovers, the held reminder is
			// delivered on the next retry.
			cb.release()
			clock.Advance(retryBaseDelay)

			msgs, _ = cb.snapshot()
			require.Len(t, msgs, 1)
			require.Equal(t, ID("transient"), msgs[0].ID)
			require.Empty(t, a.oneshots)
		})
	}
}

// TestTimeoutDropsTerminalCallbacks verifies the error that really is an end
// state, bare or wrapped, releases the entry instead of retrying into a
// stopped actor forever. The lnd actor package reports a closed mailbox as
// ErrActorTerminated too, so there is no separate closed-mailbox case.
func TestTimeoutDropsTerminalCallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "actor terminated",
			err:  actor.ErrActorTerminated,
		},
		{
			name: "wrapped terminal error",
			err: fmt.Errorf("deliver expiry: %w",
				actor.ErrActorTerminated),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			clock := newFakeClock(startEpoch)
			a := newTestActor(clock)
			cb := newWedgeableCallbackRef("terminal", tc.err)

			res := a.Receive(ctx, &ScheduleTimeoutRequest{
				ID:       "terminal",
				Duration: 50 * time.Millisecond,
				Callback: cb,
			})
			require.True(t, res.IsOk())

			clock.Advance(50 * time.Millisecond)
			require.Empty(t, a.oneshots)

			// No retry chain outlives the entry.
			clock.Advance(time.Minute)

			msgs, attempts := cb.snapshot()
			require.Empty(t, msgs)
			require.Equal(t, 1, attempts)
		})
	}
}

// TestTickRetriesTransientFailure verifies the recurring path shares the
// one-shot verdict: a transient failure holds the tick and retries, and only
// a terminal one stops the ticker.
func TestTickRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("slow-db-tick")
	cb.err = fmt.Errorf("enqueue message: %w", context.DeadlineExceeded)

	res := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "tick",
		Interval: time.Second,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(time.Second)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
	require.Len(t, a.recurring, 1)

	cb.release()
	clock.Advance(retryBaseDelay)

	msgs, _ = cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, startEpoch.Add(time.Second), msgs[0].FiredAt)
}

// TestTickStopsOnTerminalCallback verifies a ticker whose consumer is gone
// releases its entry rather than re-arming forever.
func TestTickStopsOnTerminalCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("dead-tick")
	cb.err = actor.ErrActorTerminated

	res := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "tick",
		Interval: time.Second,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(time.Second)
	require.Empty(t, a.recurring)

	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestRescheduleSupersedesArmedRetry verifies a fresh schedule for an ID with
// a retry in flight wins outright: the old reminder never lands, and the new
// one fires on its own duration.
func TestRescheduleSupersedesArmedRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "resched",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Len(t, a.oneshots, 1)

	// Re-scheduling the same ID replaces the entry, which stops the armed
	// retry and resets the attempt count with it.
	healthy := newWedgeableCallbackRef("healthy", actor.ErrMailboxFull)
	healthy.release()

	res = a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "resched",
		Duration: 10 * time.Second,
		Callback: healthy,
	})
	require.True(t, res.IsOk())

	// The old retry instant passes without waking the old callback.
	cb.release()
	clock.Advance(5 * time.Second)

	msgs, _ := cb.snapshot()
	require.Empty(t, msgs)

	msgs, _ = healthy.snapshot()
	require.Empty(t, msgs)

	// The replacement fires on its own schedule.
	clock.Advance(5 * time.Second)

	msgs, _ = healthy.snapshot()
	require.Len(t, msgs, 1)
	require.Empty(t, a.oneshots)
}

// TestCancelDuringRecurringRetry verifies a cancel lands even while a tick is
// held for retry: the ticker stops and the held tick is never delivered.
func TestCancelDuringRecurringRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("wedged-tick")

	res := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "tick",
		Interval: time.Second,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(time.Second)
	require.Len(t, a.recurring, 1)

	res = a.Receive(ctx, &CancelTimeoutRequest{ID: "tick"})
	require.True(t, res.IsOk())
	require.Empty(t, a.recurring)

	cb.release()
	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestTimeoutRetriesWedgedCallback verifies an expiry that cannot be
// delivered is held and re-armed on a growing backoff, then delivered intact
// once the requester drains. Nothing is lost and nothing is duplicated.
func TestTimeoutRetriesWedgedCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "retried",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	// First fire: the requester has no room, so the entry survives with a
	// retry armed one second out.
	clock.Advance(50 * time.Millisecond)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
	require.Len(t, a.oneshots, 1)

	// Nothing happens before the backoff elapses.
	clock.Advance(500 * time.Millisecond)

	_, attempts = cb.snapshot()
	require.Equal(t, 1, attempts)

	// Second attempt, still wedged. The next retry doubles to two
	// seconds, so a one second advance is not enough to trigger a third.
	clock.Advance(500 * time.Millisecond)
	clock.Advance(time.Second)

	msgs, attempts = cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 2, attempts)

	// Once the requester drains, the held expiry lands on the next retry.
	cb.release()
	clock.Advance(2 * time.Second)

	msgs, attempts = cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, ID("retried"), msgs[0].ID)
	require.Equal(t, 3, attempts)

	// The reminder is discharged, so no entry and no timer outlive it.
	require.Empty(t, a.oneshots)

	clock.Advance(time.Minute)

	msgs, _ = cb.snapshot()
	require.Len(t, msgs, 1)
}

// TestTimeoutRetryStaysCancellable verifies a held expiry is still owned by
// its ID: cancelling it stops the retry chain instead of leaving a reminder
// that fires long after the requester asked for it to go away.
func TestTimeoutRetryStaysCancellable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "cancelled",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Len(t, a.oneshots, 1)

	res = a.Receive(ctx, &CancelTimeoutRequest{ID: "cancelled"})
	require.True(t, res.IsOk())
	require.Empty(t, a.oneshots)

	// Even a healthy requester hears nothing more about a cancelled
	// timeout.
	cb.release()
	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestTimeoutDropsUnreachableCallback verifies a permanently gone requester
// does not earn a retry chain: there is nothing a backoff could fix, so the
// entry is released.
func TestTimeoutDropsUnreachableCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("dead", actor.ErrActorTerminated)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "dropped",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Empty(t, a.oneshots)

	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestTickRetryPreservesFireInstant verifies a tick the consumer could not
// take is held rather than dropped, and that the retry reports when the timer
// actually fired instead of when delivery finally succeeded.
func TestTickRetryPreservesFireInstant(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("wedged-tick")

	res := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "tick",
		Interval: 100 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	firedAt := startEpoch.Add(100 * time.Millisecond)

	clock.Advance(100 * time.Millisecond)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)

	// The retry base is capped at the interval, so the second attempt
	// comes one interval later rather than a full second later.
	clock.Advance(100 * time.Millisecond)

	msgs, attempts = cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 2, attempts)

	// The retry replaces the cadence while the consumer is behind, so no
	// second tick piles up during the backoff window.
	cb.release()
	clock.Advance(200 * time.Millisecond)

	msgs, attempts = cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, 3, attempts)
	require.Equal(t, firedAt, msgs[0].FiredAt)

	// With the consumer drained, the ticker resumes its normal interval.
	clock.Advance(100 * time.Millisecond)

	msgs, _ = cb.snapshot()
	require.Len(t, msgs, 2)
	require.True(t, msgs[1].FiredAt.After(msgs[0].FiredAt))
}

// chanRef is a channel-backed actor.TellOnlyRef test double. TryTell never
// blocks and reports actor.ErrMailboxFull once the buffer is full, which is
// the same contract a real actor mailbox offers. Tell blocks until there is
// room or the context is done.
type chanRef[M actor.Message] struct {
	id string
	ch chan M
}

// newChanRef returns a chanRef whose buffer holds size messages.
func newChanRef[M actor.Message](id string, size int) *chanRef[M] {
	return &chanRef[M]{
		id: id,
		ch: make(chan M, size),
	}
}

// ID returns the ref's identifier.
func (c *chanRef[M]) ID() string { return c.id }

// Tell enqueues msg, blocking until there is room or ctx is done.
func (c *chanRef[M]) Tell(ctx context.Context, msg M) {
	select {
	case c.ch <- msg:
	case <-ctx.Done():
	}
}

// TryTell enqueues msg if the buffer has room and reports ErrMailboxFull
// otherwise.
func (c *chanRef[M]) TryTell(ctx context.Context, msg M) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case c.ch <- msg:
		return nil
	default:
		return actor.ErrMailboxFull
	}
}

// Messages exposes the buffered messages for the test to drain.
func (c *chanRef[M]) Messages() <-chan M { return c.ch }

// AwaitMessage waits up to timeout for the next message.
func (c *chanRef[M]) AwaitMessage(timeout time.Duration) (M, bool) {
	select {
	case msg := <-c.ch:
		return msg, true
	case <-time.After(timeout):
		var zero M
		return zero, false
	}
}

// blockingCallbackRef parks any blocking send until the test releases it,
// while reporting a full mailbox to the non-blocking one. It models the
// shape that wedges a shared timer service: a requester whose mailbox is full
// and stays full.
type blockingCallbackRef struct {
	id      string
	release chan struct{}
	once    sync.Once

	mu       sync.Mutex
	attempts int
}

// newBlockingCallbackRef creates a callback whose Tell never returns until
// released.
func newBlockingCallbackRef(id string) *blockingCallbackRef {
	return &blockingCallbackRef{
		id:      id,
		release: make(chan struct{}),
	}
}

// ID returns the callback identifier.
func (b *blockingCallbackRef) ID() string { return b.id }

// Tell parks the caller until the test releases it.
func (b *blockingCallbackRef) Tell(ctx context.Context, _ *ExpiredMsg) {
	b.count()

	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

// TryTell reports the mailbox as full without ever parking.
func (b *blockingCallbackRef) TryTell(_ context.Context, _ *ExpiredMsg) error {
	b.count()

	return actor.ErrMailboxFull
}

// count records a delivery attempt.
func (b *blockingCallbackRef) count() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.attempts++
}

// attemptCount returns how many deliveries have been attempted.
func (b *blockingCallbackRef) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.attempts
}

// stop releases any parked sender.
func (b *blockingCallbackRef) stop() {
	b.once.Do(func() {
		close(b.release)
	})
}

// TestTimeoutActorStaysResponsiveWhileCallbackWedged is the regression test
// for a shared timer freeze. A timeout actor is typically shared by many
// requesters, so if one wedged requester can park its receive goroutine, every
// other requester's timers stop with it. Here a wedged callback must not stop
// the actor from accepting and firing an unrelated timeout.
func TestTimeoutActorStaysResponsiveWhileCallbackWedged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	behavior := NewActor()
	host, err := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "timeout-responsive",
		Behavior:    behavior,
		MailboxSize: 8,
	})
	require.NoError(t, err)
	host.Start()
	t.Cleanup(host.Stop)

	behavior.Start(host.Ref())

	wedged := newBlockingCallbackRef("wedged")
	t.Cleanup(wedged.stop)

	host.Ref().Tell(ctx, &ScheduleTimeoutRequest{
		ID:       "wedged",
		Duration: 10 * time.Millisecond,
		Callback: wedged,
	})

	// Wait until the actor has tried to hand the expiry over. From this
	// point on, a blocking delivery would have the receive goroutine
	// parked for good.
	require.Eventually(t, func() bool {
		return wedged.attemptCount() > 0
	}, 5*time.Second, 5*time.Millisecond)

	// The actor must still accept new work.
	askCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	healthy := newChanRef[*ExpiredMsg]("healthy", 1)
	schedule := &ScheduleTimeoutRequest{
		ID:       "healthy",
		Duration: 10 * time.Millisecond,
		Callback: healthy,
	}

	res := host.Ref().Ask(askCtx, schedule).Await(askCtx)
	require.True(t, res.IsOk(), "schedule was not acknowledged")

	// And it must still fire the timers it accepted.
	_, ok := healthy.AwaitMessage(5 * time.Second)
	require.True(t, ok, "timer never fired while a callback was wedged")
}

// goroutineFloor samples the process goroutine count a few times and returns
// the smallest reading, which filters out the short-lived goroutines a timer
// fire spawns and leaves only the ones that are actually parked.
func goroutineFloor() int {
	floor := runtime.NumGoroutine()
	for range 10 {
		time.Sleep(5 * time.Millisecond)

		if n := runtime.NumGoroutine(); n < floor {
			floor = n
		}
	}

	return floor
}

// TestSelfSignalDoesNotLeakGoroutines verifies that timer fires landing on a
// backed up actor coalesce instead of stacking parked senders. A blocking
// self-tell leaked one goroutine per fire for as long as the actor stayed
// behind, which is precisely when timers fire fastest.
func TestSelfSignalDoesNotLeakGoroutines(t *testing.T) {
	// Deliberately not parallel: this test counts goroutines
	// process-wide.
	ctx := t.Context()

	a := NewActor()

	// A self-ref whose mailbox is full and stays full, so every internal
	// fire meets a target that will not take it.
	wedgedSelf := newChanRef[Msg]("wedged-self", 1)
	require.NoError(t, wedgedSelf.TryTell(ctx, &internalTimerFired{}))

	a.Start(wedgedSelf)

	t.Cleanup(func() {
		// Drain the queued fires so the retry chains find room and
		// end, rather than spinning for the rest of the run.
		for {
			select {
			case <-wedgedSelf.Messages():
			case <-time.After(100 * time.Millisecond):
				return
			}
		}
	})

	callback := newChanRef[*ExpiredMsg]("callback", 1)

	before := goroutineFloor()

	const timers = 40

	for i := range timers {
		res := a.Receive(ctx, &ScheduleTimeoutRequest{
			ID:       ID(fmt.Sprintf("leak-%d", i)),
			Duration: 5 * time.Millisecond,
			Callback: callback,
		})
		require.True(t, res.IsOk())
	}

	// Long enough for every timer to fire and retry several times over.
	time.Sleep(300 * time.Millisecond)

	after := goroutineFloor()

	require.Less(
		t, after, before+timers/2,
		"timer fires parked goroutines on the wedged mailbox",
	)
}

// TestSelfSignalRetriesUntilMailboxDrains verifies the coalesced fire is a
// deferral and not a drop: once the actor's own mailbox has room again, the
// fire it could not accept arrives.
func TestSelfSignalRetriesUntilMailboxDrains(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := NewActorWithClock(clock)

	self := newChanRef[Msg]("full-self", 1)
	require.NoError(t, self.TryTell(ctx, &internalTimerFired{}))

	a.Start(self)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "deferred",
		Duration: 50 * time.Millisecond,
		Callback: newMockCallbackRef(t, "callback"),
	})
	require.True(t, res.IsOk())

	// The fire finds no room, so it is re-armed rather than dropped or
	// parked.
	clock.Advance(50 * time.Millisecond)

	// Make room and let the retry run.
	<-self.Messages()
	clock.Advance(selfSignalRetry)

	fired, ok := self.AwaitMessage(time.Second)
	require.True(t, ok, "deferred fire never arrived")
	require.Equal(t, "internalTimerFired", fired.MessageType())
}

// TestActorLogsThroughInjectedLogger verifies that a logger supplied through
// Config.Log receives the actor's diagnostics, overriding the package-level
// logger, which is disabled by default.
func TestActorLogsThroughInjectedLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	handler := btclog.NewDefaultHandler(&buf, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelWarn)

	clock := newFakeClock(startEpoch)
	a := NewActorWithConfig(Config{
		Clock: fn.Some[Clock](clock),
		Log:   fn.Some(btclog.NewSLogger(handler)),
	})
	a.Start(newSyncSelfRef(a))

	cb := newWedgeableCallbackRef("wedged-requester", actor.ErrMailboxFull)

	// The package logger is disabled, so anything that reaches the buffer
	// got there through the injected one.
	res := a.Receive(t.Context(), &ScheduleTimeoutRequest{
		ID:       "logged",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)

	logged := buf.String()
	require.Contains(t, logged, "Timeout callback delivery failed")
	require.Contains(t, logged, "wedged-requester")
	require.Contains(t, logged, "logged")
}
