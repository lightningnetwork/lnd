package timeout

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/stretchr/testify/require"
)

// deliveryRecord is one message a timedCallbackRef accepted, stamped with the
// clock reading at the instant of delivery.
type deliveryRecord struct {
	id ID
	at time.Time
}

// timedCallbackRef records every expiry it accepts along with time.Now() at
// the moment of delivery. Inside a synctest bubble time.Now() reads the
// bubble's fake clock, so the timestamps are exact.
type timedCallbackRef struct {
	mu sync.Mutex

	id      string
	records []deliveryRecord
}

// ID returns the callback identifier.
func (c *timedCallbackRef) ID() string { return c.id }

// Tell records the expiry.
func (c *timedCallbackRef) Tell(_ context.Context, msg *ExpiredMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.records = append(c.records, deliveryRecord{
		id: msg.ID,
		at: time.Now(),
	})
}

// TryTell records the expiry and always succeeds.
func (c *timedCallbackRef) TryTell(ctx context.Context,
	msg *ExpiredMsg) error {

	c.Tell(ctx, msg)

	return nil
}

// snapshot returns a copy of the deliveries recorded so far.
func (c *timedCallbackRef) snapshot() []deliveryRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]deliveryRecord{}, c.records...)
}

// startSynctestActor registers a real timeout actor, backed by RealClock, in a
// fresh actor system. It must be called from inside a synctest bubble so that
// the system's goroutines, channels and timers all belong to the bubble. The
// system is shut down when the test ends, which still happens inside the
// bubble because synctest.Test runs t.Cleanup callbacks before it returns.
func startSynctestActor(t *testing.T) (*Actor, actor.ActorRef[Msg, Resp]) {
	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown())
	})

	behavior := NewActor()
	key := actor.NewServiceKey[Msg, Resp]("synctest-timeout")
	ref, err := actor.RegisterWithSystem(
		system, "synctest-timeout", key, behavior,
	)
	require.NoError(t, err)

	behavior.Start(ref)

	return behavior, ref
}

// TestSynctestExpiryOrder runs the real actor, mailbox and RealClock inside a
// synctest bubble and verifies that timeouts scheduled out of order expire in
// duration order, each at exactly its deadline and not a moment later.
func TestSynctestExpiryOrder(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		_, ref := startSynctestActor(t)
		cb := &timedCallbackRef{id: "ordered"}

		start := time.Now()

		// Schedule deliberately out of duration order, so the expiry
		// order cannot simply echo the scheduling order.
		schedule := []struct {
			id       ID
			duration time.Duration
		}{
			{"third", 300 * time.Millisecond},
			{"first", 100 * time.Millisecond},
			{"second", 200 * time.Millisecond},
		}
		for _, s := range schedule {
			ref.Tell(ctx, &ScheduleTimeoutRequest{
				ID:       s.id,
				Duration: s.duration,
				Callback: cb,
			})
		}

		// Let the actor process every schedule request before time
		// moves.
		synctest.Wait()
		require.Empty(t, cb.snapshot())

		// Step through each deadline. After every step exactly one more
		// expiry must have landed, stamped with that deadline.
		want := []ID{"first", "second", "third"}
		for i, id := range want {
			time.Sleep(100 * time.Millisecond)
			synctest.Wait()

			got := cb.snapshot()
			require.Len(t, got, i+1)
			require.Equal(t, id, got[i].id)

			deadline := start.Add(
				time.Duration(i+1) * 100 * time.Millisecond,
			)
			require.True(
				t, deadline.Equal(got[i].at),
				"expiry %s at %v, want %v", id, got[i].at,
				deadline,
			)
		}

		// No duplicates arrive later.
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Len(t, cb.snapshot(), len(want))
	})
}

