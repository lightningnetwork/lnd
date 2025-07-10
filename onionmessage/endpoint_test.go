package onionmessage

/*
Onion Message Endpoint Tests

This file contains unit tests for the OnionEndpoint, which handles incoming
onion messages. The current implementation focuses exclusively on onion message
FORWARDING - we do not yet support sending or receiving onion messages
ourselves.

# Current Scope

The tests cover the forwarding node perspective:
  - Forward via NextNodeID: Route to next hop using explicit node pubkey
  - Forward via SCID: Route to next hop by resolving short channel ID
  - Forward with path key override: Handle concatenated blinded paths where the
    sender's path meets the receiver's path at an introduction node
  - Forward to unknown peer: Graceful handling when next hop is disconnected
  - Deliver action: Basic final hop handling (placeholder for future features)
  - Error scenarios: Invalid onion, unknown SCID, malformed route data

# Test Harness Architecture

The endpointHarness wires up all components needed for testing:
  - OnionEndpoint: The component under test
  - sphinx.Router: Processes onion packets and decrypts blinded hop data
  - actor.ActorSystem: Manages peer actors that receive forwarded messages
  - subscribe.Server: Publishes OnionMessageUpdate events
  - mockNodeIDResolver: Resolves SCIDs to node pubkeys

# Adding New Test Scenarios

Tests use a table-driven pattern with buildHopsFunc functions that return:
  - blindedPath: The sphinx.BlindedPathInfo for building the onion message
  - privKeys: Keys for each hop (used to verify onion can be peeled)
  - after: Validation function called after SendMessage (e.g., verify forward)

To add a new scenario:
 1. Create a buildXxxHops function following the existing patterns
 2. Add an entry to the tests slice in TestOnionEndpointSendMessageRouting
 3. Use finalPayload for delivery scenarios that need custom TLV data

# Future Extensions

When full onion messaging support is added, this harness can be extended for:
  - Sending onion messages: Test outbound message construction and routing
  - Receiving messages: Test request/response handling
  - Reply paths: Test bidirectional communication flows
  - BOLT12 offers: Test offer creation and invoice fetching
*/

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/onionmessage/testhelpers"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
)

// endpointHarness wires up the minimal components required to exercise
// SendMessage end-to-end.
type endpointHarness struct {
	endpoint     *OnionEndpoint
	server       *subscribe.Server
	client       *subscribe.Client
	actorSystem  *actor.ActorSystem
	receptionist *actor.Receptionist
	resolver     *mockNodeIDResolver
	router       *sphinx.Router
	nodeKey      *btcec.PrivateKey
}

func newEndpointHarness(t *testing.T) *endpointHarness {
	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: nodeKey},
		sphinx.NewMemoryReplayLog(),
	)
	require.NoError(t, router.Start())

	server := subscribe.NewServer()
	require.NoError(t, server.Start())

	client, err := server.Subscribe()
	require.NoError(t, err)

	actorSystem := actor.NewActorSystem()
	receptionist := actorSystem.Receptionist()
	resolver := newMockNodeIDResolver()

	endpoint, err := NewOnionEndpoint(
		receptionist, router, resolver, WithMessageServer(server),
	)
	require.NoError(t, err)

	return &endpointHarness{
		endpoint:     endpoint,
		server:       server,
		client:       client,
		actorSystem:  actorSystem,
		receptionist: receptionist,
		resolver:     resolver,
		router:       router,
		nodeKey:      nodeKey,
	}
}

func (h *endpointHarness) cleanup() {
	h.client.Cancel()
	_ = h.server.Stop()
	h.router.Stop()
	_ = h.actorSystem.Shutdown()
}

func pubKeyToArray(pk *btcec.PublicKey) [33]byte {
	var out [33]byte
	copy(out[:], pk.SerializeCompressed())
	return out
}

func requireUpdate(t *testing.T, c *subscribe.Client) *OnionMessageUpdate {
	t.Helper()

	select {
	case raw := <-c.Updates():
		u, ok := raw.(*OnionMessageUpdate)
		require.True(t, ok, "unexpected update type")
		return u
	case <-time.After(time.Second):
		require.FailNow(t, "no update received")
		return nil
	}
}

func requireNoUpdate(t *testing.T, c *subscribe.Client) {
	t.Helper()

	select {
	case <-c.Updates():
		require.FailNow(t, "unexpected update received")
	case <-time.After(200 * time.Millisecond):
	}
}

// hopBuildResult encapsulates the outputs of a hop building function.
type hopBuildResult struct {
	blindedPath *sphinx.BlindedPathInfo
	privKeys    []*btcec.PrivateKey
	after       func()
}

