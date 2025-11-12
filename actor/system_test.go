package actor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestActorSystemNewActorSystem verifies the basic initialization of an
// ActorSystem, including its default DLO.
func TestActorSystemNewActorSystem(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()
	require.NotNil(t, as, "newActorSystem should not return nil")
	require.NotNil(t, as.Receptionist(), "receptionist should not be nil")
	require.NotNil(t, as.DeadLetters(), "deadLetters should not be nil")
	require.Equal(t, "dead-letters", as.DeadLetters().ID(), "dLO ID mismatch")

	// Test the DLO's behavior (it should return an error for Ask).
	testDLOMsg := newTestMsg("to-dlo")
	future := as.DeadLetters().Ask(context.Background(), testDLOMsg)
	result := future.Await(context.Background())

	// We should get back an error for asks.
	require.True(
		t, result.IsErr(), "system DLO should return an error on Ask",
	)
	expectedErrStr := "message undeliverable: " + testDLOMsg.MessageType()
	require.EqualError(
		t, result.Err(), expectedErrStr, "dLO error message mismatch",
	)

	// Shutdown the system to clean up resources.
	err := as.Shutdown()
	require.NoError(t, err, "actorSystem shutdown failed")
}

// TestActorSystemRegisterWithSystem verifies actor registration, lifecycle
// management within the system.
func TestActorSystemRegisterWithSystem(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()
	defer func() {
		err := as.Shutdown()
		require.NoError(t, err)
	}()

	actorID := "test-actor-sys-reg"
	serviceKey := NewServiceKey[*testMsg, string]("test-service")

	// Using echoBehavior from actor_test.go (implicitly available)
	beh := newEchoBehavior(t, 0)

	// We'll start off by registering the actor.
	actorRef := RegisterWithSystem(as, actorID, serviceKey, beh)
	require.NotNil(t, actorRef, "registerWithSystem should return a valid ActorRef")
	require.Equal(t, actorID, actorRef.ID(), "registered actor ID mismatch")

	// The actor should be found in the receptionist.
	foundActors := FindInReceptionist(as.Receptionist(), serviceKey)
	require.Len(t, foundActors, 1, "actor not found in receptionist")
	require.Equal(t, actorRef, foundActors[0], "incorrect actor in receptionist")

	// Next, we'll send out a simple tell, using our reply channel to make
	// sure it's actually processed.
	msgData := "hello-system-actor"
	replyChan := make(chan string, 1)
	actorRef.Tell(context.Background(), newTestMsgWithReply(msgData, replyChan))

	received, err := fn.RecvOrTimeout(replyChan, 100*time.Millisecond)
	if err != nil {
		t.Fatal("timed out waiting for actor to process message")
	}
	require.Equal(t, msgData, received, "actor did not process message")

	// Stop the actor through the system.
	stopped := as.StopAndRemoveActor(actorID)
	require.True(t, stopped, "StopAndRemoveActor failed")

	// Wait for actor to fully stop.
	time.Sleep(50 * time.Millisecond)

	// Send a message to the now-stopped actor's ref. This should go to the
	// system's DLO.
	afterStopMsg := newTestMsg("after-stop-to-dlo")
	require.NotPanics(t, func() {
		actorRef.Tell(context.Background(), afterStopMsg)
	}, "tell to stopped actor should not panic")
}

