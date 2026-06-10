package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests sending and receiving of the onion message type.
//
//nolint:ll
func testOnionMessage(ht *lntest.HarnessTest) {
	// Alice needs coins to fund a channel with Bob: onion message ingress
	// is gated on the sender and receiver sharing at least one fully open
	// channel as the Sybil-resistance layer on top of the byte-granular
	// rate limiter.
	alice := ht.NewNodeWithCoins("Alice", nil)
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

	// Connect alice and bob and open a channel between them. Onion message
	// ingress is gated on having at least one fully open channel with the
	// sending peer, so without a channel Alice would silently drop Bob's
	// message and the test would time out.
	ht.EnsureConnected(alice, bob)
	ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt: btcutil.Amount(100_000),
	})

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
