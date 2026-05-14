package actor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// singletonTestHarness bundles an ActorSystem and a SingletonRef-friendly
// receptionist for singleton tests.
type singletonTestHarness struct {
	*actorTestHarness
	as           *ActorSystem
	receptionist *Receptionist
}

// newSingletonTestHarness creates a new harness with its own ActorSystem.
func newSingletonTestHarness(t *testing.T) *singletonTestHarness {
	t.Helper()
	system := NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown())
	})

	return &singletonTestHarness{
		actorTestHarness: newActorTestHarness(t),
		as:               system,
		receptionist:     system.Receptionist(),
	}
}

// TestSingletonRefNotRegistered verifies that when no actor is registered,
// Ask returns ErrNoActorsAvailable and Tell forwards to the DLO.
func TestSingletonRefNotRegistered(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-not-registered")
	ref := NewSingletonRef(h.receptionist, key, h.dlo.Ref())

	// Ask should fail immediately with ErrNoActorsAvailable.
	askMsg := newTestMsg("ask-no-actor")
	result := ref.Ask(context.Background(), askMsg).Await(
		context.Background(),
	)
	require.True(t, result.IsErr())
	require.ErrorIs(t, result.Err(), ErrNoActorsAvailable)

	// Tell should forward the message to the DLO.
	tellMsg := newTestMsg("tell-no-actor")
	ref.Tell(context.Background(), tellMsg)
	h.assertDLOMessage(tellMsg)
}

// TestSingletonRefTellAskBasic verifies that Tell and Ask correctly forward
// to the one registered actor.
func TestSingletonRefTellAskBasic(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-basic")
	_, err := key.SpawnSingleton(h.as, "singleton-basic-actor",
		newEchoBehavior(t, 0))
	require.NoError(t, err)

	ref := key.Singleton(h.as)
	require.Equal(t, "singleton(singleton-basic)", ref.ID())

	// Ask should echo through the actor.
	result := ref.Ask(
		context.Background(), newTestMsg("hello"),
	).Await(context.Background())
	require.False(t, result.IsErr())
	result.WhenOk(func(s string) {
		require.Equal(t, "echo: hello", s)
	})

	// Tell should reach the actor; we observe via reply channel.
	replyChan := make(chan string, 1)
	ref.Tell(
		context.Background(),
		newTestMsgWithReply("tell-data", replyChan),
	)
	received, err := fn.RecvOrTimeout(replyChan, time.Second)
	require.NoError(t, err)
	require.Equal(t, "tell-data", received)
}

// TestSingletonRefID verifies the generated ID format.
func TestSingletonRefID(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("my-service")
	ref := NewSingletonRef(h.receptionist, key, h.dlo.Ref())
	require.Equal(t, "singleton(my-service)", ref.ID())
}

// TestSpawnSingletonReplacesExisting verifies that calling SpawnSingleton a
// second time for the same key stops the previous actor and replaces it with
// a fresh one.
func TestSpawnSingletonReplacesExisting(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-replace")

	// Spawn the first actor.
	beh1 := newCountingEchoBehavior(t, "actor1")
	ref1, err := key.SpawnSingleton(h.as, "singleton-replace-1", beh1)
	require.NoError(t, err)

	// Verify it's reachable through the singleton lookup.
	lookup := key.Singleton(h.as)
	r := lookup.Ask(
		context.Background(), newTestMsg("m1"),
	).Await(context.Background())
	require.False(t, r.IsErr())
	r.WhenOk(func(s string) {
		require.Equal(t, "actor1:echo: m1", s)
	})
	require.EqualValues(t, 1, atomic.LoadInt64(&beh1.processedMsgs))

	// Spawn a second actor under the same key. This must stop the first
	// and leave exactly one registered.
	beh2 := newCountingEchoBehavior(t, "actor2")
	ref2, err := key.SpawnSingleton(h.as, "singleton-replace-2", beh2)
	require.NoError(t, err)

	// Only one actor should be registered under the key.
	refs := FindInReceptionist(h.receptionist, key)
	require.Len(t, refs, 1)
	require.Equal(t, ref2, refs[0])

	// The first actor should be stopped.
	staleRes := ref1.Ask(
		context.Background(), newTestMsg("m-to-stale"),
	).Await(context.Background())
	require.True(t, staleRes.IsErr())
	require.ErrorIs(t, staleRes.Err(), ErrActorTerminated)

	// The singleton lookup should now hit the second actor.
	r = lookup.Ask(
		context.Background(), newTestMsg("m2"),
	).Await(context.Background())
	require.False(t, r.IsErr())
	r.WhenOk(func(s string) {
		require.Equal(t, "actor2:echo: m2", s)
	})
	require.EqualValues(t, 1, atomic.LoadInt64(&beh2.processedMsgs))
	// beh1 must not have received the second message.
	require.EqualValues(t, 1, atomic.LoadInt64(&beh1.processedMsgs))
}