// TestActorSystemShutdown verifies that all actors are stopped and the system
// context is cancelled upon shutdown.
func TestActorSystemShutdown(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()

	// We'll start by making 3 new actors, each with a unique ID.
	numActors := 3
	actorRefs := make([]ActorRef[*testMsg, string], numActors)
	for i := 0; i < numActors; i++ {
		actorID := fmt.Sprintf("shutdown-test-actor-%d", i)
		key := NewServiceKey[*testMsg, string](
			fmt.Sprintf("service-%d", i),
		)
		beh := newEchoBehavior(t, 0)
		actorRefs[i] = RegisterWithSystem(as, actorID, key, beh)
	}

	// We'll now send a message to each actor to ensure that they're
	// running.
	for i, ref := range actorRefs {
		future := ref.Ask(
			context.Background(),
			newTestMsg(fmt.Sprintf("ping-%d", i)),
		)
		ctxAwait, cancelAwait := context.WithTimeout(
			context.Background(), time.Second,
		)
		res := future.Await(ctxAwait)
		cancelAwait()
		require.False(
			t, res.IsErr(),
			"actor %d failed to respond before shutdown: %v",
			i, res.Err(),
		)
	}

	// Next, trigger a shutdown, and assert that the done channel gets
	// closed.
	err := as.Shutdown()
	require.NoError(t, err, "actorSystem shutdown failed")

	// Check if the system context is done using RecvOrTimeout with a zero
	// timeout for a non-blocking check.
	_, err = fn.RecvOrTimeout(as.ctx.Done(), time.Millisecond*100)
	if err != nil {
		t.Fatal("actorSystem context not cancelled after shutdown")
	}

	// We'll now try to send a message to each of the actors, this should
	// result in an error.
	for i, ref := range actorRefs {
		future := ref.Ask(
			context.Background(),
			newTestMsg(fmt.Sprintf("ping-after-shutdown-%d", i)),
		)
		res := future.Await(context.Background())
		require.True(
			t, res.IsErr(),
			"actor %d Ask should fail after shutdown", i,
		)
		require.ErrorIs(t, res.Err(), ErrActorTerminated)
	}

	as.mu.RLock()
	require.Nil(t, as.actors, "actors map should be nil after shutdown")
	as.mu.RUnlock()

	// Once shutdown, we shouldn't be able to send to the DLO either.
	dloRef := as.DeadLetters()
	futureDLO := dloRef.Ask(
		context.Background(), newTestMsg("ping-dlo-after-shutdown"),
	)
	resDLO := futureDLO.Await(context.Background())
	require.True(
		t, resDLO.IsErr(), "DLO Ask should fail after system shutdown",
	)
	require.ErrorIs(
		t, resDLO.Err(), ErrActorTerminated,
	)
}

// TestActorSystemStopAndRemoveActor verifies specific actor stopping and
// removal.
func TestActorSystemStopAndRemoveActor(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()
	defer func() {
		err := as.Shutdown()
		require.NoError(t, err)
	}()

	// Make some actor IDs, then unique service keys, then use that to
	// register two actors.
	actor1ID := "actor-to-stop"
	actor2ID := "actor-to-keep"
	key1 := NewServiceKey[*testMsg, string]("service1")
	key2 := NewServiceKey[*testMsg, string]("service2")
	beh := newEchoBehavior(t, 0)

	ref1 := RegisterWithSystem(as, actor1ID, key1, beh)
	ref2 := RegisterWithSystem(as, actor2ID, key2, beh)

	// If we remove one actor, then try to send to it, we should get an
	// error.
	stopped := as.StopAndRemoveActor(actor1ID)
	require.True(t, stopped, "failed to stop and remove actor1")

	future1 := ref1.Ask(context.Background(), newTestMsg("ping-actor1"))
	res1 := future1.Await(context.Background())
	require.True(t, res1.IsErr(), "actor1 should be stopped")
	require.ErrorIs(t, res1.Err(), ErrActorTerminated)

	as.mu.RLock()
	_, exists := as.actors[actor1ID]
	as.mu.RUnlock()

	// The actor should no longer be found.
	require.False(t, exists, "actor1 still in system's actor map")

	// Make sure that we can still send messages to the existing actor.
	future2 := ref2.Ask(
		context.Background(), newTestMsg("ping-actor2"),
	)

	ctxAwait2, cancelAwait2 := context.WithTimeout(
		context.Background(), time.Second,
	)
	res2 := future2.Await(ctxAwait2)
	cancelAwait2()

	require.False(
		t, res2.IsErr(), "actor2 should still be running: %v",
		res2.Err(),
	)
	res2.WhenOk(func(s string) {
		require.Equal(t, "echo: ping-actor2", s)
	})

	stoppedNonExistent := as.StopAndRemoveActor("non-existent-actor")
	require.False(
		t, stoppedNonExistent, "stopping non-existent actor should "+
			"return false",
	)
}

