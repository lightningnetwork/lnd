package peer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntest/wait"

	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lntest/channels"

	"github.com/stretchr/testify/require"

	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/routing"
)

const defaultTimeout = 30 * time.Second

var (
	testAddr1 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}

	testAddr2 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9001}

	testAddr3 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9002}
)

// TestPersistentPeerManager tests that the PersistentPeerManager correctly
// manages the connection requests for persistent peers and is able to update
// connection requests accordingly when a peer announces new addresses.
func TestPersistentPeerManager(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, alicePubKey := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.AlicesPrivKey,
	)
	alicePubKeyStr := string(alicePubKey.SerializeCompressed())

	_, bobPubKey := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.AlicesPrivKey,
	)
	bobPubKeyStr := string(bobPubKey.SerializeCompressed())

	// Create and start a new connection manager.
	connMgr, err := connmgr.New(&connmgr.Config{
		Dial: func(addr net.Addr) (net.Conn, error) {
			return nil, nil
		},
	})
	require.NoError(t, err)

	connMgr.Start()
	defer connMgr.Stop()

	// Wrap the connection manager in a mockConnMgr so that we can keep
	// track of calls to the connection manager.
	cm := &mockConnMgr{
		reqs: make(map[uint64]*connmgr.ConnReq),
		cm:   connMgr,
	}

	// assertOnceConnReqPerAddress is a helper closure that ensures that the
	// connection manager has one connection request for each address given.
	assertOnceConnReqPerAddress := func(addrs ...net.Addr) {
		err = wait.Predicate(func() bool {
			for _, addr := range addrs {
				if cm.NumConnReqs(addr) != 1 {
					return false
				}
			}
			return true
		}, defaultTimeout)
		require.NoError(t, err)
	}

	// assertNoConnReqs is a helper closure that ensures that the
	// connection manager has no connection requests for any of the given
	// addresses.
	assertNoConnReqs := func(addrs ...net.Addr) {
		err = wait.Predicate(func() bool {
			for _, addr := range addrs {
				if cm.NumConnReqs(addr) != 0 {
					return false
				}
			}
			return true
		}, defaultTimeout)
		require.NoError(t, err)
	}

	// topChangeChan is a channel that will be used to send network updates
	// on.
	topChangeChan := make(chan *routing.TopologyChange)

	// addUpdate is a closure helper used to send mock NodeAnnouncement
	// network updates.
	addUpdate := func(pubKey *btcec.PublicKey, addrs ...net.Addr) {
		topChangeChan <- &routing.TopologyChange{
			NodeUpdates: []*routing.NetworkNodeUpdate{
				{
					IdentityKey: pubKey,
					Addresses:   addrs,
				},
			},
		}
	}

	// updates will be used by the PersistentPeerManger to subscribe to
	// network updates from topChangeChan.
	updates := func() (*routing.TopologyClient, error) {
		return &routing.TopologyClient{
			TopologyChanges: topChangeChan,
			Cancel:          cancel,
		}, nil
	}

	// Create and start a new PersistentPeeManager.
	manager := NewPersistentPeerManager(cm, updates, 0, 0)
	err = manager.Start()
	require.NoError(t, err)
	defer manager.Stop()

	// Alice and Bob should not be persistent peers initially.
	require.False(t, manager.IsPersistentPeer(alicePubKeyStr))
	require.False(t, manager.IsPersistentPeer(bobPubKeyStr))

	// Register Alice as a persistent peer with an address.
	manager.AddPeer(alicePubKeyStr, []*lnwire.NetAddress{
		{
			IdentityKey: alicePubKey,
			Address:     testAddr1,
		},
	}, false)
	require.True(t, manager.IsPersistentPeer(alicePubKeyStr))

	// Calling ConnectPeer for Alice should result in a connection request
	// since we have added an address for Alice.
	manager.ConnectPeer(alicePubKeyStr)
	assertOnceConnReqPerAddress(testAddr1)
	require.Equal(t, 1, manager.NumConnReq(alicePubKeyStr))

	// Advertise the same address for Alice in a new NodeAnnouncement.
	addUpdate(alicePubKey, testAddr1)

	// Check that there is still only one connection request for Alice
	// since her address did not change.
	assertOnceConnReqPerAddress(testAddr1)
	require.Equal(t, 1, manager.NumConnReq(alicePubKeyStr))

	// Advertise new addresses for Alice, one being the same as what she
	// had before and the other being a new one.
	addUpdate(alicePubKey, testAddr1, testAddr2)

	// Check that there is now one connection request for each of Alice's
	// addresses.
	assertOnceConnReqPerAddress(testAddr1, testAddr2)
	require.Equal(t, 2, manager.NumConnReq(alicePubKeyStr))

	// Advertise new addresses for Alice. This announcement has one of the
	// same addresses as what she advertised before and is missing the
	// other.
	addUpdate(alicePubKey, testAddr2)

	// Check that there is no longer a connection request for the address
	// that was not in Alice's latest NodeAnnouncement and that there is
	// still a connection request for the other.
	assertNoConnReqs(testAddr1)
	assertOnceConnReqPerAddress(testAddr2)
	require.Equal(t, 1, manager.NumConnReq(alicePubKeyStr))

	// Once again advertise two addresses for Alice.
	addUpdate(alicePubKey, testAddr1, testAddr2)
	assertOnceConnReqPerAddress(testAddr1, testAddr2)

	// Get the id of the connection request associated with testAddr1
	// and remove all connection requests for Alice except for this one.
	idAddr1 := cm.GetID(testAddr1)
	require.False(t, idAddr1 == UnassignedConnID)
	manager.RemovePeerConns(alicePubKeyStr, &idAddr1)
	assertOnceConnReqPerAddress(testAddr1)
	assertNoConnReqs(testAddr2)

	// Remove all connection request for Alice.
	manager.RemovePeerConns(alicePubKeyStr, nil)

	// Check that there are no more connection requests for Alice.
	assertNoConnReqs(testAddr2)
	require.Equal(t, 0, manager.NumConnReq(alicePubKeyStr))

	// Advertise an update for Bob. This should not result in any new
	// connection requests since Bob is not a persistent peer.
	addUpdate(bobPubKey, testAddr3)
	assertNoConnReqs(testAddr3)
	require.Equal(t, 0, manager.NumConnReq(bobPubKeyStr))

	// Delete Alice from the persistent manager.
	manager.DelPeer(alicePubKeyStr)
	require.False(t, manager.IsPersistentPeer(alicePubKeyStr))

	// Send an update for Alice. This should result in no new connection
	// requests though since Alice is no longer a persistent peer.
	addUpdate(alicePubKey, testAddr2)
	assertNoConnReqs(testAddr2)
	require.Equal(t, 0, manager.NumConnReq(alicePubKeyStr))
}

type mockConnMgr struct {
	reqs map[uint64]*connmgr.ConnReq

	cm *connmgr.ConnManager
	sync.Mutex
}

func (m *mockConnMgr) NumConnReqs(addr fmt.Stringer) int {
	m.Lock()
	defer m.Unlock()

	count := 0
	for _, cr := range m.reqs {
		if cr.Addr.(*lnwire.NetAddress).Address.String() == addr.String() {
			count++
		}
	}

	return count
}

func (m *mockConnMgr) GetID(addr fmt.Stringer) uint64 {
	m.Lock()
	defer m.Unlock()

	for id, cr := range m.reqs {
		if cr.Addr.(*lnwire.NetAddress).Address.String() == addr.String() {
			return id
		}
	}

	return UnassignedConnID
}

func (m *mockConnMgr) Connect(c *connmgr.ConnReq) {
	m.Lock()
	defer m.Unlock()

	m.cm.Connect(c)
	m.reqs[c.ID()] = c
}

func (m *mockConnMgr) Remove(id uint64) {
	m.Lock()
	defer m.Unlock()

	m.cm.Remove(id)
	delete(m.reqs, id)
}

var _ connMgr = (*mockConnMgr)(nil)
