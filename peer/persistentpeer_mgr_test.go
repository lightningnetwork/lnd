package peer

import (
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	testAddr1 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}

	testAddr2 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9001}

	testAddr3 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9003}

	connReq1 = &connmgr.ConnReq{Addr: testAddr1}
	connReq2 = &connmgr.ConnReq{Addr: testAddr2}
	connReq3 = &connmgr.ConnReq{Addr: testAddr3}
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

	// Add an address for Bob.
	m.AddPeerAddresses(bobPubKey, &lnwire.NetAddress{
		IdentityKey: bobPubKey,
		Address:     testAddr1,
	})

	// Add another address for Bob.
	m.AddPeerAddresses(bobPubKey, &lnwire.NetAddress{
		IdentityKey: bobPubKey,
		Address:     testAddr2,
	})

	// Both addresses should appear in Bob's address list.
	addrs := m.GetPeerAddresses(bobPubKey)
	require.Len(t, addrs, 2)
	if addrs[0].Address.String() == testAddr1.String() {
		require.Equal(t, addrs[1].Address.String(), testAddr2.String())
	} else {
		require.Equal(t, addrs[0].Address.String(), testAddr2.String())
		require.Equal(t, addrs[1].Address.String(), testAddr1.String())
	}

	// If SetAddresses is used, however, then this should overwrite any
	// previous addresses stored for Bob.
	m.SetPeerAddresses(bobPubKey, &lnwire.NetAddress{
		IdentityKey: bobPubKey,
		Address:     testAddr3,
	})
	addrs = m.GetPeerAddresses(bobPubKey)
	require.Len(t, addrs, 1)
	require.Equal(t, addrs[0].Address.String(), testAddr3.String())

	// Add a connection request for Bob.
	m.AddPeerConnReq(bobPubKey, connReq1)
	require.Equal(t, m.NumPeerConnReqs(bobPubKey), 1)

	// Add another connection request for Bob.
	m.AddPeerConnReq(bobPubKey, connReq2)
	require.Equal(t, m.NumPeerConnReqs(bobPubKey), 2)

	// Both connection requests should appear in Bob's connection request
	// list.
	reqs := m.GetPeerConnReqs(bobPubKey)
	require.Len(t, reqs, 2)
	if reqs[0].String() == connReq1.String() {
		require.Equal(t, reqs[1].String(), connReq2.String())
	} else {
		require.Equal(t, reqs[0].String(), connReq2.String())
		require.Equal(t, reqs[1].String(), connReq1.String())
	}

	// If SetPeerConnReqs is used, however, then this should overwrite any
	// previously stored connection requests for Bob.
	m.SetPeerConnReqs(bobPubKey, connReq3)
	reqs = m.GetPeerConnReqs(bobPubKey)
	require.Equal(t, m.NumPeerConnReqs(bobPubKey), 1)
	require.Len(t, reqs, 1)
	require.Equal(t, reqs[0].String(), connReq3.String())

	// Delete Bob.
	m.DelPeer(bobPubKey)
	peers = m.PersistentPeers()
	require.Len(t, peers, 0)
}