// TestReceptionist covers basic registration, finding, and unregistration.
func TestReceptionist(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()
	defer func() {
		err := as.Shutdown()
		require.NoError(t, err)
	}()
	receptionist := as.Receptionist()

	key1 := NewServiceKey[*testMsg, string]("key1")
	key2 := NewServiceKey[*testMsg, string]("key2")
	key1Again := NewServiceKey[*testMsg, string]("key1")

	// Register 3 actor instance using the service keys we created above.
	beh := newEchoBehavior(t, 0)
	actor1Ref := RegisterWithSystem(as, "actor1-rec", key1, beh)
	actor2Ref := RegisterWithSystem(as, "actor2-rec", key1, beh)
	actor3Ref := RegisterWithSystem(as, "actor3-rec", key2, beh)

	// We should be able to find the actors we registered.
	foundForKey1 := FindInReceptionist(receptionist, key1)
	require.Len(t, foundForKey1, 2, "should find 2 actors for key1")
	require.Contains(t, foundForKey1, actor1Ref)
	require.Contains(t, foundForKey1, actor2Ref)

	foundForKey1Again := FindInReceptionist(receptionist, key1Again)
	require.ElementsMatch(t, foundForKey1, foundForKey1Again)

	// Same goes for the second key we added.
	foundForKey2 := FindInReceptionist(receptionist, key2)
	require.Len(t, foundForKey2, 1, "should find 1 actor for key2")
	require.Equal(t, actor3Ref, foundForKey2[0])

	// We shouldn't be able to find a key we didn't add.
	nonExistentKey := NewServiceKey[*testMsg, string]("non-existent")
	foundForNonExistent := FindInReceptionist(receptionist, nonExistentKey)
	require.Empty(t, foundForNonExistent)

	// We should be able to unregister the actors we added.
	unregistered := UnregisterFromReceptionist(
		receptionist, key1, actor1Ref,
	)
	require.True(t, unregistered, "failed to unregister actor1Ref")

	foundForKey1AfterUnreg := FindInReceptionist(receptionist, key1)
	require.Len(t, foundForKey1AfterUnreg, 1)
	require.Equal(t, actor2Ref, foundForKey1AfterUnreg[0])

	// If we try to unregister the same actor again, it should fail.
	unregisteredAgain := UnregisterFromReceptionist(receptionist, key1, actor1Ref)
	require.False(t, unregisteredAgain)

	unregisteredLast := UnregisterFromReceptionist(receptionist, key1, actor2Ref)
	require.True(t, unregisteredLast)
	foundForKey1AfterAllUnreg := FindInReceptionist(receptionist, key1)
	require.Empty(t, foundForKey1AfterAllUnreg)

	receptionist.mu.RLock()
	_, exists := receptionist.registrations[key1.name]
	receptionist.mu.RUnlock()
	require.False(t, exists, "key1 should be removed from registrations map")

	// Finally, if we use the wrong key, or one that doesn't exist, that
	// should also fail.
	unregisteredWrongKey := UnregisterFromReceptionist(receptionist, key1, actor3Ref)
	require.False(t, unregisteredWrongKey)
	unregisteredNonExistentKey := UnregisterFromReceptionist(receptionist, nonExistentKey, actor1Ref)
	require.False(t, unregisteredNonExistentKey)
}

// TestServiceKeyMethods tests Spawn and Unregister methods on ServiceKey.
func TestServiceKeyMethods(t *testing.T) {
	t.Parallel()

	as := NewActorSystem()
	defer func() {
		err := as.Shutdown()
		require.NoError(t, err)
	}()

	key := NewServiceKey[*testMsg, string]("sk-service")
	beh := newEchoBehavior(t, 0)

	// Attempt to spawn a new actor using the service key and desired
	// behavior.
	actorRef := key.Spawn(as, "actor-sk-spawn", beh)
	require.NotNil(t, actorRef)
	require.Equal(t, "actor-sk-spawn", actorRef.ID())

	// We should be able to find the actor in the receptionist.
	found := FindInReceptionist(as.Receptionist(), key)
	require.Len(t, found, 1)
	require.Equal(t, actorRef, found[0])

	as.mu.RLock()
	_, sysExists := as.actors[actorRef.ID()]
	as.mu.RUnlock()
	require.True(t, sysExists)

	// Next, try to unregister the actor using the service key.
	success := key.Unregister(as, actorRef)
	require.True(t, success, "serviceKey.Unregister failed")

	// The actor should no longer be found in the receptionist.
	foundAfter := FindInReceptionist(as.Receptionist(), key)
	require.Empty(t, foundAfter)

	as.mu.RLock()
	_, sysExistsAfter := as.actors[actorRef.ID()]
	as.mu.RUnlock()
	require.False(t, sysExistsAfter)

	// If we try to send a message to the actor after unregistering it, then
	// we should get an error.
	future := actorRef.Ask(context.Background(), newTestMsg("ping"))
	res := future.Await(context.Background())
	require.True(t, res.IsErr() && errors.Is(res.Err(), ErrActorTerminated))

	successAgain := key.Unregister(as, actorRef)
	require.False(t, successAgain)

	otherSys := NewActorSystem() // Create a different actor system
	defer func() {
		err := otherSys.Shutdown()
		require.NoError(t, err)
	}()

	// Create a dummy actor in otherSys of the correct generic type for the
	// key. This actor won't be found in 'as', so Unregister should fail.
	dummyBehOther := newEchoBehavior(t, 0)
	dummyKeyOther := NewServiceKey[*testMsg, string]("dummy-other")
	dummyActorRefOtherSys := RegisterWithSystem(
		otherSys, "dummy-other-actor", dummyKeyOther, dummyBehOther,
	)

	successNonMember := key.Unregister(as, dummyActorRefOtherSys)
	require.False(t, successNonMember)
}

