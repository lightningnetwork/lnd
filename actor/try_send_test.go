package actor

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// wedgeBehavior parks inside Receive until released, which lets a test hold an
// actor's only processing goroutine hostage and fill its mailbox.
type wedgeBehavior struct {
	started chan struct{}
	release chan struct{}
}

// newWedgeBehavior creates a behavior that signals on started when its first
// message lands and stays inside Receive until release is closed.
func newWedgeBehavior() *wedgeBehavior {
	return &wedgeBehavior{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

// Receive signals that processing began and blocks until the test releases it
// or the actor shuts down.
func (b *wedgeBehavior) Receive(actorCtx context.Context,
	_ *testMsg) fn.Result[string] {

	select {
	case b.started <- struct{}{}:
	default:
	}

	select {
	case <-b.release:
	case <-actorCtx.Done():
	}

	return fn.Ok("wedged")
}

// newWedgedActor starts an actor with a one slot mailbox whose behavior is
// parked inside Receive, then fills the mailbox so the next send has nowhere
// to go. The caller owns stopping the actor.
func newWedgedActor(t *testing.T) (*Actor[*testMsg, string], *wedgeBehavior) {
	t.Helper()

	beh := newWedgeBehavior()
	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID:          "wedged-" + t.Name(),
		Behavior:    beh,
		MailboxSize: 1,
	})
	require.NoError(t, err)
	a.Start()

	// The first message parks the process loop, which frees the single
	// buffer slot again. The second then fills it.
	ctx := t.Context()
	require.NoError(t, a.Ref().TryTell(ctx, newTestMsg("a")))
	<-beh.started
	require.NoError(t, a.Ref().TryTell(ctx, newTestMsg("b")))

	return a, beh
}

// chanMailbox returns the actor's default ChannelMailbox.
func chanMailbox(t *testing.T,
	a *Actor[*testMsg, string]) *ChannelMailbox[*testMsg, string] {

	t.Helper()

	box, ok := a.mailbox.(*ChannelMailbox[*testMsg, string])
	require.True(t, ok)

	return box
}

// TestTryTellTryAskNeverBlockFromPlainGoroutine asserts the core guarantee of
// the non-blocking primitives: called from a goroutine that isn't an actor,
// against a full mailbox, both return ErrMailboxFull at once. The synctest
// bubble makes "at once" exact, since the fake clock can't advance while the
// call is running, and a call that parked would never signal done.
func TestTryTellTryAskNeverBlockFromPlainGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _ := newWedgedActor(t)
		defer a.Stop()

		ref := a.Ref()

		var (
			tellErr error
			askRes  fn.Result[string]
			done    = make(chan struct{})
			start   = time.Now()
		)
		go func() {
			defer close(done)

			// A context with no deadline, so only a non-blocking
			// implementation can return.
			ctx := t.Context()

			tellErr = ref.TryTell(ctx, newTestMsg("c"))

			// The future is already complete, so awaiting it must
			// not park either.
			fut := ref.TryAsk(ctx, newTestMsg("d"))
			askRes = fut.Await(ctx)
		}()

		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("TryTell/TryAsk blocked on a full mailbox")
		}

		require.Equal(t, start, time.Now())
		require.ErrorIs(t, tellErr, ErrMailboxFull)

		_, err := askRes.Unpack()
		require.ErrorIs(t, err, ErrMailboxFull)
	})
}

// TestTryTellTryAskSuccess checks that with room available TryTell enqueues
// and TryAsk delivers the actor's reply.
func TestTryTellTryAskSuccess(t *testing.T) {
	h := newActorTestHarness(t)
	a := h.newActor("try-ok", newEchoBehavior(t, 0), 4)
	ref := a.Ref()
	ctx := t.Context()

	require.NoError(t, ref.TryTell(ctx, newTestMsg("tell")))

	res := ref.TryAsk(ctx, newTestMsg("ask")).Await(ctx)
	val, err := res.Unpack()
	require.NoError(t, err)
	require.Equal(t, "echo: ask", val)
}

// TestTryTellTryAskTerminated checks that a stopped actor is reported as
// terminated rather than full, and that a TryTell is routed to the DLO.
func TestTryTellTryAskTerminated(t *testing.T) {
	h := newActorTestHarness(t)
	a := h.newActor("try-stopped", newEchoBehavior(t, 0), 1)
	ref := a.Ref()
	ctx := t.Context()

	a.Stop()

	msg := newTestMsg("late")
	require.ErrorIs(t, ref.TryTell(ctx, msg), ErrActorTerminated)
	h.assertDLOMessage(msg)

	_, err := ref.TryAsk(ctx, newTestMsg("late")).Await(ctx).Unpack()
	require.ErrorIs(t, err, ErrActorTerminated)
}