// TestSynctestCancelBeforeExpiry verifies a cancel that reaches the actor
// before the deadline suppresses the expiry for good, while an unrelated
// timeout with the same deadline still fires on time.
func TestSynctestCancelBeforeExpiry(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		_, ref := startSynctestActor(t)
		cb := &timedCallbackRef{id: "cancel"}

		start := time.Now()

		ref.Tell(ctx, &ScheduleTimeoutRequest{
			ID:       "cancelled",
			Duration: time.Second,
			Callback: cb,
		})
		ref.Tell(ctx, &ScheduleTimeoutRequest{
			ID:       "control",
			Duration: time.Second,
			Callback: cb,
		})

		// Cancel halfway to the deadline.
		time.Sleep(500 * time.Millisecond)
		ref.Tell(ctx, &CancelTimeoutRequest{ID: "cancelled"})
		synctest.Wait()
		require.Empty(t, cb.snapshot())

		// Cross the shared deadline. Only the control fires.
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()

		got := cb.snapshot()
		require.Len(t, got, 1)
		require.Equal(t, ID("control"), got[0].id)
		require.True(t, start.Add(time.Second).Equal(got[0].at))

		// Nothing more ever arrives for the cancelled ID.
		time.Sleep(time.Hour)
		synctest.Wait()
		require.Len(t, cb.snapshot(), 1)
	})
}

// TestSynctestRecurringTick verifies the fixed-delay ticker against the real
// clock: every tick reports the exact instant its timer fired, and a cancel
// stops the chain.
func TestSynctestRecurringTick(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		_, ref := startSynctestActor(t)
		cb := newMockTickCallback(t, "ticks")

		start := time.Now()

		ref.Tell(ctx, &ScheduleRecurringTickRequest{
			ID:       "tick",
			Interval: 100 * time.Millisecond,
			Callback: cb,
		})

		time.Sleep(350 * time.Millisecond)
		synctest.Wait()

		got := cb.snapshot()
		require.Len(t, got, 3)
		for i, tick := range got {
			want := start.Add(
				time.Duration(i+1) * 100 * time.Millisecond,
			)
			require.Equal(t, ID("tick"), tick.ID)
			require.True(
				t, want.Equal(tick.FiredAt),
				"tick %d fired at %v, want %v", i,
				tick.FiredAt, want,
			)
		}

		ref.Tell(ctx, &CancelTimeoutRequest{ID: "tick"})
		synctest.Wait()

		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, 3, cb.count())
	})
}

// TestSynctestRetryBackoffTiming verifies the retry path against the real
// clock: an expiry refused with a full mailbox is re-offered exactly
// retryBaseDelay later, then twice that, and lands intact once the requester
// drains.
func TestSynctestRetryBackoffTiming(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		behavior, ref := startSynctestActor(t)
		cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

		ref.Tell(ctx, &ScheduleTimeoutRequest{
			ID:       "retried",
			Duration: 50 * time.Millisecond,
			Callback: cb,
		})

		// First attempt at the deadline is refused.
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		msgs, attempts := cb.snapshot()
		require.Empty(t, msgs)
		require.Equal(t, 1, attempts)

		// The second attempt comes exactly one base delay later, not a
		// moment sooner.
		time.Sleep(retryBaseDelay - time.Nanosecond)
		synctest.Wait()
		_, attempts = cb.snapshot()
		require.Equal(t, 1, attempts)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		_, attempts = cb.snapshot()
		require.Equal(t, 2, attempts)

		// The third attempt comes after a doubled delay, and succeeds
		// because the requester has drained by then.
		cb.release()
		time.Sleep(2*retryBaseDelay - time.Nanosecond)
		synctest.Wait()
		msgs, attempts = cb.snapshot()
		require.Empty(t, msgs)
		require.Equal(t, 2, attempts)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		msgs, attempts = cb.snapshot()
		require.Len(t, msgs, 1)
		require.Equal(t, ID("retried"), msgs[0].ID)
		require.Equal(t, 3, attempts)

		// The reminder is discharged. Ask the actor for an ack, which
		// orders this read of its state after every prior message.
		res := ref.Ask(ctx, &CancelTimeoutRequest{ID: "unrelated"}).
			Await(ctx)
		require.True(t, res.IsOk())
		require.Empty(t, behavior.oneshots)
	})
}