// TestServiceKeyUnregisterAll tests the UnregisterAll method on ServiceKey.
// It covers scenarios including basic unregistration of multiple actors,
// attempting to unregister with no actors present, unregistering actors for
// one key while leaving others intact, and the idempotency of the operation.
func TestServiceKeyUnregisterAll(t *testing.T) {
	t.Parallel()

	// Common setup for all sub-tests.
	as := NewActorSystem()
	defer func() {
		err := as.Shutdown()
		require.NoError(t, err, "ActorSystem shutdown failed.")
	}()

	// Common behavior for test actors used across sub-tests.
	beh := newEchoBehavior(t, 0)

	t.Run("unregister all multiple actors", func(st *testing.T) {
		key1 := NewServiceKey[*testMsg, string]("sk-ua-key1")
		actor1Key1 := key1.Spawn(as, "actor1-k1-ua", beh)
		actor2Key1 := key1.Spawn(as, "actor2-k1-ua", beh)

		// Verify they are registered in the receptionist.
		foundActorsForKey1 := FindInReceptionist(
			as.Receptionist(), key1,
		)
		require.Len(
			st, foundActorsForKey1, 2,
			"actors for key1 not in receptionist initially.",
		)

		// Verify they are in the system's actor map.
		as.mu.RLock()
		_, actor1Key1Exists := as.actors[actor1Key1.ID()]
		_, actor2Key1Exists := as.actors[actor2Key1.ID()]
		as.mu.RUnlock()
		require.True(
			st, actor1Key1Exists,
			"actor1 for key1 not in system actors map initially.",
		)
		require.True(
			st, actor2Key1Exists,
			"actor2 for key1 not in system actors map initially.",
		)

		// Unregister all for key1.
		stoppedCountKey1 := key1.UnregisterAll(as)
		require.Equal(
			st, 2, stoppedCountKey1,
			"UnregisterAll for key1 returned incorrect count.",
		)

		// Verify they are unregistered from the receptionist.
		foundActorsForKey1After := FindInReceptionist(
			as.Receptionist(), key1,
		)
		require.Empty(
			st, foundActorsForKey1After,
			"actors for key1 still in receptionist after "+
				"UnregisterAll.",
		)

		// Verify they are removed from system actors map.
		as.mu.RLock()
		_, actor1Key1ExistsAfter := as.actors[actor1Key1.ID()]
		_, actor2Key1ExistsAfter := as.actors[actor2Key1.ID()]
		as.mu.RUnlock()
		require.False(
			st, actor1Key1ExistsAfter,
			"Actor1 for key1 still in system actors "+
				"map after UnregisterAll.",
		)
		require.False(
			st, actor2Key1ExistsAfter,
			"Actor2 for key1 still in system actors "+
				"map after UnregisterAll.",
		)

		// Verify actors are stopped.
		resultActor1Key1 := actor1Key1.Ask(
			context.Background(), newTestMsg("ping-k1-a1"),
		).Await(context.Background())
		require.True(
			st, resultActor1Key1.IsErr(),
			"Actor1 key1 Ask should fail after UnregisterAll.",
		)
		require.ErrorIs(
			st, resultActor1Key1.Err(), ErrActorTerminated,
			"Actor1 key1 not terminated with correct error.",
		)

		resultActor2Key1 := actor2Key1.Ask(
			context.Background(), newTestMsg("ping-k1-a2"),
		).Await(context.Background())
		require.True(
			st, resultActor2Key1.IsErr(),
			"Actor2 key1 Ask should fail after UnregisterAll.",
		)
		require.ErrorIs(
			st, resultActor2Key1.Err(), ErrActorTerminated,
			"Actor2 key1 not terminated with correct error.",
		)
	})

	t.Run("unregister all with no actors for the key", func(st *testing.T) {
		keyEmpty := NewServiceKey[*testMsg, string]("sk-ua-key-empty")
		stoppedCountEmptyKey := keyEmpty.UnregisterAll(as)
		require.Equal(
			st, 0, stoppedCountEmptyKey,
			"UnregisterAll for empty key returned non-zero count.",
		)

		foundActorsForKeyEmpty := FindInReceptionist(
			as.Receptionist(), keyEmpty,
		)
		require.Empty(
			st, foundActorsForKeyEmpty,
			"Receptionist not empty for keyEmpty "+
				"after UnregisterAll.",
		)
	})

	t.Run("unregister all with mixed keys", func(st *testing.T) {
		keyA := NewServiceKey[*testMsg, string]("sk-ua-keyA")
		keyB := NewServiceKey[*testMsg, string]("sk-ua-keyB")

		// Spawn 3 actors, two of them will share the same service key.
		actorA1 := keyA.Spawn(as, "actorA1-ua-mixed", beh)
		actorA2 := keyA.Spawn(as, "actorA2-ua-mixed", beh)
		actorB1 := keyB.Spawn(as, "actorB1-ua-mixed", beh)

		// Make sure we're able to find them in the receptionist.
		require.Len(
			st, FindInReceptionist(as.Receptionist(), keyA), 2,
			"KeyA initial registration count mismatch.",
		)
		require.Len(
			st, FindInReceptionist(as.Receptionist(), keyB), 1,
			"KeyB initial registration count mismatch.",
		)

		// We'll start by unregistering all actors for keyA.
		stoppedCountKeyA := keyA.UnregisterAll(as)
		require.Equal(
			st, 2, stoppedCountKeyA,
			"UnregisterAll for keyA returned incorrect count.",
		)

		// Verify keyA actors are gone from receptionist, keyB actor
		// remains.
		require.Empty(
			st, FindInReceptionist(as.Receptionist(), keyA),
			"actors for keyA still in receptionist after "+
				"UnregisterAll.",
		)
		foundActorsForKeyBAfterA := FindInReceptionist(
			as.Receptionist(), keyB,
		)
		require.Len(
			st, foundActorsForKeyBAfterA, 1,
			"Actor for keyB affected by UnregisterAll on keyA.",
		)
		require.Equal(
			st, actorB1, foundActorsForKeyBAfterA[0],
			"Wrong actor found for keyB.",
		)

		// Verify keyA actors are removed from system map, keyB actor
		// remains.
		as.mu.RLock()
		_, actorA1ExistsAfterMixed := as.actors[actorA1.ID()]
		_, actorA2ExistsAfterMixed := as.actors[actorA2.ID()]
		_, actorB1ExistsAfterMixed := as.actors[actorB1.ID()]
		as.mu.RUnlock()
		require.False(
			st, actorA1ExistsAfterMixed,
			"ActorA1 still in system actors map after "+
				"mixed UnregisterAll.",
		)
		require.False(
			st, actorA2ExistsAfterMixed,
			"ActorA2 still in system actors map after "+
				"mixed UnregisterAll.",
		)
		require.True(
			st, actorB1ExistsAfterMixed,
			"ActorB1 removed from system actors map incorrectly.",
		)

		// Verify keyA actors are stopped, keyB actor is running.
		resultActorA1Mixed := actorA1.Ask(
			context.Background(), newTestMsg("ping-kA-a1"),
		).Await(context.Background())
		require.True(st, resultActorA1Mixed.IsErr())
		require.ErrorIs(
			st, resultActorA1Mixed.Err(), ErrActorTerminated,
		)

		resultActorB1Mixed := actorB1.Ask(
			context.Background(), newTestMsg("ping-kB-a1"),
		).Await(context.Background())
		require.False(
			st, resultActorB1Mixed.IsErr(),
			"ActorB1 terminated incorrectly (mixed test): %v",
			resultActorB1Mixed.Err(),
		)
		resultActorB1Mixed.WhenOk(func(s string) {
			require.Equal(st, "echo: ping-kB-a1", s)
		})
	})

	t.Run("idempotency of UnregisterAll", func(st *testing.T) {
		keyIdempotent := NewServiceKey[*testMsg, string](
			"sk-ua-key-idem",
		)
		actorIdem := keyIdempotent.Spawn(as, "actor-idem-ua", beh)

		// First call should unregister and stop.
		stoppedCountFirstCall := keyIdempotent.UnregisterAll(as)
		require.Equal(
			st, 1, stoppedCountFirstCall,
			"UnregisterAll (first call) incorrect count.",
		)

		// Second call should do nothing and return 0.
		stoppedCountSecondCall := keyIdempotent.UnregisterAll(as)
		require.Equal(
			st, 0, stoppedCountSecondCall,
			"UnregisterAll (second call) incorrect count, not "+
				"idempotent.",
		)

		// Verify actor is gone from receptionist and system map, and is
		// stopped.
		require.Empty(
			st, FindInReceptionist(as.Receptionist(), keyIdempotent),
			"Actors for keyIdempotent still in receptionist "+
				"after calls.",
		)

		as.mu.RLock()
		_, actorIdemExistsAfter := as.actors[actorIdem.ID()]
		as.mu.RUnlock()
		require.False(
			st, actorIdemExistsAfter,
			"ActorIdem still in system actors map after calls.",
		)

		resultActorIdem := actorIdem.Ask(
			context.Background(), newTestMsg("ping-kidem-a1"),
		).Await(context.Background())
		require.True(st, resultActorIdem.IsErr())
		require.ErrorIs(st, resultActorIdem.Err(), ErrActorTerminated)
	})
}

