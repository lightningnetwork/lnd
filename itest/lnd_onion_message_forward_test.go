package itest

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
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

	// Open channels: Alice --- Bob --- Carol and wait for each node to
	// sync the network graph.
	aliceBobChanPoint := ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt: 500_000,
	})
	ht.AssertNumChannelUpdates(carol, aliceBobChanPoint, 2)

	bobCarolChanPoint := ht.OpenChannel(bob, carol, lntest.OpenChannelParams{
		Amt: 500_000,
	})
	ht.AssertNumChannelUpdates(alice, bobCarolChanPoint, 2)

	// Create a blinded route

	// Create a set of 2 blinded hops for our path.
	hopsToBlind := make([]*sphinx.HopInfo, 2)

	// Our path is: Alice -> Bob -> Carol
	// So Bob needs to receive the public Key of Carol.

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)
	data0 := record.NewNonFinalBlindedRouteDataOnionMessage(
		carolPubKey, nil, nil, nil,
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
	sphinxPath, err := blindedToSphinx(
		blindedPath.Path, nil, nil, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(ht.T, err)

	// Create an onion packet with no associated data.
	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.MaxRoutingPayloadSize, sphinx.MaxOnionMessagePayloadSize,
	)

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

	blindingPoint := blindingKey.PubKey().SerializeCompressed()

	// Send it from Alice to Bob.
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Peer:          bob.PubKey[:],
		BlindingPoint: blindingPoint,
		Onion:         buf.Bytes(),
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	// Wait for Carol to receive the message.
	select {
	case msg := <-messages:
		// Check our type and data and (sanity) check the peer we got
		// it from.
		require.Equal(ht, bob.PubKey[:], msg.Peer, "msg peer wrong")
		require.NotEmpty(ht, msg.CustomRecords)
		require.NotNil(ht, msg.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)])
		require.Equal(ht, msg.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)], []byte{1, 2, 3})

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("carol did not receive onion message: %v", aliceMsg)
	}

	ht.CloseChannel(alice, aliceBobChanPoint)
	ht.CloseChannel(bob, bobCarolChanPoint)
}

// blindedToSphinx converts the blinded path provided to a sphinx path that can
// be wrapped up in an onion, encoding the TLV payload for each hop along the
// way.
func blindedToSphinx(blindedRoute *sphinx.BlindedPath,
	extraHops []*lnwire.BlindedHop, replyPath *lnwire.ReplyPath,
	finalPayloads []*lnwire.FinalHopPayload) (
	*sphinx.PaymentPath, error) {

	var (
		sphinxPath sphinx.PaymentPath

		ourHopCount   = len(blindedRoute.BlindedHops)
		extraHopCount = len(extraHops)
	)

	// Fill in the blinded node id and encrypted data for all hops. This
	// requirement differs from blinded hops used for payments, where we
	// don't use the blinded introduction node id. However, since onion
	// messages are fully blinded by default, we use the blinded
	// introduction node id.
	for i := 0; i < ourHopCount; i++ {
		// Create an onion message payload with the encrypted data for
		// this hop.
		payload := &lnwire.OnionMessagePayload{
			EncryptedData: blindedRoute.BlindedHops[i].CipherText,
		}

		// If we're on the final hop and there are no extra hops to add
		// onto our path, include the tlvs intended for the final hop
		// and the reply path (if provided).
		if i == ourHopCount-1 && extraHopCount == 0 {
			payload.FinalHopPayloads = finalPayloads
			payload.ReplyPath = replyPath
		}

		// Encode the tlv stream for inclusion in our message.
		hop, err := createSphinxHop(
			*blindedRoute.BlindedHops[i].BlindedNodePub, payload,
		)
		if err != nil {
			return nil, fmt.Errorf("sphinx hop %v: %w", i, err)
		}
		sphinxPath[i] = *hop
	}

	// If we don't have any more hops to append to our path, just return
	// it as-is here.
	if extraHopCount == 0 {
		return &sphinxPath, nil
	}

	for i := 0; i < extraHopCount; i++ {
		payload := &lnwire.OnionMessagePayload{
			EncryptedData: extraHops[i].EncryptedData,
		}

		// If we're on the last hop, add our optional final payload
		// and reply path.
		if i == extraHopCount-1 {
			payload.FinalHopPayloads = finalPayloads
			payload.ReplyPath = replyPath
		}

		hop, err := createSphinxHop(
			*extraHops[i].BlindedNodeID, payload,
		)
		if err != nil {
			return nil, fmt.Errorf("sphinx hop %v: %w", i, err)
		}

		// We need to offset our index in the sphinx path by the
		// number of hops that we added in the loop above.
		sphinxIndex := i + ourHopCount
		sphinxPath[sphinxIndex] = *hop
	}

	return &sphinxPath, nil
}

// createSphinxHop encodes an onion message payload and produces a sphinx
// onion hop for it.
func createSphinxHop(nodeID btcec.PublicKey,
	payload *lnwire.OnionMessagePayload) (*sphinx.OnionHop, error) {

	payloadTLVs, err := payload.Encode()
	if err != nil {
		return nil, fmt.Errorf("payload: encode: %v", err)
	}

	return &sphinx.OnionHop{
		NodePub: nodeID,
		HopPayload: sphinx.HopPayload{
			Type:    sphinx.PayloadTLV,
			Payload: payloadTLVs,
		},
	}, nil
}
