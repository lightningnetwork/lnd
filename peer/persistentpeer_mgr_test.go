package peer

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/stretchr/testify/require"
)

// TestPersistentPeerManager tests that the PersistentPeerManager correctly
// manages the persistent peers.
func TestPersistentPeerManager(t *testing.T) {
	_, alicePubKey := btcec.PrivKeyFromBytes(channels.AlicesPrivKey)
	_, bobPubKey := btcec.PrivKeyFromBytes(channels.BobsPrivKey)

	m := NewPersistentPeerManager()

	// Alice should not initially be a persistent peer.
	require.False(t, m.IsPersistentPeer(alicePubKey))

	// Now add Alice as a non-permanent persistent peer.
	m.AddPeer(alicePubKey, false)
	require.True(t, m.IsPersistentPeer(alicePubKey))
	require.True(t, m.IsNonPermPersistentPeer(alicePubKey))

	// Bob should not yet be a persistent peer.
	require.False(t, m.IsPersistentPeer(bobPubKey))

	// Now add Bob as a permanent persistent peer.
	m.AddPeer(bobPubKey, true)
	require.True(t, m.IsPersistentPeer(bobPubKey))
	require.False(t, m.IsNonPermPersistentPeer(bobPubKey))

	// Both Alice and Bob should be listed as persistent peers.
	peers := m.PersistentPeers()
	require.Len(t, peers, 2)

	if peers[0].IsEqual(alicePubKey) {
		require.True(t, peers[1].IsEqual(bobPubKey))
	} else {
		require.True(t, peers[0].IsEqual(bobPubKey))
		require.True(t, peers[1].IsEqual(alicePubKey))
	}

	// Delete Alice.
	m.DelPeer(alicePubKey)
	require.False(t, m.IsPersistentPeer(alicePubKey))

	peers = m.PersistentPeers()
	require.Len(t, peers, 1)
	require.True(t, peers[0].IsEqual(bobPubKey))

	// Delete Bob.
	m.DelPeer(bobPubKey)
	peers = m.PersistentPeers()
	require.Len(t, peers, 0)
}
