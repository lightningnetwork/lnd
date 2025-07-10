package onionmessage

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestSpawnOnionPeerActorSendsMessage verifies that spawning an actor and
// sending a message results in the sender function being called with the
// correct onion message.
func TestSpawnOnionPeerActorSendsMessage(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	// Generate a pubkey for the peer.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	var pubKeyArr [33]byte
	copy(pubKeyArr[:], pubKey.SerializeCompressed())

	// Track messages sent through the sender function.
	sent := make(chan *lnwire.OnionMessage, 1)
	sender := func(msg *lnwire.OnionMessage) {
		sent <- msg
	}

	// Spawn the actor.
	actorRef := SpawnOnionPeerActor(system, sender, pubKeyArr)

	// Create and send a test message.
	testPathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	expectedMsg := lnwire.NewOnionMessage(
		testPathKey.PubKey(), []byte{1, 2, 3, 4},
	)

	req := &OMRequest{msg: *expectedMsg}
	actorRef.Tell(context.Background(), req)

	// Verify the message was sent.
	select {
	case receivedMsg := <-sent:
		require.Equal(t, expectedMsg.PathKey, receivedMsg.PathKey)
		require.Equal(t, expectedMsg.OnionBlob, receivedMsg.OnionBlob)
	case <-time.After(time.Second):
		require.FailNow(t, "message not sent within timeout")
	}
}

// TestFindPeerActorExists verifies that findPeerActor returns the actor
// reference when the actor has been spawned.
func TestFindPeerActorExists(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	receptionist := system.Receptionist()

	// Generate a pubkey for the peer.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	var pubKeyArr [33]byte
	copy(pubKeyArr[:], pubKey.SerializeCompressed())

	// Spawn the actor.
	sender := func(msg *lnwire.OnionMessage) {}
	SpawnOnionPeerActor(system, sender, pubKeyArr)

	// Find the actor.
	actorOpt := findPeerActor(receptionist, pubKeyArr)

	require.True(t, actorOpt.IsSome(), "actor should be found")
}

// TestFindPeerActorNotExists verifies that findPeerActor returns None when
// no actor has been spawned for the given pubkey.
func TestFindPeerActorNotExists(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	receptionist := system.Receptionist()

	// Generate a pubkey for a peer that has no actor.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	var pubKeyArr [33]byte
	copy(pubKeyArr[:], pubKey.SerializeCompressed())

	// Try to find the actor without spawning one.
	actorOpt := findPeerActor(receptionist, pubKeyArr)

	require.True(t, actorOpt.IsNone(), "actor should not be found")
}