// buildHopsFunc is the signature for functions that construct test hop data.
type buildHopsFunc func(t *testing.T, h *endpointHarness) hopBuildResult

// buildForwardNextNodeHops constructs hops for testing forward via next node.
func buildForwardNextNodeHops(t *testing.T, h *endpointHarness) hopBuildResult {
	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	forwarded := make(chan *lnwire.OnionMessage, 1)
	SpawnOnionPeerActor(
		h.actorSystem,
		func(msg *lnwire.OnionMessage) { forwarded <- msg },
		pubKeyToArray(nextNodePub),
	)

	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		nextNodePub,
	)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	plainA := testhelpers.EncodeBlindedRouteData(t, rdA)
	plainB := testhelpers.EncodeBlindedRouteData(t, rdB)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plainA},
		{NodePub: nextNodePub, PlainText: plainB},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	after := func() {
		select {
		case forwardedMsg := <-forwarded:
			require.NotNil(t, forwardedMsg)
		case <-time.After(time.Second):
			require.FailNow(t, "forwarded message not received")
		}
	}

	return hopBuildResult{
		blindedPath: testhelpers.BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildForwardSCIDHops constructs hops for testing forward via SCID.
func buildForwardSCIDHops(t *testing.T, h *endpointHarness) hopBuildResult {
	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	scid := lnwire.NewShortChanIDFromInt(555)
	h.resolver.addPeer(scid, nextNodePub)

	forwarded := make(chan *lnwire.OnionMessage, 1)
	SpawnOnionPeerActor(
		h.actorSystem,
		func(msg *lnwire.OnionMessage) { forwarded <- msg },
		pubKeyToArray(nextNodePub),
	)

	nextNode := fn.NewRight[*btcec.PublicKey](scid)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	plainA := testhelpers.EncodeBlindedRouteData(t, rdA)
	plainB := testhelpers.EncodeBlindedRouteData(t, rdB)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plainA},
		{NodePub: nextNodePub, PlainText: plainB},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	after := func() {
		select {
		case forwardedMsg := <-forwarded:
			require.NotNil(t, forwardedMsg)
		case <-time.After(time.Second):
			require.FailNow(t, "forwarded message not received")
		}
	}

	return hopBuildResult{
		blindedPath: testhelpers.BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildDeliverHops constructs hops for testing the deliver action.
func buildDeliverHops(t *testing.T, h *endpointHarness) hopBuildResult {
	rd := &record.BlindedRouteData{}
	plain := testhelpers.EncodeBlindedRouteData(t, rd)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plain},
	}
	privKeys := []*btcec.PrivateKey{h.nodeKey}

	return hopBuildResult{
		blindedPath: testhelpers.BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       func() {},
	}
}

