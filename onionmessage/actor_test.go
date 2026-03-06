package onionmessage

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// mockPeerMessageSender implements PeerMessageSender for testing.
type mockPeerMessageSender struct {
	sent chan peerMessage
	err  error
}

type peerMessage struct {
	pubKey [33]byte
	msg    *lnwire.OnionMessage
}

func newMockPeerMessageSender() *mockPeerMessageSender {
	return &mockPeerMessageSender{
		sent: make(chan peerMessage, 1),
	}
}

func (m *mockPeerMessageSender) SendToPeer(pubKey [33]byte,
	msg *lnwire.OnionMessage) error {

	if m.err != nil {
		return m.err
	}

	m.sent <- peerMessage{pubKey: pubKey, msg: msg}

	return nil
}

// mockUpdateDispatcher implements OnionMessageUpdateDispatcher for testing.
type mockUpdateDispatcher struct {
	updates chan *OnionMessageUpdate
	err     error
}

func newMockUpdateDispatcher() *mockUpdateDispatcher {
	return &mockUpdateDispatcher{
		updates: make(chan *OnionMessageUpdate, 1),
	}
}

func (m *mockUpdateDispatcher) SendUpdate(update any) error {
	if m.err != nil {
		return m.err
	}

	u, ok := update.(*OnionMessageUpdate)
	if !ok {
		return fmt.Errorf("unexpected update type: %T", update)
	}

	m.updates <- u

	return nil
}

// actorHarness wires up the minimal components required to exercise
// OnionPeerActor.Receive end-to-end.
type actorHarness struct {
	actor      *OnionPeerActor
	sender     *mockPeerMessageSender
	dispatcher *mockUpdateDispatcher
	resolver   *mockNodeIDResolver
	router     *sphinx.Router
	nodeKey    *btcec.PrivateKey
}

func newActorHarness(t *testing.T) *actorHarness {
	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: nodeKey},
		sphinx.NewNoOpReplayLog(),
	)
	require.NoError(t, router.Start())
	t.Cleanup(func() { router.Stop() })

	sender := newMockPeerMessageSender()
	dispatcher := newMockUpdateDispatcher()
	resolver := newMockNodeIDResolver()

	var peerPubKey [33]byte
	copy(peerPubKey[:], nodeKey.PubKey().SerializeCompressed())

	peerActor := &OnionPeerActor{
		peerPubKey:       peerPubKey,
		peerSender:       sender,
		router:           router,
		resolver:         resolver,
		updateDispatcher: dispatcher,
	}

	return &actorHarness{
		actor:      peerActor,
		sender:     sender,
		dispatcher: dispatcher,
		resolver:   resolver,
		router:     router,
		nodeKey:    nodeKey,
	}
}

func pubKeyToArray(pk *btcec.PublicKey) [33]byte {
	var out [33]byte
	copy(out[:], pk.SerializeCompressed())
	return out
}

// hopBuildResult encapsulates the outputs of a hop building function.
type hopBuildResult struct {
	blindedPath *sphinx.BlindedPathInfo
	privKeys    []*btcec.PrivateKey
	after       func()
}

// buildHopsFunc is the signature for functions that construct test hop data.
type buildHopsFunc func(t *testing.T, h *actorHarness) hopBuildResult

