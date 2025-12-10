package onionmessage

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// mockNodeIDResolver implements NodeIDResolver for tests.
type mockNodeIDResolver struct {
	peers map[lnwire.ShortChannelID]*btcec.PublicKey
}

// addPeer registers a single SCID to pubkey mapping for tests.
func (m *mockNodeIDResolver) addPeer(scid lnwire.ShortChannelID,
	pubKey *btcec.PublicKey) {

	m.peers[scid] = pubKey
}

// newMockNodeIDResolver creates a new instance of mockNodeIDResolver.
func newMockNodeIDResolver() *mockNodeIDResolver {
	return &mockNodeIDResolver{
		peers: make(map[lnwire.ShortChannelID]*btcec.PublicKey),
	}
}

// RemotePubFromSCID resolves a node public key from a short channel ID.
func (m *mockNodeIDResolver) RemotePubFromSCID(
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	if pk, ok := m.peers[scid]; ok {
		return pk, nil
	}
	return nil, fmt.Errorf("unknown scid: %v", scid)
}

// buildOnionMessage builds an onion message and returns the message along with
// the ciphertext of the first blinded hop, mirroring how tests construct onion
// packets elsewhere.
func buildOnionMessage(t *testing.T, hops []*sphinx.HopInfo) (
	*lnwire.OnionMessage, []byte) {
	t.Helper()

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a BlindedPath from the list of HopInfo representing the
	// (unblinded) nodes in the blinded path. The first hop in paymentPath
	// is expected to be the introduction node.
	blindedPath, err := sphinx.BuildBlindedPath(sessionKey, hops)
	require.NoError(t, err)

	finalHopPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{1, 2, 3},
	}

	// Convert the blinded path to a sphinx path and add a final payload.
	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		blindedPath.Path, nil, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(t, err)

	onionSessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create an onion packet with no associated data.
	onionPkt, err := sphinx.NewOnionPacket(
		sphinxPath, onionSessionKey, nil,
		sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(t, err)

	// Encode the onion message packet.
	var buf bytes.Buffer
	require.NoError(t, onionPkt.Encode(&buf))

	onionMsg := &lnwire.OnionMessage{
		PathKey:   sessionKey.PubKey(),
		OnionBlob: buf.Bytes(),
	}

	return onionMsg, blindedPath.Path.BlindedHops[0].CipherText
}