// buildForwardUnknownPeerHops constructs hops for testing forward to an
// unknown peer. The next node has no actor registered, so forwarding will
// fail but the message is still processed.
func buildForwardUnknownPeerHops(
	t *testing.T, h *endpointHarness) hopBuildResult {

	// Generate a key for the "next node" but do NOT spawn an actor for it.
	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	// Build a forwarding path where the next hop has no actor.
	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		nextNodePub,
	)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub:   h.nodeKey.PubKey(),
			PlainText: testhelpers.EncodeBlindedRouteData(t, rdA),
		},
		{
			NodePub:   nextNodePub,
			PlainText: testhelpers.EncodeBlindedRouteData(t, rdB),
		},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	// Create a channel to verify no message is forwarded.
	forwarded := make(chan *lnwire.OnionMessage, 1)

	after := func() {
		select {
		case <-forwarded:
			require.FailNow(t, "message should not have been "+
				"forwarded to unknown peer")
		case <-time.After(200 * time.Millisecond):
			// Expected: no forwarding happened.
		}
	}

	return hopBuildResult{
		blindedPath: testhelpers.BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildConcatenatedPathHops constructs a concatenated blinded path scenario.
// This tests the common case where a receiver provides a blinded path starting
// at an introduction node, and the sender prepends their own path to reach that
// introduction node. The sender's last hop payload includes a
// NextBlindingOverride pointing to the receiver's blinding point.
//
// Note: The sender creates the onion but is NOT part of the blinded path.
// The first node to peel the onion is firstHopNode (h.nodeKey in this test).
//
// Path structure (from the sender's perspective):
//
//	Sender (creates onion, not in path)
//	  └─> firstHopNode ─> introNode ─> finalNode
//
//	Sender's blinded path:   [firstHopNode] (with NextBlindingOverride)
//	Receiver's blinded path: [introNode, finalNode]
//	Concatenated:            [firstHopNode, introNode, finalNode]
//
// The firstHopNode (h.nodeKey) peels the first layer, sees the forward to
// introNode with the NextBlindingOverride, and forwards with the new path key.
func buildConcatenatedPathHops(
	t *testing.T, h *endpointHarness) hopBuildResult {

	// Generate keys for the path nodes.
	introNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	introNodePub := introNodeKey.PubKey()

	finalNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	finalNodePub := finalNodeKey.PubKey()

	// Spawn actor for the introduction node to receive forwarded messages.
	forwarded := make(chan *lnwire.OnionMessage, 1)
	SpawnOnionPeerActor(
		h.actorSystem,
		func(msg *lnwire.OnionMessage) { forwarded <- msg },
		pubKeyToArray(introNodePub),
	)

	// Build the receiver's blinded path: introNode -> finalNode.
	// This is the path the receiver creates and shares with the sender.
	nextNodeReceiver := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		finalNodePub,
	)
	rdReceiverIntro := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNodeReceiver, nil, nil,
	)
	rdReceiverFinal := &record.BlindedRouteData{}

	receiverHops := []*sphinx.HopInfo{
		{
			NodePub: introNodePub,
			PlainText: testhelpers.EncodeBlindedRouteData(
				t, rdReceiverIntro,
			),
		},
		{
			NodePub: finalNodePub,
			PlainText: testhelpers.EncodeBlindedRouteData(
				t, rdReceiverFinal,
			),
		},
	}
	receiverPath := testhelpers.BuildBlindedPath(t, receiverHops)

	// Build the sender's path: firstHopNode -> introNode.
	// The payload for firstHopNode includes NextBlindingOverride pointing
	// to the receiver's blinding point.
	nextNodeSender := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		introNodePub,
	)
	blindingOverride := receiverPath.Path.BlindingPoint
	rdFirstHop := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNodeSender, blindingOverride, nil,
	)

	senderHops := []*sphinx.HopInfo{
		{
			NodePub: h.nodeKey.PubKey(),
			PlainText: testhelpers.EncodeBlindedRouteData(
				t, rdFirstHop,
			),
		},
	}
	senderPath := testhelpers.BuildBlindedPath(t, senderHops)

	// Concatenate the two paths. The sender's path ends at introNode,
	// and receiver's path starts at introNode.
	concatenatedPath := testhelpers.ConcatBlindedPaths(
		t, senderPath, receiverPath,
	)

	// Keys for peeling: firstHopNode, introNode, finalNode.
	privKeys := []*btcec.PrivateKey{h.nodeKey, introNodeKey, finalNodeKey}

	// Capture the expected path key for validation in after().
	expectedPathKey := blindingOverride

	after := func() {
		select {
		case forwardedMsg := <-forwarded:
			require.NotNil(t, forwardedMsg)

			// Verify the forwarded message uses the receiver's
			// blinding point as the new path key (from the
			// NextBlindingOverride).
			require.Equal(
				t, expectedPathKey, forwardedMsg.PathKey,
				"forwarded message should use override path "+
					"key",
			)
		case <-time.After(time.Second):
			require.FailNow(t, "forwarded message not received")
		}
	}

	return hopBuildResult{
		blindedPath: concatenatedPath,
		privKeys:    privKeys,
		after:       after,
	}
}

func TestOnionEndpointSendMessageRouting(t *testing.T) {
	t.Parallel()

	customTLVType := lnwire.InvoiceRequestNamespaceType + 1

	tests := []struct {
		name         string
		buildHops    buildHopsFunc
		finalPayload []*lnwire.FinalHopPayload
	}{
		{
			name:      "forward next node",
			buildHops: buildForwardNextNodeHops,
		},
		{
			name:      "forward scid",
			buildHops: buildForwardSCIDHops,
		},
		{
			name:      "deliver",
			buildHops: buildDeliverHops,
			finalPayload: []*lnwire.FinalHopPayload{
				{
					TLVType: customTLVType,
					Value:   []byte{1, 2, 3},
				},
			},
		},
		{
			name:      "forward concatenated path",
			buildHops: buildConcatenatedPathHops,
		},
		{
			name:      "forward unknown peer",
			buildHops: buildForwardUnknownPeerHops,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEndpointHarness(t)
			defer h.cleanup()

			result := tc.buildHops(t, h)
			onionMsg, cipherTexts := testhelpers.BuildOnionMessage(
				t, result.blindedPath, tc.finalPayload,
			)

			// The introduction point is the first hop that will
			// process this onion message.
			introPoint := result.blindedPath.Path.IntroductionPoint

			msg := msgmux.PeerMsg{
				Message: onionMsg,
				PeerPub: *introPoint,
			}

			ctx := context.Background()
			require.True(t, h.endpoint.SendMessage(ctx, msg))

			update := requireUpdate(t, h.client)
			require.Equal(
				t, pubKeyToArray(introPoint), update.Peer,
			)
			require.Equal(t, onionMsg.OnionBlob, update.OnionBlob)
			expectedData := cipherTexts[0]
			require.Equal(
				t, expectedData, update.EncryptedRecipientData,
			)

			peeled := testhelpers.PeelOnionLayers(
				t, result.privKeys, onionMsg,
			)
			require.Len(t, peeled, len(cipherTexts))
			for i := range peeled {
				require.Equal(
					t, cipherTexts[i],
					peeled[i].EncryptedData,
				)
			}

			for _, fp := range tc.finalPayload {
				TLVType := fp.TLVType
				require.Equal(
					t, fp.Value,
					update.CustomRecords[uint64(TLVType)],
				)
			}

			result.after()
		})
	}
}

