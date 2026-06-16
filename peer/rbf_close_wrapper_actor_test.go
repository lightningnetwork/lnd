package peer

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/stretchr/testify/require"
)

// TestRbfCloseActorSingleton verifies that registering an RBF close actor for
// the same channel point twice results in only a single registered actor. The
// second call to registerActor should unregister the first actor before
// spawning a replacement.
func TestRbfCloseActorSingleton(t *testing.T) {
	t.Parallel()

	actorSystem := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, actorSystem.Shutdown())
	})

	chanPoint := wire.OutPoint{Index: 1}
	serviceKey := NewRbfCloserPeerServiceKey(chanPoint)

	// Register the actor for the first time.
	actor1 := newRbfCloseActor(chanPoint, nil, actorSystem)
	require.NoError(t, actor1.registerActor())

	// Verify exactly one actor is registered.
	refs := actor.FindInReceptionist(
		actorSystem.Receptionist(), serviceKey,
	)
	require.Len(t, refs, 1)

	// Register the actor again for the same channel point.
	actor2 := newRbfCloseActor(chanPoint, nil, actorSystem)
	require.NoError(t, actor2.registerActor())

	// Verify there is still exactly one actor registered (the second one
	// replaced the first).
	refs = actor.FindInReceptionist(
		actorSystem.Receptionist(), serviceKey,
	)
	require.Len(t, refs, 1)
}
