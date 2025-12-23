package onionmessage

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
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
