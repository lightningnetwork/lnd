package itest

import (
	"bytes"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests forwarding of onion messages.
func testOnionMessageForwarding(ht *lntest.HarnessTest) {
	// Spin up a three node because we will need a three-hop network for
	// this test.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	// Create a session key for the blinded path.
	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	// Connect nodes before channel opening so that they can share gossip.
	ht.ConnectNodesPerm(alice, bob)
	ht.ConnectNodesPerm(bob, carol)

	// Create a blinded route

	// Create a set of 2 blinded hops for our path.
	hopsToBlind := make([]*sphinx.HopInfo, 2)

	// Our path is: Alice -> Bob -> Carol
	// So Bob needs to receive the public Key of Carol.

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)
	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		carolPubKey,
	)
	data0 := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	encoded0, err := record.EncodeBlindedRouteData(data0)
	require.NoError(ht.T, err)

	data1 := &record.BlindedRouteData{}
	encoded1, err := record.EncodeBlindedRouteData(data1)
	require.NoError(ht.T, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	// The first hop is for Bob. This will be blinded at a later stage.
	hopsToBlind[0] = &sphinx.HopInfo{
		NodePub:   bobPubKey,
		PlainText: encoded0,
	}
	// The second hop is to Carol.
	hopsToBlind[1] = &sphinx.HopInfo{
		NodePub:   carolPubKey,
		PlainText: encoded1,
	}

	blindedPath, err := sphinx.BuildBlindedPath(blindingKey, hopsToBlind)
	require.NoError(ht.T, err)

	finalHopPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{1, 2, 3},
	}

	// Convert that blinded path to a sphinx path and add a final payload.
	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPath.Path, nil, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(ht.T, err)

	// Create an onion packet with no associated data.
	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(ht.T, err, "new onion packet")

	buf := new(bytes.Buffer)
	err = onionPacket.Encode(buf)
	require.NoError(ht.T, err, "encode onion packet")

	// Subscribe Carol to onion messages before we send any, so that we
	// don't miss any.
	msgClient, cancel := carol.RPC.SubscribeOnionMessages()
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

	pathKey := blindingKey.PubKey().SerializeCompressed()

	// Send it from Alice to Bob.
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    bob.PubKey[:],
		PathKey: pathKey,
		Onion:   buf.Bytes(),
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	// Wait for Carol to receive the message.
	select {
	case msg := <-messages:
		// Check our type and data and (sanity) check the peer we got
		// it from.
		require.Equal(ht, bob.PubKey[:], msg.Peer, "msg peer wrong")
		require.NotEmpty(ht, msg.CustomRecords)
		customRecordsKey := uint64(lnwire.InvoiceRequestNamespaceType)
		require.NotNil(ht, msg.CustomRecords[customRecordsKey])
		require.Equal(
			ht, []byte{1, 2, 3},
			msg.CustomRecords[customRecordsKey],
		)

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("carol did not receive onion message: %v", aliceMsg)
	}
}