// TestSingletonRefDynamicRegistration verifies that a SingletonRef picks up
// an actor that is registered after the ref was created, and that it returns
// to the no-actor state after the actor is unregistered.
func TestSingletonRefDynamicRegistration(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-dynamic")
	ref := NewSingletonRef(h.receptionist, key, h.dlo.Ref())

	// No actor registered yet — Ask should return ErrNoActorsAvailable.
	res := ref.Ask(
		context.Background(), newTestMsg("before"),
	).Await(context.Background())
	require.ErrorIs(t, res.Err(), ErrNoActorsAvailable)

	// Spawn the actor.
	_, err := key.SpawnSingleton(
		h.as, "singleton-dynamic-actor", newEchoBehavior(t, 0),
	)
	require.NoError(t, err)

	// Now Ask should succeed.
	res = ref.Ask(
		context.Background(), newTestMsg("after-spawn"),
	).Await(context.Background())
	require.False(t, res.IsErr())
	res.WhenOk(func(s string) {
		require.Equal(t, "echo: after-spawn", s)
	})

	// Unregister the actor; Ask should fail again.
	require.Equal(t, 1, key.UnregisterAll(h.as))
	res = ref.Ask(
		context.Background(), newTestMsg("after-unreg"),
	).Await(context.Background())
	require.ErrorIs(t, res.Err(), ErrNoActorsAvailable)
}

// TestSingletonRefMultipleRegistrations verifies that if two actors somehow
// end up registered under the same singleton key, the ref still works by
// forwarding to the first registered actor (invariant violation tolerance).
func TestSingletonRefMultipleRegistrations(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-multi")

	// Register two actors directly, bypassing SpawnSingleton, to simulate
	// a registration race.
	beh1 := newCountingEchoBehavior(t, "actor1")
	beh2 := newCountingEchoBehavior(t, "actor2")
	_, err := RegisterWithSystem(h.as, "singleton-multi-1", key, beh1)
	require.NoError(t, err)
	_, err = RegisterWithSystem(h.as, "singleton-multi-2", key, beh2)
	require.NoError(t, err)

	require.Len(t, FindInReceptionist(h.receptionist, key), 2)

	// Ask should still succeed; our tolerant implementation routes to the
	// first registered actor.
	ref := NewSingletonRef(h.receptionist, key, h.dlo.Ref())
	res := ref.Ask(
		context.Background(), newTestMsg("hello"),
	).Await(context.Background())
	require.False(t, res.IsErr())
	res.WhenOk(func(s string) {
		require.Equal(t, "actor1:echo: hello", s)
	})

	// Only the first actor should have processed the message.
	require.EqualValues(t, 1, atomic.LoadInt64(&beh1.processedMsgs))
	require.EqualValues(t, 0, atomic.LoadInt64(&beh2.processedMsgs))
}

