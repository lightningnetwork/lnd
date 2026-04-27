package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests sending and receiving of the onion message type.
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

	// Build a valid onion message destined for Alice.
	alicePubKey, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht.T, err)

	// Alice is the final destination, so her route data is empty.
	aliceData := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub: alicePubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, aliceData,
			),
		},
	}

	blindedPath := onionmessage.BuildBlindedPath(ht.T, hops)

	// Add a custom payload to verify it's received correctly.
	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{1, 2, 3},
		},
	}

	onionMsg, _ := onionmessage.BuildOnionMessage(
		ht.T, blindedPath, finalHopTLVs,
	)

	// Send it from Bob to Alice.
	pathKey := blindedPath.SessionKey.PubKey().SerializeCompressed()
	bobMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    alice.PubKey[:],
		PathKey: pathKey,
		Onion:   onionMsg.OnionBlob,
	}
	bob.RPC.SendOnionMessage(bobMsg)

	// Wait for Alice to receive the message.
	select {
	case msg := <-messages:
		// Check we received the message from Bob.
		require.Equal(ht, bob.PubKey[:], msg.Peer, "msg peer wrong")

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("alice did not receive onion message: %v", bobMsg)
	}
}
