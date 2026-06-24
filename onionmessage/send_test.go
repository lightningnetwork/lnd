package onionmessage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// mockGraphSession wraps a mockNodeTraverser to satisfy GraphSessionProvider.
type mockGraphSession struct {
	traverser *mockNodeTraverser
}

// GraphSession provides a session for graph traversal.
func (m *mockGraphSession) GraphSession(_ context.Context,
	cb func(graphdb.NodeTraverser) error, _ func()) error {

	return cb(m.traverser)
}

// mockPeerSender captures the first hop and message passed to SendToPeer.
type mockPeerSender struct {
	firstHop [33]byte
	msg      *lnwire.OnionMessage
	err      error
}

// SendToPeer sends a message to a peer.
func (m *mockPeerSender) SendToPeer(pubKey [33]byte,
	msg *lnwire.OnionMessage) error {

	if m.err != nil {
		return m.err
	}

	m.firstHop = pubKey
	m.msg = msg

	return nil
}

// pubKeyToVertex converts a public key to a route.Vertex.
func pubKeyToVertex(pub *btcec.PublicKey) route.Vertex {
	var v route.Vertex
	copy(v[:], pub.SerializeCompressed())

	return v
}

// genKey is a test helper that generates a private key, failing the test on
// error.
func genKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	k, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return k
}

// TestSendDirectNeighbor tests that a message is sent correctly when the
// destination is a direct graph neighbor of the source.
func TestSendDirectNeighbor(t *testing.T) {
	t.Parallel()

	srcKey := genKey(t)
	destKey := genKey(t)

	srcVertex := pubKeyToVertex(srcKey.PubKey())
	destVertex := pubKeyToVertex(destKey.PubKey())

	graph := newMockNodeTraverser()
	graph.addNode(destVertex, onionFeatures())
	graph.addEdge(srcVertex, destVertex)

	sender := &mockPeerSender{}
	s := NewSender(&mockGraphSession{graph}, srcVertex, sender)

	err := s.Send(t.Context(), destVertex, nil, nil)
	require.NoError(t, err)

	// Message must be delivered to the destination directly.
	require.Equal(t, [33]byte(destVertex), sender.firstHop)
	require.NotNil(t, sender.msg)

	// Peel the single onion layer: destination should be the final hop.
	hops := PeelOnionLayers(t, []*btcec.PrivateKey{destKey}, sender.msg)
	require.Len(t, hops, 1)
	require.True(t, hops[0].IsFinal)
}

// TestSendMultiHop tests that a multi-hop path is built and delivered to the
// correct first hop, with each intermediate hop carrying the next node ID.
func TestSendMultiHop(t *testing.T) {
	t.Parallel()

	srcKey := genKey(t)
	aKey := genKey(t)
	destKey := genKey(t)

	srcVertex := pubKeyToVertex(srcKey.PubKey())
	aVertex := pubKeyToVertex(aKey.PubKey())
	destVertex := pubKeyToVertex(destKey.PubKey())

	graph := newMockNodeTraverser()
	graph.addNode(aVertex, onionFeatures())
	graph.addNode(destVertex, onionFeatures())
	graph.addEdge(srcVertex, aVertex)
	graph.addEdge(aVertex, destVertex)

	sender := &mockPeerSender{}
	s := NewSender(&mockGraphSession{graph}, srcVertex, sender)

	err := s.Send(t.Context(), destVertex, nil, nil)
	require.NoError(t, err)

	// Message must be delivered to the first hop (a).
	require.Equal(t, [33]byte(aVertex), sender.firstHop)
	require.NotNil(t, sender.msg)

	// Peel both onion layers and verify the route structure.
	hops := PeelOnionLayers(
		t, []*btcec.PrivateKey{aKey, destKey}, sender.msg,
	)
	require.Len(t, hops, 2)
	require.False(t, hops[0].IsFinal)
	require.True(t, hops[1].IsFinal)
}

