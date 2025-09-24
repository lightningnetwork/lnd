package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests sending and receiving of the onion message type.
func testOnionMessage(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// Subscribe Alice to onion messages before we send any, so that we
	// don't miss any.
	msgClient, cancel := alice.RPC.SubscribeOnionMessages()
	defer cancel()

	// Create a channel to receive onion messages on.
	messages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			// If we fail to receive, just exit. The test should
			// fail elsewhere if it doesn't get a message that it
			// was expecting.
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}

			// Deliver the message into our channel or exit if the
			// test is shutting down.
			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	// Connect alice and bob so that they can exchange messages.
	ht.EnsureConnected(alice, bob)

	// Create a random onion message.
	randomPriv, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)
	randomPub := randomPriv.PubKey()
	msgBlindingPoint := randomPub.SerializeCompressed()
	msgOnion := []byte{1, 2, 3}

	// Send it from Bob to Alice.
	bobMsg := &lnrpc.SendOnionMessageRequest{
		Peer:          alice.PubKey[:],
		BlindingPoint: msgBlindingPoint,
		Onion:         msgOnion,
	}
	bob.RPC.SendOnionMessage(bobMsg)

	// Wait for Alice to receive the message.
	select {
	case msg := <-messages:
		// Check our type and data and (sanity) check the peer we got
		// it from.
		require.Equal(ht, msgOnion, msg.Onion, "msg data wrong")
		require.Equal(ht, msgBlindingPoint, msg.BlindingPoint, "msg "+
			"blinding point wrong")
		require.Equal(ht, bob.PubKey[:], msg.Peer, "msg peer wrong")

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("alice did not receive onion message: %v", bobMsg)
	}
}
