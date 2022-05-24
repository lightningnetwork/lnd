package peer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const defaultTimeout = 30 * time.Second

var (
	testAddr1 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}

	testAddr2 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9001}

	testAddr3 = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9003}
)

// TestPersistentPeerManagerBasics tests the basic getters and setters of the
// PersistentPeerManager.
func TestPersistentPeerManagerBasics(t *testing.T) {
	_, alicePubKey := btcec.PrivKeyFromBytes(channels.AlicesPrivKey)
	_, bobPubKey := btcec.PrivKeyFromBytes(channels.BobsPrivKey)

	m := NewPersistentPeerManager(&PersistentPeerMgrConfig{
		MinBackoff: time.Millisecond * 10,
		MaxBackoff: time.Millisecond * 100,
		AddrTypeIsSupported: func(addr net.Addr) bool {
			return true
		},
		FetchNodeAdvertisedAddrs: func(vertex route.Vertex) ([]net.Addr,
			error) {

			return nil, nil
		},
	})
	defer m.Stop()

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

// TestRetryCanceller tests that the peer retry canceller channel logic works
// as expected.
func TestRetryCanceller(t *testing.T) {
	m := NewPersistentPeerManager(&PersistentPeerMgrConfig{
		MinBackoff: time.Millisecond * 10,
		MaxBackoff: time.Millisecond * 100,
		FetchNodeAdvertisedAddrs: func(_ route.Vertex) ([]net.Addr,
			error) {

			return nil, nil
		},
	})
	defer m.Stop()

	_, alicePubKey := btcec.PrivKeyFromBytes(channels.AlicesPrivKey)
	m.AddPeer(alicePubKey, false)

	rc := m.conns[route.NewVertex(alicePubKey)].getRetryCanceller()

	var wg sync.WaitGroup

	// retryFunction represents a function that should be canceled if the
	// retry canceller channel is closed.
	retryFunction := func() {
		wg.Add(1)
		defer wg.Done()
		select {
		case <-time.After(time.Second * 10):
			t.Fatalf("retry canceller was not cancelled")
		case <-rc:
		}
	}

	// Spin off a few retry functions.
	go retryFunction()
	go retryFunction()
	go retryFunction()

	// Cancel the retries. This should cause all the retry functions to
	// exit.
	m.conns[route.NewVertex(alicePubKey)].cancelRetries()
	wg.Wait()

	// Calling cancel again should not cause any closing-of-nil-channel
	// panics.
	m.conns[route.NewVertex(alicePubKey)].cancelRetries()
}

// TestConnectionLogic tests that the PersistentPeerManager's correctly adds
// and removes connection requests.
func TestConnectionLogic(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// Create and a new mock connection manager.
	cm := newMockConnMgr(t)
	defer cm.stop()

	_, alicePubKey := btcec.PrivKeyFromBytes(channels.AlicesPrivKey)

	// Create a new PersistentPeerManager.
	m := NewPersistentPeerManager(&PersistentPeerMgrConfig{
		ConnMgr:           cm,
		SubscribeTopology: updates,
		ChainNet:          wire.MainNet,
		MinBackoff:        time.Millisecond * 10,
		MaxBackoff:        time.Millisecond * 100,
		AddrTypeIsSupported: func(addr net.Addr) bool {
			return true
		},
		FetchNodeAdvertisedAddrs: func(vertex route.Vertex) ([]net.Addr,
			error) {

			switch vertex {
			case route.NewVertex(alicePubKey):
				return []net.Addr{testAddr1}, nil
			default:
				return nil, fmt.Errorf("unknown node")
			}
		},
	})
	require.NoError(t, m.Start())
	defer m.Stop()

	// Add Alice as a persistent peer.
	m.AddPeer(alicePubKey, false)

	// Since an address from FetchNodeAdvertisedAddrs would have been found
	// for Alice, calling ConnectPeer should result in a connection request
	// for that address.
	m.ConnectPeer(alicePubKey)
	assertOneConnReqPerAddress(t, cm, testAddr1)

	// Advertise the same address for Alice in a new NodeAnnouncement.
	// There should still only be one connection request for this address.
	addUpdate(alicePubKey, testAddr1)
	assertOneConnReqPerAddress(t, cm, testAddr1)

	// Advertise new addresses for Alice, one being the same as what she
	// had before and the other being a new one.
	addUpdate(alicePubKey, testAddr1, testAddr2)
	assertOneConnReqPerAddress(t, cm, testAddr1, testAddr2)

	// If addresses come through from NodeAnnouncement updates, they should
	// overwrite the current list of addresses stored for Alice and then
	// call ConnectPeer again, the appropriate connection requests should
	// be added and removed. We will set addresses 2 and 3. We then expect
	// the connection request for address 1 to be removed, the connReq for
	// 2 to remain and a connReq for 3 to be added.
	addUpdate(alicePubKey, testAddr2, testAddr3)
	m.ConnectPeer(alicePubKey)
	assertOneConnReqPerAddress(t, cm, testAddr2, testAddr3)
	assertNoConnReqs(t, cm, testAddr1)

	// Test that calling CancelConnReqs with ignore set to address 2, will
	// cancel the connection request for address 3 but leave the one for
	// address 2 intact.
	skip := cm.getID(testAddr2)
	m.CancelConnReqs(alicePubKey, &skip)
	assertOneConnReqPerAddress(t, cm, testAddr2)
	assertNoConnReqs(t, cm, testAddr3)
}

var _ connMgr = (*mockConnMgr)(nil)

// mockConnMgr mocks the connmgr.
type mockConnMgr struct {
	// reqs holds the active connection requests. It is a map from conn req
	// ID to conn req object.
	reqs map[uint64]*connmgr.ConnReq

	// cm is a real ConnManager. It is used (but not started) so that ID's
	// can be assigned to connmgr.ConnReq objects. We cannot manually
	// assign IDs since the id field is an unexported field.
	cm *connmgr.ConnManager

	sync.Mutex
}

// newMockConnMgr constructs a new mockConnMgr.
func newMockConnMgr(t *testing.T) *mockConnMgr {
	cm, err := connmgr.New(&connmgr.Config{
		Dial: func(addr net.Addr) (net.Conn, error) {
			return nil, nil
		},
	})
	require.NoError(t, err)
	cm.Start()

	return &mockConnMgr{
		reqs: make(map[uint64]*connmgr.ConnReq),
		cm:   cm,
	}
}

// stop cleans up any resources managed by the mockConnMgr.
func (m *mockConnMgr) stop() {
	m.cm.Stop()
}

// totalNumConnReqs returns the number of connection requests that the
// mockConnMgr is keeping track of.
func (m *mockConnMgr) totalNumConnReqs() int {
	m.Lock()
	defer m.Unlock()

	return len(m.reqs)
}

// numConnReqs returns the number of active connection requests to the given
// address.
func (m *mockConnMgr) numConnReqs(addr net.Addr) int {
	m.Lock()
	defer m.Unlock()

	count := 0
	for _, cr := range m.reqs {
		if cr.Addr.(*lnwire.NetAddress).Address.String() ==
			addr.String() {

			count++
		}
	}

	return count
}

// getID returns the ID that was assigned to the connection request for the
// given address.
func (m *mockConnMgr) getID(addr net.Addr) uint64 {
	m.Lock()
	defer m.Unlock()

	for id, cr := range m.reqs {
		if cr.Addr.(*lnwire.NetAddress).Address.String() ==
			addr.String() {

			return id
		}
	}

	return UnassignedConnID
}

// Connect adds the given connection request to the active set and generates
// a unique ID for it.
func (m *mockConnMgr) Connect(c *connmgr.ConnReq) {
	m.Lock()
	defer m.Unlock()

	m.cm.Connect(c)
	m.reqs[c.ID()] = c
}

// Remove removes the connection request with the given ID from the active set.
func (m *mockConnMgr) Remove(id uint64) {
	m.Lock()
	defer m.Unlock()

	m.cm.Remove(id)
	delete(m.reqs, id)
}

var _ connMgr = (*mockConnMgr)(nil)

// assertNoConnReqs ensures that the connection manager has no connection
// requests for any of the given addresses.
func assertNoConnReqs(t *testing.T, cm *mockConnMgr, addrs ...net.Addr) {
	err := wait.Predicate(func() bool {
		for _, addr := range addrs {
			if cm.numConnReqs(addr) != 0 {
				return false
			}
		}
		return true
	}, defaultTimeout)
	require.NoError(t, err)
}

// assertOneConnReqPerAddress ensures that the mock connection manager has one
// connection request for each address given.
func assertOneConnReqPerAddress(t *testing.T, cm *mockConnMgr,
	addrs ...net.Addr) {

	err := wait.Predicate(func() bool {
		for _, addr := range addrs {
			if cm.numConnReqs(addr) != 1 {
				return false
			}
		}
		return true
	}, defaultTimeout)
	require.NoError(t, err)
}