// TestSendFallbackDirectPeer tests that when graph pathfinding yields
// ErrNoPathFound (destination present but unreachable), the sender falls back
// to a direct send to the destination peer.
func TestSendFallbackDirectPeer(t *testing.T) {
	t.Parallel()

	srcKey := genKey(t)
	destKey := genKey(t)

	srcVertex := pubKeyToVertex(srcKey.PubKey())
	destVertex := pubKeyToVertex(destKey.PubKey())

	// Destination supports onion messages but has no edges — BFS finds no
	// path.
	graph := newMockNodeTraverser()
	graph.addNode(destVertex, onionFeatures())

	sender := &mockPeerSender{}
	s := NewSender(&mockGraphSession{graph}, srcVertex, sender)

	err := s.Send(t.Context(), destVertex, nil, nil)
	require.NoError(t, err)

	// Fallback: message sent directly to destination.
	require.Equal(t, [33]byte(destVertex), sender.firstHop)
	require.NotNil(t, sender.msg)

	// Single final hop in the fallback onion.
	hops := PeelOnionLayers(t, []*btcec.PrivateKey{destKey}, sender.msg)
	require.Len(t, hops, 1)
	require.True(t, hops[0].IsFinal)
}

// TestSendDestNoOnionSupport tests that ErrDestinationNoOnionSupport is
// returned without attempting a fallback when the destination does not
// advertise onion message support.
func TestSendDestNoOnionSupport(t *testing.T) {
	t.Parallel()

	srcKey := genKey(t)
	destKey := genKey(t)

	srcVertex := pubKeyToVertex(srcKey.PubKey())
	destVertex := pubKeyToVertex(destKey.PubKey())

	graph := newMockNodeTraverser()
	graph.addNode(destVertex, noOnionFeatures())
	graph.addEdge(srcVertex, destVertex)

	sender := &mockPeerSender{}
	s := NewSender(&mockGraphSession{graph}, srcVertex, sender)

	err := s.Send(t.Context(), destVertex, nil, nil)
	require.ErrorIs(t, err, ErrDestinationNoOnionSupport)

	// No message should have been sent.
	require.Nil(t, sender.msg)
}

// TestSendToSelf tests that ErrCannotSendToSelf is returned when the
// destination equals this node's own pubkey.
func TestSendToSelf(t *testing.T) {
	t.Parallel()

	key := genKey(t)
	self := pubKeyToVertex(key.PubKey())

	s := NewSender(
		&mockGraphSession{newMockNodeTraverser()}, self,
		&mockPeerSender{},
	)

	err := s.Send(t.Context(), self, nil, nil)
	require.ErrorIs(t, err, ErrCannotSendToSelf)
}

// TestSendPeerNotConnected tests that when pathfinding yields ErrNoPathFound
// and the subsequent direct SendToPeer also fails, the error is returned.
func TestSendPeerNotConnected(t *testing.T) {
	t.Parallel()

	srcKey := genKey(t)
	destKey := genKey(t)

	srcVertex := pubKeyToVertex(srcKey.PubKey())
	destVertex := pubKeyToVertex(destKey.PubKey())

	// Destination has onion support but no edges: triggers direct fallback.
	graph := newMockNodeTraverser()
	graph.addNode(destVertex, onionFeatures())

	sendErr := errors.New("peer not connected")
	s := NewSender(
		&mockGraphSession{graph}, srcVertex,
		&mockPeerSender{err: sendErr},
	)

	err := s.Send(t.Context(), destVertex, nil, nil)
	require.ErrorIs(t, err, sendErr)
}

// TestBuildHopInfos verifies that intermediate hops carry the next node's
// pubkey in their route data and that the final hop carries empty route data.
func TestBuildHopInfos(t *testing.T) {
	t.Parallel()

	k1 := genKey(t)
	k2 := genKey(t)

	v1 := pubKeyToVertex(k1.PubKey())
	v2 := pubKeyToVertex(k2.PubKey())

	hopInfos, err := buildHopInfos([]route.Vertex{v1, v2})
	require.NoError(t, err)
	require.Len(t, hopInfos, 2)

	// Intermediate hop: next node ID must equal k2's pubkey.
	routeData0, err := record.DecodeBlindedRouteData(
		bytes.NewReader(hopInfos[0].PlainText),
	)
	require.NoError(t, err)

	nextNode, err := routeData0.NextNodeID.UnwrapOrErr(
		errors.New("expected next node ID on intermediate hop"),
	)
	require.NoError(t, err)
	require.Equal(t,
		k2.PubKey().SerializeCompressed(),
		nextNode.Val.SerializeCompressed(),
	)

	// Final hop: no next node ID, no SCID.
	routeData1, err := record.DecodeBlindedRouteData(
		bytes.NewReader(hopInfos[1].PlainText),
	)
	require.NoError(t, err)
	require.True(t, routeData1.NextNodeID.IsNone())
	require.True(t, routeData1.ShortChannelID.IsNone())
}
