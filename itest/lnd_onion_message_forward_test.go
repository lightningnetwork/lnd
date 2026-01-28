package itest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage/testhelpers"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// onionMessageTestCase defines a test case for onion message forwarding.
type onionMessageTestCase struct {
	name string

	// setup is called before building the blinded path to perform any
	// additional setup (e.g., opening channels for SCID tests).
	setup func(ht *lntest.HarnessTest, alice, bob, carol *node.HarnessNode)

	// buildPath builds the blinded path for the test. It returns the
	// blinded path info, the final hop payloads, the first hop node,
	// and the expected receiving peer pubkey for validation.
	buildPath func(ht *lntest.HarnessTest, alice, bob,
		carol *node.HarnessNode) (
		blindedPath *sphinx.BlindedPathInfo,
		finalHopTLVs []*lnwire.FinalHopTLV,
		firstHop *node.HarnessNode,
		expectedPeer []byte,
	)
}

// testOnionMessageForwarding tests forwarding of onion messages across
// multiple scenarios including forwarding by node ID, by SCID, and with
// concatenated blinded paths.
func testOnionMessageForwarding(ht *lntest.HarnessTest) {
	// Spin up three nodes for the test network.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	// Connect nodes so they can share gossip and forward messages.
	ht.ConnectNodesPerm(alice, bob)
	ht.ConnectNodesPerm(bob, carol)

	testCases := []onionMessageTestCase{
		{
			name: "forward via next node id",
			buildPath: func(ht *lntest.HarnessTest, alice, bob,
				carol *node.HarnessNode) (
				*sphinx.BlindedPathInfo,
				[]*lnwire.FinalHopTLV,
				*node.HarnessNode, []byte,
			) {

				return buildForwardNextNodePath(
					ht, bob, carol,
				)
			},
		},
		{
			name: "forward via scid",
			setup: func(ht *lntest.HarnessTest, alice, bob,
				carol *node.HarnessNode) {

				// Open a channel between Bob and Carol so we
				// have an SCID to use.
				chanPoint := ht.OpenChannel(
					bob, carol,
					lntest.OpenChannelParams{Amt: 100000},
				)

				// Wait for the channel to be in the graph so
				// the SCID can be resolved.
				ht.AssertChannelInGraph(bob, chanPoint)
			},
			buildPath: func(ht *lntest.HarnessTest, alice, bob,
				carol *node.HarnessNode) (
				*sphinx.BlindedPathInfo,
				[]*lnwire.FinalHopTLV,
				*node.HarnessNode, []byte,
			) {

				return buildForwardSCIDPath(ht, bob, carol)
			},
		},
		{
			name:      "forward concatenated path",
			buildPath: buildConcatenatedPath,
		},
	}

	for _, tc := range testCases {
		success := ht.Run(tc.name, func(t *testing.T) {
			// Run optional setup.
			if tc.setup != nil {
				tc.setup(ht, alice, bob, carol)
			}

			// Build the blinded path for this test case.
			blindedPath, finalPayloads, firstHop, expectedPeer :=
				tc.buildPath(ht, alice, bob, carol)

			// Build the onion message.
			onionMsg, _ := testhelpers.BuildOnionMessage(
				ht.T, blindedPath, finalPayloads,
			)

			// Subscribe to onion messages on Carol before sending.
			msgClient, cancel := carol.RPC.SubscribeOnionMessages()
			defer cancel()

			messages := make(chan *lnrpc.OnionMessageUpdate)
			go func() {
				for {
					msg, err := msgClient.Recv()
					if err != nil {
						return
					}
					select {
					case messages <- msg:
					case <-ht.Context().Done():
						return
					}
				}
			}()

			// Send the message from Alice to the first hop.
			pathKey := blindedPath.SessionKey.PubKey().
				SerializeCompressed()
			aliceMsg := &lnrpc.SendOnionMessageRequest{
				Peer:    firstHop.PubKey[:],
				PathKey: pathKey,
				Onion:   onionMsg.OnionBlob,
			}
			alice.RPC.SendOnionMessage(aliceMsg)

			// Wait for Carol to receive the message.
			select {
			case msg := <-messages:
				require.Equal(
					ht, expectedPeer, msg.Peer,
					"unexpected peer",
				)

				// Verify final payload if provided.
				for _, fp := range finalPayloads {
					tlvType := uint64(fp.TLVType)
					require.Equal(
						ht, fp.Value,
						msg.CustomRecords[tlvType],
					)
				}

			case <-time.After(lntest.DefaultTimeout):
				ht.Fatalf("carol did not receive onion message")
			}
		})
		if !success {
			break
		}
	}
}