func TestOnionEndpointSendMessageProcessError(t *testing.T) {
	h := newEndpointHarness(t)
	defer h.cleanup()

	// Provide an invalid onion blob so processOnionMessage fails to decode.
	onionMsg := &lnwire.OnionMessage{
		PathKey:   h.nodeKey.PubKey(),
		OnionBlob: []byte{1, 2, 3},
	}

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *h.nodeKey.PubKey(),
	}

	require.False(t, h.endpoint.SendMessage(context.Background(), msg))
	requireNoUpdate(t, h.client)
}

// TestOnionEndpointForwardSCIDResolutionFailure tests that forwarding via SCID
// fails gracefully when the SCID cannot be resolved to a node ID.
func TestOnionEndpointForwardSCIDResolutionFailure(t *testing.T) {
	t.Parallel()

	h := newEndpointHarness(t)
	defer h.cleanup()

	// Generate a key for the "next node".
	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	// Use an SCID that is NOT registered in the resolver.
	unknownSCID := lnwire.NewShortChanIDFromInt(99999)

	// Build a forwarding path using the unknown SCID.
	nextNode := fn.NewRight[*btcec.PublicKey](unknownSCID)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{
			NodePub:   h.nodeKey.PubKey(),
			PlainText: testhelpers.EncodeBlindedRouteData(t, rdA),
		},
		{
			NodePub:   nextNodePub,
			PlainText: testhelpers.EncodeBlindedRouteData(t, rdB),
		},
	}

	blindedPath := testhelpers.BuildBlindedPath(t, hops)
	onionMsg, _ := testhelpers.BuildOnionMessage(t, blindedPath, nil)

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *h.nodeKey.PubKey(),
	}

	// The SCID resolution fails during processOnionMessage, so SendMessage
	// returns false.
	ctx := context.Background()
	require.False(t, h.endpoint.SendMessage(ctx, msg))
	requireNoUpdate(t, h.client)
}

// TestOnionEndpointInvalidBlindedRouteData tests that an invalid/malformed
// BlindedRouteData causes processing to fail gracefully.
func TestOnionEndpointInvalidBlindedRouteData(t *testing.T) {
	t.Parallel()

	h := newEndpointHarness(t)
	defer h.cleanup()

	// Build a single-hop path with garbage plaintext that cannot be decoded
	// as valid BlindedRouteData.
	hops := []*sphinx.HopInfo{
		{
			NodePub: h.nodeKey.PubKey(),
			// Invalid TLV data - not a valid BlindedRouteData
			// encoding.
			PlainText: []byte{0xFF, 0xFF, 0xFF, 0xFF},
		},
	}

	blindedPath := testhelpers.BuildBlindedPath(t, hops)
	onionMsg, _ := testhelpers.BuildOnionMessage(t, blindedPath, nil)

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *h.nodeKey.PubKey(),
	}

	// The BlindedRouteData decoding fails, so SendMessage returns false.
	ctx := context.Background()
	require.False(t, h.endpoint.SendMessage(ctx, msg))
	requireNoUpdate(t, h.client)
}

func TestOnionEndpointCanHandle(t *testing.T) {
	t.Parallel()

	endpoint := &OnionEndpoint{}

	t.Run("onion message", func(t *testing.T) {
		t.Parallel()

		msg := msgmux.PeerMsg{Message: &lnwire.OnionMessage{}}
		require.True(t, endpoint.CanHandle(msg))
	})

	t.Run("non onion message", func(t *testing.T) {
		t.Parallel()

		msg := msgmux.PeerMsg{Message: &lnwire.Ping{}}
		require.False(t, endpoint.CanHandle(msg))
	})
}