// routerTestHarness helps set up routers and their associated actors for testing.
// It uses an actorTestHarness internally for DLO observation for the router.
type routerTestHarness struct {
	*actorTestHarness
	as           *ActorSystem
	receptionist *Receptionist
}

// newRouterTestHarness sets up a new harness for router testing.
// It creates an ActorSystem for actors that the router will route to,
// and uses the embedded actorTestHarness for the router's own DLO.
func newRouterTestHarness(t *testing.T) *routerTestHarness {
	t.Helper()
	system := NewActorSystem()
	t.Cleanup(func() {
		err := system.Shutdown()
		require.NoError(t, err, "router test actor system shutdown failed")
	})

	// The DLO for the router itself will come from actorTestHarness.
	// Actors managed by `system` (router targets) will use `system.DeadLetters()`.
	return &routerTestHarness{
		actorTestHarness: newActorTestHarness(t),
		as:               system,
		receptionist:     system.Receptionist(),
	}
}

// newRouterTargetActor creates an actor, registers it with the harness's
// ActorSystem (h.as) and Receptionist under the given service key. This actor
// is intended to be a target for the router.
func (h *routerTestHarness) newRouterTargetActor(id string,
	key ServiceKey[*testMsg, string],
	beh ActorBehavior[*testMsg, string]) ActorRef[*testMsg, string] {

	h.t.Helper()
	return RegisterWithSystem(h.as, id, key, beh)
}

