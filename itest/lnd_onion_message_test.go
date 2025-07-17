package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
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
