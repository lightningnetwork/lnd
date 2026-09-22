package actor

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// gatedBehavior blocks inside Receive until its gate is closed, recording each
// message it processes. It lets a test hold the actor mid-message so the
// mailbox fills deterministically.
type gatedBehavior struct {
	gate chan struct{}
	seen chan string
}

// newGatedBehavior returns a gatedBehavior whose gate starts closed or open.
func newGatedBehavior(open bool) *gatedBehavior {
	b := &gatedBehavior{
		gate: make(chan struct{}),
		seen: make(chan string, 16),
	}
	if open {
		close(b.gate)
	}

	return b
}

// Receive records the message, then waits for the gate.
func (b *gatedBehavior) Receive(ctx context.Context,
	msg *testMsg) fn.Result[string] {

	b.seen <- msg.data

	select {
	case <-b.gate:
	case <-ctx.Done():
	}

	return fn.Ok(msg.data)
}

// newGatedActor starts an actor with the given mailbox size around b.
func newGatedActor(t *testing.T, b *gatedBehavior,
	mailboxSize int) *Actor[*testMsg, string] {

	t.Helper()

	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID:          "gated",
		Behavior:    b,
		MailboxSize: mailboxSize,
	})
	require.NoError(t, err)
	a.Start()

	return a
}

// TestTryTellEnqueues asserts that TryTell delivers a message to an actor with
// room in its mailbox.
func TestTryTellEnqueues(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		b := newGatedBehavior(true)
		a := newGatedActor(t, b, 4)
		defer a.Stop()

		ctx := t.Context()
		require.NoError(t, a.Ref().TryTell(ctx, newTestMsg("one")))

		synctest.Wait()
		require.Equal(t, "one", <-b.seen)
	})
}

// TestTryTellMailboxFull asserts that TryTell reports ErrMailboxFull, rather
// than blocking, once the mailbox has no room.
func TestTryTellMailboxFull(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		b := newGatedBehavior(false)
		a := newGatedActor(t, b, 1)
		defer a.Stop()

		ctx := t.Context()
		ref := a.Ref()

		// The first message is taken by the processing goroutine,
		// which then parks on the gate. Wait until it is durably
		// parked so the mailbox is known to be empty again.
		require.NoError(t, ref.TryTell(ctx, newTestMsg("one")))
		synctest.Wait()
		require.Equal(t, "one", <-b.seen)

		// The second message fills the single mailbox slot, and the
		// third has nowhere to go.
		require.NoError(t, ref.TryTell(ctx, newTestMsg("two")))
		require.ErrorIs(
			t, ref.TryTell(ctx, newTestMsg("three")),
			ErrMailboxFull,
		)

		// Releasing the gate drains the queued message, after which
		// TryTell succeeds again.
		close(b.gate)
		synctest.Wait()
		require.Equal(t, "two", <-b.seen)
		require.NoError(t, ref.TryTell(ctx, newTestMsg("four")))
	})
}

// TestTryTellTerminated asserts that TryTell reports ErrActorTerminated for a
// stopped actor.
func TestTryTellTerminated(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		b := newGatedBehavior(true)
		a := newGatedActor(t, b, 4)
		a.Stop()
		synctest.Wait()

		err := a.Ref().TryTell(t.Context(), newTestMsg("late"))
		require.ErrorIs(t, err, ErrActorTerminated)
	})
}

// TestTryTellCancelledContext asserts that TryTell honors an already
// cancelled caller context.
func TestTryTellCancelledContext(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		b := newGatedBehavior(true)
		a := newGatedActor(t, b, 4)
		defer a.Stop()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := a.Ref().TryTell(ctx, newTestMsg("x"))
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestRouterTryTellNoActors asserts that a router with no registered actors
// reports ErrNoActorsAvailable from TryTell.
func TestRouterTryTellNoActors(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		system := NewActorSystem()
		defer func() { require.NoError(t, system.Shutdown()) }()

		key := NewServiceKey[*testMsg, string]("nobody")
		router := NewRouter(
			system.Receptionist(), key,
			NewRoundRobinStrategy[*testMsg, string](),
			nil,
		)

		err := router.TryTell(t.Context(), newTestMsg("x"))
		require.ErrorIs(t, err, ErrNoActorsAvailable)
	})
}