// TestRouterNewRouter verifies that a new router can be created as expected.
func TestRouterNewRouter(t *testing.T) {
	t.Parallel()
	h := newRouterTestHarness(t)

	key := NewServiceKey[*testMsg, string]("router-service")
	strategy := NewRoundRobinStrategy[*testMsg, string]()

	router := NewRouter(h.receptionist, key, strategy, h.dlo.Ref())
	require.NotNil(t, router, "newRouter should not return nil")
	require.Equal(t, "router(router-service)", router.ID(), "router ID mismatch")
}

// countingEchoBehavior is an echo behavior that also counts how many messages
// it has processed.
type countingEchoBehavior struct {
	*echoBehavior
	id            string
	processedMsgs int64
}

func newCountingEchoBehavior(t *testing.T, id string) *countingEchoBehavior {
	return &countingEchoBehavior{
		echoBehavior: newEchoBehavior(t, 0),
		id:           id,
	}
}

func (b *countingEchoBehavior) Receive(ctx context.Context,
	msg *testMsg) fn.Result[string] {

	atomic.AddInt64(&b.processedMsgs, 1)

	// Include actor ID in reply for easier verification.
	res := b.echoBehavior.Receive(ctx, msg)
	val, err := res.Unpack()
	if err == nil {
		return fn.Ok(fmt.Sprintf("%s:%s", b.id, val))
	}
	return res
}