// buildForwardNextNodeHops constructs hops for testing forward via next node.
func buildForwardNextNodeHops(
	t *testing.T, h *actorHarness) hopBuildResult {

	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		nextNodePub,
	)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	plainA := EncodeBlindedRouteData(t, rdA)
	plainB := EncodeBlindedRouteData(t, rdB)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plainA},
		{NodePub: nextNodePub, PlainText: plainB},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	after := func() {
		select {
		case msg := <-h.sender.sent:
			require.NotNil(t, msg.msg)
			require.Equal(
				t, pubKeyToArray(nextNodePub), msg.pubKey,
			)
		default:
			require.FailNow(t, "forwarded message not sent")
		}
	}

	return hopBuildResult{
		blindedPath: BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildForwardSCIDHops constructs hops for testing forward via SCID.
func buildForwardSCIDHops(t *testing.T, h *actorHarness) hopBuildResult {
	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	scid := lnwire.NewShortChanIDFromInt(555)
	h.resolver.addPeer(scid, nextNodePub)

	nextNode := fn.NewRight[*btcec.PublicKey](scid)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	plainA := EncodeBlindedRouteData(t, rdA)
	plainB := EncodeBlindedRouteData(t, rdB)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plainA},
		{NodePub: nextNodePub, PlainText: plainB},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	after := func() {
		select {
		case msg := <-h.sender.sent:
			require.NotNil(t, msg.msg)
		default:
			require.FailNow(t, "forwarded message not sent")
		}
	}

	return hopBuildResult{
		blindedPath: BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildDeliverHops constructs hops for testing the deliver action.
func buildDeliverHops(t *testing.T, h *actorHarness) hopBuildResult {
	rd := &record.BlindedRouteData{}
	plain := EncodeBlindedRouteData(t, rd)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plain},
	}
	privKeys := []*btcec.PrivateKey{h.nodeKey}

	return hopBuildResult{
		blindedPath: BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       func() {},
	}
}

// buildForwardUnknownPeerHops constructs hops for testing forward to an
// unknown peer. The sender returns an error, so forwarding will fail but the
// message is still processed.
func buildForwardUnknownPeerHops(
	t *testing.T, h *actorHarness) hopBuildResult {

	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	// Set up the sender to return an error for the unknown peer.
	h.sender.err = fmt.Errorf("peer not connected")

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
			PlainText: EncodeBlindedRouteData(t, rdA),
		},
		{
			NodePub:   nextNodePub,
			PlainText: EncodeBlindedRouteData(t, rdB),
		},
	}

	privKeys := []*btcec.PrivateKey{h.nodeKey, nextNodeKey}

	after := func() {
		// Verify no message was successfully sent.
		select {
		case <-h.sender.sent:
			require.FailNow(t, "message should not have been "+
				"forwarded to unknown peer")
		default:
			// Expected: no forwarding happened.
		}
	}

	return hopBuildResult{
		blindedPath: BuildBlindedPath(t, hops),
		privKeys:    privKeys,
		after:       after,
	}
}

// buildConcatenatedPathHops constructs a concatenated blinded path scenario.
func buildConcatenatedPathHops(
	t *testing.T, h *actorHarness) hopBuildResult {

	introNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	introNodePub := introNodeKey.PubKey()

	finalNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	finalNodePub := finalNodeKey.PubKey()

	// Build the receiver's blinded path: introNode -> finalNode.
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
			PlainText: EncodeBlindedRouteData(
				t, rdReceiverIntro,
			),
		},
		{
			NodePub: finalNodePub,
			PlainText: EncodeBlindedRouteData(
				t, rdReceiverFinal,
			),
		},
	}
	receiverPath := BuildBlindedPath(t, receiverHops)

	// Build the sender's path: firstHopNode -> introNode.
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
			PlainText: EncodeBlindedRouteData(
				t, rdFirstHop,
			),
		},
	}
	senderPath := BuildBlindedPath(t, senderHops)

	concatenatedPath := ConcatBlindedPaths(
		t, senderPath, receiverPath,
	)

	privKeys := []*btcec.PrivateKey{h.nodeKey, introNodeKey, finalNodeKey}

	expectedPathKey := blindingOverride

	after := func() {
		select {
		case msg := <-h.sender.sent:
			require.NotNil(t, msg.msg)

			// Verify the forwarded message uses the receiver's
			// blinding point as the new path key.
			require.Equal(
				t, expectedPathKey, msg.msg.PathKey,
				"forwarded message should use override "+
					"path key",
			)
		default:
			require.FailNow(t, "forwarded message not sent")
		}
	}

	return hopBuildResult{
		blindedPath: concatenatedPath,
		privKeys:    privKeys,
		after:       after,
	}
}