// TestTryTellTryAskCancelledContext checks that a caller that has already
// given up never has its message enqueued.
func TestTryTellTryAskCancelledContext(t *testing.T) {
	h := newActorTestHarness(t)
	a := h.newActor("try-cancel", newEchoBehavior(t, 0), 4)
	ref := a.Ref()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, ref.TryTell(ctx, newTestMsg("x")), context.Canceled)

	_, err := ref.TryAsk(ctx, newTestMsg("x")).Await(
		t.Context(),
	).Unpack()
	require.ErrorIs(t, err, context.Canceled)
}

// TestTryTellTryAskClosedMailbox checks that a closed mailbox on a live actor
// is reported as ErrMailboxClosed.
func TestTryTellTryAskClosedMailbox(t *testing.T) {
	h := newActorTestHarness(t)
	a := h.newActor("try-closed", newEchoBehavior(t, 0), 1)
	ref := a.Ref()
	ctx := t.Context()

	a.mailbox.Close()

	require.ErrorIs(t, ref.TryTell(ctx, newTestMsg("x")), ErrMailboxClosed)

	_, err := ref.TryAsk(ctx, newTestMsg("x")).Await(ctx).Unpack()
	require.ErrorIs(t, err, ErrMailboxClosed)
}

// TestRouterTryTellTryAsk checks that the router forwards the non-blocking
// sends, and surfaces ErrNoActorsAvailable when it has no target.
func TestRouterTryTellTryAsk(t *testing.T) {
	system := NewActorSystem()
	t.Cleanup(func() { _ = system.Shutdown() })

	key := NewServiceKey[*testMsg, string]("try-router")
	ctx := t.Context()

	router := NewRouter(
		system.Receptionist(), key,
		NewRoundRobinStrategy[*testMsg, string](), nil,
	)

	require.ErrorIs(t, router.TryTell(ctx, newTestMsg("x")),
		ErrNoActorsAvailable)
	_, err := router.TryAsk(ctx, newTestMsg("x")).Await(ctx).Unpack()
	require.ErrorIs(t, err, ErrNoActorsAvailable)

	_, err = RegisterWithSystem(
		system, "try-router-1", key, newEchoBehavior(t, 0),
	)
	require.NoError(t, err)

	require.NoError(t, router.TryTell(ctx, newTestMsg("x")))
	val, err := router.TryAsk(ctx, newTestMsg("y")).Await(ctx).Unpack()
	require.NoError(t, err)
	require.Equal(t, "echo: y", val)
}

// TestAdmissionOutcomesFromPlainGoroutine checks that from a goroutine that
// isn't an actor, a rejected TryTell or TryAsk enqueues nothing and reports
// one of three distinct outcomes: mailbox full, actor terminated, or the
// caller's context already cancelled.
func TestAdmissionOutcomesFromPlainGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		full, beh := newWedgedActor(t)
		stopped, err := NewActor(ActorConfig[*testMsg, string]{
			ID: "admission-stopped", Behavior: newEchoBehavior(
				t, 0,
			),
			MailboxSize: 1,
		})
		require.NoError(t, err)
		stopped.Start()
		stopped.Stop()
		synctest.Wait()

		ctx := t.Context()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		type outcome struct {
			tell, ask error
		}
		results := make(map[string]outcome)

		done := make(chan struct{})
		go func() {
			defer close(done)

			try := func(name string,
				ref ActorRef[*testMsg, string],
				c context.Context) {

				_, askErr := ref.TryAsk(
					c, newTestMsg(name),
				).Await(ctx).Unpack()

				results[name] = outcome{
					tell: ref.TryTell(c, newTestMsg(name)),
					ask:  askErr,
				}
			}

			try("full", full.Ref(), ctx)
			try("stopped", stopped.Ref(), ctx)
			try("cancelled", full.Ref(), cancelled)
		}()
		synctest.Wait()
		<-done

		require.ErrorIs(t, results["full"].tell, ErrMailboxFull)
		require.ErrorIs(t, results["full"].ask, ErrMailboxFull)
		require.ErrorIs(t, results["stopped"].tell, ErrActorTerminated)
		require.ErrorIs(t, results["stopped"].ask, ErrActorTerminated)
		require.ErrorIs(t, results["cancelled"].tell, context.Canceled)
		require.ErrorIs(t, results["cancelled"].ask, context.Canceled)

		// Nothing was enqueued by any rejected send: release the
		// wedge, and only the two accepted messages are processed.
		close(beh.release)
		synctest.Wait()
		require.Zero(t, len(chanMailbox(t, full).ch))

		full.Stop()
	})
}