// TestRouterTellAndAskRoundRobin verifies that the router distributes messages
// in a round robin properly.
func TestRouterTellAndAskRoundRobin(t *testing.T) {
	t.Parallel()
	h := newRouterTestHarness(t)

	// Make a new router for the given service key and round robin strategy.
	serviceKey := NewServiceKey[*testMsg, string]("rr-service")
	strategy := NewRoundRobinStrategy[*testMsg, string]()
	router := NewRouter(h.receptionist, serviceKey, strategy, h.dlo.Ref())

	// We'll now register two actors with the router, each with a different
	// service key.
	actor1Beh := newCountingEchoBehavior(t, "actor1")
	actor2Beh := newCountingEchoBehavior(t, "actor2")
	_ = h.newRouterTargetActor("actor1-rr", serviceKey, actor1Beh)
	_ = h.newRouterTargetActor("actor2-rr", serviceKey, actor2Beh)

	// Nxet, we'll send a mix of Tell and Ask messages to the router.
	numMessages := 6
	for i := 0; i < numMessages; i++ {
		msgData := fmt.Sprintf("message-%d", i)
		if i%2 == 0 {
			router.Tell(context.Background(), newTestMsg(msgData))
		} else {
			future := router.Ask(
				context.Background(), newTestMsg(msgData),
			)
			ctxAwait, cancelAwait := context.WithTimeout(
				context.Background(), time.Second,
			)

			result := future.Await(ctxAwait)
			cancelAwait()
			require.False(
				t, result.IsErr(), "ask failed: %v", result.Err(),
			)
		}
	}

	// Wait a bit for Tell messages to be processed.
	time.Sleep(100 * time.Millisecond)

	// Each actor should have processed numMessages / 2 messages.
	require.EqualValues(
		t, numMessages/2, atomic.LoadInt64(&actor1Beh.processedMsgs),
		"actor1 processed message count mismatch",
	)
	require.EqualValues(
		t, numMessages/2, atomic.LoadInt64(&actor2Beh.processedMsgs),
		"actor2 processed message count mismatch",
	)

	// Router's DLO should be empty.
	h.assertNoDLOMessages()
}

// TestRouterNoActorsAvailable verifies that if no actors are available for the
// message, then an error is returned.
func TestRouterNoActorsAvailable(t *testing.T) {
	t.Parallel()
	h := newRouterTestHarness(t)

	serviceKey := NewServiceKey[*testMsg, string]("no-actor-service")
	strategy := NewRoundRobinStrategy[*testMsg, string]()
	router := NewRouter(h.receptionist, serviceKey, strategy, h.dlo.Ref())

	// We'll send a message, then assert that it goes to the DLO.
	tellMsg := newTestMsg("tell-no-actor")
	router.Tell(context.Background(), tellMsg)
	h.assertDLOMessage(tellMsg)

	// If we use an ask instead, then we should get an error.
	askMsg := newTestMsg("ask-no-actor")
	future := router.Ask(context.Background(), askMsg)
	result := future.Await(context.Background())

	require.True(
		t, result.IsErr(), "ask should fail when no actors are available",
	)
	require.ErrorIs(t, result.Err(), ErrNoActorsAvailable, "error mismatch")
}

// TestRouterTellAskContextCancellation verifies that if the context is
// canceled, then sending aborts.
func TestRouterTellAskContextCancellation(t *testing.T) {
	t.Parallel()
	h := newRouterTestHarness(t)

	serviceKey := NewServiceKey[*testMsg, string]("ctx-cancel-service")
	strategy := NewRoundRobinStrategy[*testMsg, string]()
	router := NewRouter(h.receptionist, serviceKey, strategy, h.dlo.Ref())

	// Use a regular echo actor, but we'll control context for Tell/Ask.
	targetActorBeh := newEchoBehavior(t, 50*time.Millisecond)
	_ = h.newRouterTargetActor("target-ctx", serviceKey, targetActorBeh)

	// Next, we'll send a Tell message with a context that will be cancelled
	// before we even send.
	ctxTell, cancelTell := context.WithCancel(context.Background())
	cancelTell()
	router.Tell(ctxTell, newTestMsg("tell-ctx-cancelled"))

	// The Message should be dropped by actorRefImpl.Tell if ctx is
	// cancelled. Router's DLO should not receive it from this path.
	h.assertNoDLOMessages()

	// Next, we'll do the same for Ask. This time, we should get an error.
	ctxAsk, cancelAsk := context.WithCancel(context.Background())
	cancelAsk()
	futureAsk := router.Ask(ctxAsk, newTestMsg("ask-ctx-cancelled"))
	resultAsk := futureAsk.Await(context.Background())

	require.True(
		t, resultAsk.IsErr(), "ask with cancelled context should fail",
	)
	require.ErrorIs(
		t, resultAsk.Err(), context.Canceled,
		"error should be context.Canceled",
	)
}

