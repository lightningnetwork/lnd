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
	actorRef, _ := SpawnOnionPeerActor(system, sender, pubKeyArr)

	// Create and send a test message.
	testPathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	expectedMsg := lnwire.NewOnionMessage(
		testPathKey.PubKey(), []byte{1, 2, 3, 4},
	)

	req := &Request{msg: *expectedMsg}
	actorRef.Tell(t.Context(), req)

	// Verify the message was sent.
	select {
	case receivedMsg := <-sent:
		require.Equal(t, expectedMsg.PathKey, receivedMsg.PathKey)
		require.Equal(t, expectedMsg.OnionBlob, receivedMsg.OnionBlob)
	case <-time.After(time.Second * 5):
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

// TestOnionPeerActorReceive tests the OnionPeerActor.Receive method directly
// without the actor system, verifying that messages are properly forwarded to
// the sender.
func TestOnionPeerActorReceive(t *testing.T) {
	t.Parallel()

	// Track messages sent through the sender.
	var sentMsg *lnwire.OnionMessage
	peerActor := NewOnionPeerActor(func(msg *lnwire.OnionMessage) {
		sentMsg = msg
	})

	// Create a test message.
	testPathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	expectedMsg := lnwire.NewOnionMessage(
		testPathKey.PubKey(), []byte{1, 2, 3, 4},
	)

	req := &Request{msg: *expectedMsg}

	// Call Receive directly.
	result := peerActor.Receive(t.Context(), req)

	// Verify success.
	require.True(t, result.IsOk())
	result.WhenOk(func(resp *Response) {
		require.True(t, resp.Success)
	})

	// Verify the message was sent.
	require.NotNil(t, sentMsg)
	require.Equal(t, expectedMsg.PathKey, sentMsg.PathKey)
	require.Equal(t, expectedMsg.OnionBlob, sentMsg.OnionBlob)
}

// TestOnionPeerActorReceiveContextCanceled tests that OnionPeerActor.Receive
// returns an error when the context is canceled.
func TestOnionPeerActorReceiveContextCanceled(t *testing.T) {
	t.Parallel()

	peerActor := NewOnionPeerActor(func(msg *lnwire.OnionMessage) {
		t.Fatal("sender should not be called when context is canceled")
	})

	// Create a canceled context.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := &Request{}

	// Call Receive with canceled context.
	result := peerActor.Receive(ctx, req)

	// Verify error.
	require.True(t, result.IsErr())
	result.WhenErr(func(err error) {
		require.ErrorIs(t, err, ErrActorShuttingDown)
	})
}

// TestStopPeerActor verifies that StopPeerActor correctly stops and removes
// an actor that was spawned for a peer.
func TestStopPeerActor(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	// Generate a pubkey for the peer.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	var pubKeyArr [33]byte
	copy(pubKeyArr[:], pubKey.SerializeCompressed())

	// Spawn the actor.
	sender := func(msg *lnwire.OnionMessage) {}
	_, _ = SpawnOnionPeerActor(system, sender, pubKeyArr)

	// Verify the actor exists.
	actorOpt := findPeerActor(system.Receptionist(), pubKeyArr)
	require.True(t, actorOpt.IsSome(), "actor should exist before stop")

	// Stop the actor.
	StopPeerActor(system, pubKeyArr)

	// Verify the actor no longer exists.
	actorOpt = findPeerActor(system.Receptionist(), pubKeyArr)
	require.True(t, actorOpt.IsNone(), "actor should not exist after stop")
}

// TestStopPeerActorNotExists verifies that StopPeerActor is a no-op when
// no actor exists for the given pubkey.
func TestStopPeerActorNotExists(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() { _ = system.Shutdown() }()

	// Generate a pubkey for a peer that has no actor.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	var pubKeyArr [33]byte
	copy(pubKeyArr[:], pubKey.SerializeCompressed())

	// This should not panic or error - it's a no-op.
	StopPeerActor(system, pubKeyArr)
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