// TestOnionPeerActorRouting tests the OnionPeerActor's message routing
// functionality across various scenarios including forwarding via next node ID,
// forwarding via SCID, delivery, concatenated paths, and unknown peer handling.
func TestOnionPeerActorRouting(t *testing.T) {
	t.Parallel()

	customTLVType := lnwire.InvoiceRequestNamespaceType + 1

	tests := []struct {
		name         string
		buildHops    buildHopsFunc
		finalHopTLVs []*lnwire.FinalHopTLV
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
			finalHopTLVs: []*lnwire.FinalHopTLV{
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
			h := newActorHarness(t)

			result := tc.buildHops(t, h)
			onionMsg, cipherTexts := BuildOnionMessage(
				t, result.blindedPath, tc.finalHopTLVs,
			)

			req := &Request{msg: *onionMsg}
			actorResult := h.actor.Receive(t.Context(), req)
			require.True(t, actorResult.IsOk())

			// Verify the update was dispatched.
			select {
			case update := <-h.dispatcher.updates:
				require.Equal(
					t, h.actor.peerPubKey,
					update.Peer,
				)
				require.Equal(
					t, onionMsg.OnionBlob,
					update.OnionBlob,
				)
				expectedData := cipherTexts[0]
				require.Equal(
					t, expectedData,
					update.EncryptedRecipientData,
				)

				for _, fht := range tc.finalHopTLVs {
					tlvType := fht.TLVType
					require.Equal(
						t, fht.Value,
						update.CustomRecords[uint64(
							tlvType,
						)],
					)
				}
			default:
				require.FailNow(t, "no update dispatched")
			}

			peeled := PeelOnionLayers(
				t, result.privKeys, onionMsg,
			)
			require.Len(t, peeled, len(cipherTexts))
			for i := range peeled {
				require.Equal(
					t, cipherTexts[i],
					peeled[i].EncryptedData,
				)
			}

			result.after()
		})
	}
}

// TestOnionPeerActorReceiveContextCanceled tests that OnionPeerActor.Receive
// returns an error when the context is canceled.
func TestOnionPeerActorReceiveContextCanceled(t *testing.T) {
	t.Parallel()

	h := newActorHarness(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := &Request{}

	result := h.actor.Receive(ctx, req)

	require.True(t, result.IsErr())
	result.WhenErr(func(err error) {
		require.ErrorIs(t, err, ErrActorShuttingDown)
	})
}

// TestOnionPeerActorReceiveInvalidOnionBlob verifies that processing fails
// gracefully when provided with an invalid onion blob that cannot be decoded.
func TestOnionPeerActorReceiveInvalidOnionBlob(t *testing.T) {
	t.Parallel()

	h := newActorHarness(t)

	onionMsg := lnwire.OnionMessage{
		PathKey:   h.nodeKey.PubKey(),
		OnionBlob: []byte{1, 2, 3},
	}

	req := &Request{msg: onionMsg}

	result := h.actor.Receive(t.Context(), req)
	require.True(t, result.IsErr())

	// Verify no update was dispatched.
	select {
	case <-h.dispatcher.updates:
		require.FailNow(t, "unexpected update dispatched")
	default:
	}
}

// TestOnionPeerActorReceiveDispatcherError verifies that the actor returns an
// error when the update dispatcher fails.
func TestOnionPeerActorReceiveDispatcherError(t *testing.T) {
	t.Parallel()

	h := newActorHarness(t)
	h.dispatcher.err = fmt.Errorf("dispatcher error")

	rd := &record.BlindedRouteData{}
	plain := EncodeBlindedRouteData(t, rd)
	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: plain},
	}

	blindedPath := BuildBlindedPath(t, hops)
	onionMsg, _ := BuildOnionMessage(t, blindedPath, nil)

	req := &Request{msg: *onionMsg}
	result := h.actor.Receive(t.Context(), req)
	require.True(t, result.IsErr())
}
