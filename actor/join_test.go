package actor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// joinBehavior signals when a Receive starts, then runs until the actor's
// context is cancelled and takes a while to finish, so a test can tell a
// real join from a signal.
type joinBehavior struct {
	started  chan struct{}
	finished atomic.Bool
}

// Receive implements ActorBehavior.
func (b *joinBehavior) Receive(actorCtx context.Context,
	_ *testMsg) fn.Result[string] {

	close(b.started)
	<-actorCtx.Done()
	time.Sleep(50 * time.Millisecond)
	b.finished.Store(true)

	return fn.Ok("done")
}

// TestStopAndWaitJoinsReceive checks that StopAndWait returns only after the
// in-progress Receive has returned, so the owner can close resources.
func TestStopAndWaitJoinsReceive(t *testing.T) {
	beh := &joinBehavior{started: make(chan struct{})}
	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID: "join", Behavior: beh, MailboxSize: 2,
	})
	require.NoError(t, err)
	a.Start()

	ctx := t.Context()
	a.Ref().Tell(ctx, newTestMsg("work"))
	<-beh.started

	// A Future that was abandoned says nothing about the join; Done must
	// still be open while Receive runs.
	a.Stop()
	select {
	case <-a.Done():
		t.Fatal("Done closed while Receive was still running")
	default:
	}

	require.NoError(t, a.StopAndWait(ctx))
	require.True(t, beh.finished.Load())

	select {
	case <-a.Done():
	default:
		t.Fatal("Done not closed after join")
	}
}

// TestStopAndWaitContextExpiry checks that StopAndWait reports the context's
// error when the actor doesn't stop in time, and that a never-started actor
// joins immediately.
func TestStopAndWaitContextExpiry(t *testing.T) {
	beh := newWedgeBehavior()
	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID: "join-timeout", Behavior: &stuckBehavior{beh: beh},
		MailboxSize: 1,
	})
	require.NoError(t, err)
	a.Start()
	a.Ref().Tell(t.Context(), newTestMsg("x"))
	<-beh.started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, a.StopAndWait(ctx), context.DeadlineExceeded)

	// Let the Receive return so the actor goroutine doesn't leak.
	close(beh.release)
	require.NoError(t, a.StopAndWait(t.Context()))

	idle, err := NewActor(ActorConfig[*testMsg, string]{
		ID: "join-unstarted", Behavior: newEchoBehavior(t, 0),
	})
	require.NoError(t, err)
	require.NoError(t, idle.StopAndWait(t.Context()))
}

// stuckBehavior ignores the actor context, only returning once the wrapped
// wedge is released.
type stuckBehavior struct {
	beh *wedgeBehavior
}

// Receive implements ActorBehavior.
func (s *stuckBehavior) Receive(_ context.Context,
	_ *testMsg) fn.Result[string] {

	select {
	case s.beh.started <- struct{}{}:
	default:
	}
	<-s.beh.release

	return fn.Ok("released")
}

// TestShutdownAndWait checks that the system-level join waits for every
// managed actor's Receive to return.
func TestShutdownAndWait(t *testing.T) {
	system := NewActorSystem()
	key := NewServiceKey[*testMsg, string]("join-system")

	beh := &joinBehavior{started: make(chan struct{})}
	ref, err := RegisterWithSystem(system, "join-sys-1", key, beh)
	require.NoError(t, err)

	ref.Tell(t.Context(), newTestMsg("work"))
	<-beh.started

	require.NoError(t, system.ShutdownAndWait(t.Context()))
	require.True(t, beh.finished.Load())
}
