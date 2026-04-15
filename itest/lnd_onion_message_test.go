package itest

import (
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests sending and receiving of the onion message type.
//
//nolint:ll
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

	// Bob sends an onion message to Alice using the send API. The server
	// handles pathfinding and blinded path construction automatically.
	finalPayload := []byte{1, 2, 3}
	bobMsg := &lnrpc.SendOnionMessageRequest{
		Destination: alice.PubKey[:],
		FinalHopTlvs: map[uint64][]byte{
			uint64(lnwire.InvoiceRequestNamespaceType): finalPayload,
		},
	}
	bob.RPC.SendOnionMessage(bobMsg)

	// Wait for Alice to receive the message.
	select {
	case msg := <-messages:
		// Check we received the message from Bob.
		require.Equal(ht, bob.PubKey[:], msg.Peer, "msg peer wrong")
		require.Equal(
			ht, finalPayload,
			msg.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)],
		)

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("alice did not receive onion message: %v", bobMsg)
	}
}