// TestRouterDynamicActorRegistration tests that we're able to dynamically add
// and remove actors from the router.
func TestRouterDynamicActorRegistration(t *testing.T) {
	t.Parallel()
	h := newRouterTestHarness(t)

	serviceKey := NewServiceKey[*testMsg, string]("dynamic-service")
	strategy := NewRoundRobinStrategy[*testMsg, string]()
	router := NewRouter(h.receptionist, serviceKey, strategy, h.dlo.Ref())

	// If we try to send a mesasge to the router before any actors are
	// added, we should get an error.
	futureNoActor := router.Ask(context.Background(), newTestMsg("ping-no-actors"))
	resNoActor := futureNoActor.Await(context.Background())
	require.ErrorIs(t, resNoActor.Err(), ErrNoActorsAvailable)

	actor1Beh := newCountingEchoBehavior(t, "actor1")
	actor1Ref := h.newRouterTargetActor("actor1-dynamic", serviceKey, actor1Beh)

	// At this point, we have a new actor added, but we'll try to send a
	// message to a different actor ID. This should go to the router's DLO.
	futureActor1 := router.Ask(context.Background(), newTestMsg("ping-actor1"))
	ctxAwaitA1, cancelAwaitA1 := context.WithTimeout(context.Background(), time.Second)
	resActor1 := futureActor1.Await(ctxAwaitA1)
	cancelAwaitA1()
	require.False(t, resActor1.IsErr(), "ask to actor1 failed: %v", resActor1.Err())
	resActor1.WhenOk(func(s string) {
		require.Equal(t, "actor1:echo: ping-actor1", s)
	})

	actor2Beh := newCountingEchoBehavior(t, "actor2")
	actor2Ref := h.newRouterTargetActor(
		"actor2-dynamic", serviceKey, actor2Beh,
	)

	// Now that we've added two actors above, we should round robin between
	// them when sending.
	ctxAwaitDA1, cancelAwaitDA1 := context.WithTimeout(
		context.Background(), time.Second,
	)
	router.Ask(context.Background(), newTestMsg("dynamic-ask1")).Await(
		ctxAwaitDA1,
	)
	cancelAwaitDA1()

	ctxAwaitDA2, cancelAwaitDA2 := context.WithTimeout(context.Background(), time.Second)
	router.Ask(context.Background(), newTestMsg("dynamic-ask2")).Await(ctxAwaitDA2)
	cancelAwaitDA2()

	time.Sleep(50 * time.Millisecond)

	// actor1 should have processed 2 messages (ping-actor1, dynamic-ask1),
	require.EqualValues(t, 2, atomic.LoadInt64(&actor1Beh.processedMsgs))
	require.EqualValues(t, 1, atomic.LoadInt64(&actor2Beh.processedMsgs))

	// Next, we'll unregister the first actor ref.
	unregistered := UnregisterFromReceptionist(
		h.receptionist, serviceKey, actor1Ref,
	)
	require.True(t, unregistered)

	// All the messages should now go to the second actor.
	for i := 0; i < 2; i++ {
		msgData := fmt.Sprintf("to-actor2-%d", i)
		future := router.Ask(context.Background(), newTestMsg(msgData))
		ctxAwaitLoop, cancelAwaitLoop := context.WithTimeout(
			context.Background(), time.Second,
		)

		res := future.Await(ctxAwaitLoop)
		cancelAwaitLoop()

		require.False(
			t, res.IsErr(), "ask to actor2 failed: %v", res.Err(),
		)
		res.WhenOk(func(s string) {
			require.Equal(t, "actor2:echo: "+msgData, s)
		})
	}

	// Actor 1 shouldn't have got any of the messages, they should go to
	// actor 2.
	require.EqualValues(t, 2, atomic.LoadInt64(&actor1Beh.processedMsgs))
	require.EqualValues(t, 1+2, atomic.LoadInt64(&actor2Beh.processedMsgs))

	// Next, we'll unregister the second actor ref.
	unregistered2 := UnregisterFromReceptionist(
		h.receptionist, serviceKey, actor2Ref,
	)
	require.True(t, unregistered2)

	// If we try to send another message, it should go to the DL.
	tellMsg := newTestMsg("dynamic-tell-no-actors")
	router.Tell(context.Background(), tellMsg)
	h.assertDLOMessage(tellMsg)
}
