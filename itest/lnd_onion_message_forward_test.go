package itest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// dropWait is how long a test waits to confirm that a message was dropped.
const dropWait = 5 * time.Second

// onionMessageTestCase defines a test case for onion message forwarding.
// Channel creation and SCID selection stay in the test body. A case is
// either a path builder, the already-open Bob-Carol channel, or a channel
// opened from openChannel.
type onionMessageTestCase struct {
	name string

	// expectDrop is true when the next hop must not resolve and Carol
	// must not receive the message.
	expectDrop bool

	// useExisting forwards via the confirmed SCID of the Bob-Carol
	// channel opened with the network.
	useExisting bool

	// openChannel, if set, opens a Bob-Carol channel with these params
	// and forwards via its SCID.
	openChannel *lntest.OpenChannelParams

	// useAlias selects the first alias of that channel instead of the
	// confirmed SCID.
	useAlias bool

	// buildPath builds a blinded path that does not come from a channel
	// SCID. It returns the blinded path, the final hop payloads, the
	// first hop node, and the expected receiving peer pubkey.
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
	// Spin up a three-node chain Alice -> Bob -> Carol, with both
	// channels opened up front via CreateSimpleNetwork. Opening the
	// channels before any forwarding run matters because onion message
	// ingress is gated on having at least one fully open channel with
	// the sending peer, so without these channels every hop would
	// silently drop the message. The Bob -> Carol channel also doubles
	// as the SCID source for the "forward via scid" test case, which
	// keeps the per-test setup minimal.
	//
	// Bob and Carol also enable option-scid-alias, so the private
	// channel cases can resolve a local alias.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.anchors",
	}
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, scidAliasArgs, scidAliasArgs},
		lntest.OpenChannelParams{
			Amt: btcutil.Amount(100_000),
		},
	)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	bobCarolChan := chanPoints[1]

	privateParams := &lntest.OpenChannelParams{
		Amt:     btcutil.Amount(100_000),
		Private: true,
	}
	aliasParams := &lntest.OpenChannelParams{
		Amt:       btcutil.Amount(100_000),
		Private:   true,
		ScidAlias: true,
	}

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
			name:        "forward via scid",
			useExisting: true,
		},
		{
			name:      "forward concatenated path",
			buildPath: buildConcatenatedPath,
		},
		{
			// BOLT 4 resolves only an announced SCID or a local
			// alias. The confirmed SCID of a private channel is
			// in the local graph and must not resolve.
			name:        "drop via private channel confirmed scid",
			expectDrop:  true,
			openChannel: privateParams,
		},
		{
			// A local alias is not in the graph, so Bob resolves
			// it through the HTLC switch.
			name:        "forward via private channel alias",
			openChannel: aliasParams,
			useAlias:    true,
		},
	}

	for _, tc := range testCases {
		success := ht.Run(tc.name, func(t *testing.T) {
			var (
				blindedPath   *sphinx.BlindedPathInfo
				finalPayloads []*lnwire.FinalHopTLV
				firstHop      *node.HarnessNode
				expectedPeer  []byte
			)
			if tc.buildPath != nil {
				blindedPath, finalPayloads, firstHop,
					expectedPeer = tc.buildPath(
					ht, alice, bob, carol,
				)
			} else {
				var channel *lnrpc.Channel
				if tc.useExisting {
					ht.AssertChannelInGraph(
						bob, bobCarolChan,
					)
					channel = ht.QueryChannelByChanPoint(
						bob, bobCarolChan,
					)
				} else {
					chanPoint := ht.OpenChannel(
						bob, carol, *tc.openChannel,
					)
					channel = ht.QueryChannelByChanPoint(
						bob, chanPoint,
					)
				}

				var scid lnwire.ShortChannelID
				if tc.useAlias {
					require.NotEmpty(
						ht, channel.AliasScids,
					)
					scid = lnwire.NewShortChanIDFromInt(
						channel.AliasScids[0],
					)
				} else {
					scid = lnwire.NewShortChanIDFromInt(
						channel.ChanId,
					)
				}

				blindedPath, finalPayloads, firstHop,
					expectedPeer = buildForwardSCIDPath(
					ht, bob, carol, scid,
				)
			}

			// Build the onion message.
			onionMsg, _ := onionmessage.BuildOnionMessage(
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

			// A dropped message must not reach Carol.
			if tc.expectDrop {
				select {
				case <-messages:
					ht.Fatalf("carol received a dropped " +
						"onion message")

				case <-time.After(dropWait):
				}

				return
			}

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
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
		{
			NodePub: carolPubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}

	blindedPath := onionmessage.BuildBlindedPath(ht.T, hops)

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
	carol *node.HarnessNode, scid lnwire.ShortChannelID) (
	*sphinx.BlindedPathInfo, []*lnwire.FinalHopTLV,
	*node.HarnessNode, []byte,
) {

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)

	// Bob's payload: forward to Carol via the caller-supplied SCID. The
	// caller picks the confirmed SCID or a local alias.
	nextNode := fn.NewRight[*btcec.PublicKey](scid)
	bobData := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)

	// Carol's payload: final hop (empty route data).
	carolData := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub: bobPubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
		{
			NodePub: carolPubKey,
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}

	blindedPath := onionmessage.BuildBlindedPath(ht.T, hops)

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
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, carolData,
			),
		},
	}
	receiverPath := onionmessage.BuildBlindedPath(ht.T, receiverHops)

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
			PlainText: onionmessage.EncodeBlindedRouteData(
				ht.T, bobData,
			),
		},
	}
	senderPath := onionmessage.BuildBlindedPath(ht.T, senderHops)

	// Concatenate the paths.
	concatenatedPath := onionmessage.ConcatBlindedPaths(
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