// TestSingletonRefNoDLOConfigured verifies that Tell with no actor and no
// DLO configured does not panic and drops the message cleanly.
func TestSingletonRefNoDLOConfigured(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-no-dlo")
	ref := NewSingletonRef(h.receptionist, key, nil)

	require.NotPanics(t, func() {
		ref.Tell(context.Background(), newTestMsg("lost-message"))
	})

	// Nothing should go to the harness's DLO either, since it isn't
	// wired up to the ref.
	h.assertNoDLOMessages()
}

// TestSingletonRefContextCancellation verifies that a cancelled context
// propagates to Ask.
func TestSingletonRefContextCancellation(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-cancel")

	// The behavior's processing delay is irrelevant here: Ask short-
	// circuits on an already-cancelled context before reaching the
	// mailbox, so the behavior's Receive is never invoked.
	_, err := key.SpawnSingleton(
		h.as, "singleton-cancel-actor", newEchoBehavior(t, 0),
	)
	require.NoError(t, err)

	ref := key.Singleton(h.as)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := ref.Ask(ctx, newTestMsg("cancel-me")).Await(
		context.Background(),
	)
	require.True(t, result.IsErr())
	require.ErrorIs(t, result.Err(), context.Canceled)
}

// TestServiceKeySingletonHelper verifies that ServiceKey.Singleton returns a
// ref pointing to the same receptionist and key as the ActorSystem.
func TestServiceKeySingletonHelper(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-helper")
	ref := key.Singleton(h.as)

	require.Equal(t, "singleton(singleton-helper)", ref.ID())
	require.Equal(t, h.as.Receptionist(), ref.receptionist)
	require.Equal(t, h.as.DeadLetters(), ref.dlo)
}

// TestSingletonRefImplementsActorRef verifies at runtime that SingletonRef
// can be used as an ActorRef.
func TestSingletonRefImplementsActorRef(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-iface")
	_, err := key.SpawnSingleton(
		h.as, "singleton-iface-actor", newEchoBehavior(t, 0),
	)
	require.NoError(t, err)

	var ref ActorRef[*testMsg, string] = key.Singleton(h.as)
	res := ref.Ask(
		context.Background(), newTestMsg("via-interface"),
	).Await(context.Background())
	require.False(t, res.IsErr())
	res.WhenOk(func(s string) {
		require.Equal(t, "echo: via-interface", s)
	})
}

// TestSpawnSingletonIdempotentReplace verifies that calling SpawnSingleton
// repeatedly for the same key results in exactly one registered actor each
// time, and that the fresh actor (not a stale predecessor) is the one that
// receives messages. Each behavior closes over its own actor ID and echoes
// it back, so the assertion directly compares the reply to the just-spawned
// actor's ID.
func TestSpawnSingletonIdempotentReplace(t *testing.T) {
	t.Parallel()
	h := newSingletonTestHarness(t)

	key := NewServiceKey[*testMsg, string]("singleton-idem")

	const replacements = 5
	for i := 0; i < replacements; i++ {
		actorID := fmt.Sprintf("idem-actor-%d", i)

		// The closure captures actorID so that whichever actor
		// responds reveals its identity in the reply.
		beh := NewFunctionBehavior(
			func(_ context.Context,
				_ *testMsg) fn.Result[string] {

				return fn.Ok(actorID)
			},
		)

		_, err := key.SpawnSingleton(h.as, actorID, beh)
		require.NoError(t, err)

		// After each spawn, exactly one actor should be registered.
		require.Len(t, FindInReceptionist(h.receptionist, key), 1)

		// The reply must identify the just-spawned actor. If a stale
		// predecessor answered, we'd see its earlier actorID here.
		ref := key.Singleton(h.as)
		res := ref.Ask(
			context.Background(), newTestMsg("who-replies"),
		).Await(context.Background())
		require.False(t, res.IsErr())
		res.WhenOk(func(replier string) {
			require.Equal(t, actorID, replier)
		})
	}
}