// buildForwardNextNodePath builds a blinded path for forwarding via explicit
// next node ID. Path: Alice -> Bob -> Carol.
func buildForwardNextNodePath(ht *lntest.HarnessTest, bob,
	carol *node.HarnessNode) (
	*sphinx.BlindedPathInfo, []*lnwire.FinalHopTLV,
	*node.HarnessNode, []byte,
) {

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)

	// Bob's payload: forward to Carol via node ID.
	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		carolPubKey,
	)
	bobData := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)

	// Carol's payload: final hop (empty route data).
	carolData := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub: bobPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
		{
			NodePub: carolPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}

	blindedPath := testhelpers.BuildBlindedPath(ht.T, hops)

	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{1, 2, 3},
		},
	}

	return blindedPath, finalHopTLVs, bob, bob.PubKey[:]
}

// buildForwardSCIDPath builds a blinded path for forwarding via SCID.
// Requires a channel between Bob and Carol to exist.
// Path: Alice -> Bob -> Carol (Bob uses SCID to identify Carol).
func buildForwardSCIDPath(ht *lntest.HarnessTest, bob,
	carol *node.HarnessNode) (
	*sphinx.BlindedPathInfo, []*lnwire.FinalHopTLV,
	*node.HarnessNode, []byte,
) {

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)

	// Get the SCID of the Bob-Carol channel from Bob's perspective.
	channels := bob.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		Peer: carol.PubKey[:],
	})
	require.Len(ht.T, channels.Channels, 1, "expected one channel")
	scid := lnwire.NewShortChanIDFromInt(channels.Channels[0].ChanId)

	// Bob's payload: forward to Carol via SCID.
	nextNode := fn.NewRight[*btcec.PublicKey](scid)
	bobData := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)

	// Carol's payload: final hop (empty route data).
	carolData := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub: bobPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
		{
			NodePub: carolPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}

	blindedPath := testhelpers.BuildBlindedPath(ht.T, hops)

	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{4, 5, 6},
		},
	}

	return blindedPath, finalHopTLVs, bob, bob.PubKey[:]
}

// buildConcatenatedPath builds a concatenated blinded path scenario.
// Alice builds a path to Bob, Carol provides a blinded path starting at Bob.
// Bob's payload includes NextBlindingOverride to switch to Carol's path.
// Path: Alice -> Bob (intro) -> Carol.
func buildConcatenatedPath(ht *lntest.HarnessTest, alice, bob,
	carol *node.HarnessNode) (
	*sphinx.BlindedPathInfo, []*lnwire.FinalHopTLV,
	*node.HarnessNode, []byte,
) {

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)

	// Carol creates a blinded path starting at Bob (introduction node).
	// Carol's route data: final hop.
	carolData := &record.BlindedRouteData{}

	receiverHops := []*sphinx.HopInfo{
		{
			NodePub: carolPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}
	receiverPath := testhelpers.BuildBlindedPath(ht.T, receiverHops)

	// Alice creates a path to Bob with NextBlindingOverride pointing to
	// Carol's blinding point.
	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		carolPubKey,
	)
	bobData := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, receiverPath.Path.BlindingPoint, nil,
	)

	senderHops := []*sphinx.HopInfo{
		{
			NodePub: bobPubKey,
			PlainText: testhelpers.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
	}
	senderPath := testhelpers.BuildBlindedPath(ht.T, senderHops)

	// Concatenate the paths.
	concatenatedPath := testhelpers.ConcatBlindedPaths(
		ht.T, senderPath, receiverPath,
	)

	finalHopTLVs := []*lnwire.FinalHopTLV{
		{
			TLVType: lnwire.InvoiceRequestNamespaceType,
			Value:   []byte{7, 8, 9},
		},
	}

	return concatenatedPath, finalHopTLVs, bob, bob.PubKey[:]
}
