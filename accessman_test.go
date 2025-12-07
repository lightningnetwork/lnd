package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// assertInboundConnection asserts that we're able to accept an inbound
// connection successfully without any access permissions being violated.
func assertInboundConnection(t *testing.T, a *accessMan,
	remotePub *btcec.PublicKey, status peerAccessStatus) {

	remotePubSer := string(remotePub.SerializeCompressed())

	isSlotAvailable, err := a.checkAcceptIncomingConn(remotePub)
	require.NoError(t, err)
	require.True(t, isSlotAvailable)

	peerAccess, err := a.assignPeerPerms(remotePub)
	require.NoError(t, err)
	require.Equal(t, status, peerAccess)

	a.addPeerAccess(remotePub, peerAccess, true)
	peerScore, ok := a.peerScores[remotePubSer]
	require.True(t, ok)
	require.Equal(t, status, peerScore.state)
}

func assertAccessState(t *testing.T, a *accessMan, remotePub *btcec.PublicKey,
	expectedStatus peerAccessStatus) {

	remotePubSer := string(remotePub.SerializeCompressed())
	peerScore, ok := a.peerScores[remotePubSer]
	require.True(t, ok)
	require.Equal(t, expectedStatus, peerScore.state)
}

// TestAccessManRestrictedSlots tests that the configurable number of
// restricted slots are properly allocated. It also tests that certain peers
// with access permissions are allowed to bypass the slot mechanism.
func TestAccessManRestrictedSlots(t *testing.T) {
	t.Parallel()

	// We'll pre-populate the map to mock the database fetch. We'll make
	// three peers. One has an open/closed channel. One has both an open
	// / closed channel and a pending channel. The last one has only a
	// pending channel.
	peerPriv1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey1 := peerPriv1.PubKey()
	peerKeySer1 := string(peerKey1.SerializeCompressed())

	peerPriv2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey2 := peerPriv2.PubKey()
	peerKeySer2 := string(peerKey2.SerializeCompressed())

	peerPriv3, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey3 := peerPriv3.PubKey()
	peerKeySer3 := string(peerKey3.SerializeCompressed())

	var (
		peer1PendingCount = 0
		peer2PendingCount = 1
		peer3PendingCount = 1
	)

	initPerms := func() (map[string]channeldb.ChanCount, error) {
		return map[string]channeldb.ChanCount{
			peerKeySer1: {
				HasOpenOrClosedChan: true,
				PendingOpenCount:    uint64(peer1PendingCount),
			},
			peerKeySer2: {
				HasOpenOrClosedChan: true,
				PendingOpenCount:    uint64(peer2PendingCount),
			},
			peerKeySer3: {
				HasOpenOrClosedChan: false,
				PendingOpenCount:    uint64(peer3PendingCount),
			},
		}, nil
	}

	disconnect := func(*btcec.PublicKey) (bool, error) {
		return false, nil
	}

	cfg := &accessManConfig{
		initAccessPerms:    initPerms,
		shouldDisconnect:   disconnect,
		maxRestrictedSlots: 1,
	}

	a, err := newAccessMan(cfg)
	require.NoError(t, err)

	// Check that the peerChanInfo map is correctly populated with three
	// peers.
	require.Equal(t, 0, int(a.numRestricted))
	require.Equal(t, 3, len(a.peerChanInfo))

	peerCount1, ok := a.peerChanInfo[peerKeySer1]
	require.True(t, ok)
	require.True(t, peerCount1.HasOpenOrClosedChan)
	require.Equal(t, peer1PendingCount, int(peerCount1.PendingOpenCount))

	peerCount2, ok := a.peerChanInfo[peerKeySer2]
	require.True(t, ok)
	require.True(t, peerCount2.HasOpenOrClosedChan)
	require.Equal(t, peer2PendingCount, int(peerCount2.PendingOpenCount))

	peerCount3, ok := a.peerChanInfo[peerKeySer3]
	require.True(t, ok)
	require.False(t, peerCount3.HasOpenOrClosedChan)
	require.Equal(t, peer3PendingCount, int(peerCount3.PendingOpenCount))

	// We'll now start to connect the peers. We'll add a new fourth peer
	// that will take up the restricted slot. The first three peers should
	// be able to bypass this restricted slot mechanism.
	peerPriv4, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey4 := peerPriv4.PubKey()

	// Follow the normal process of an incoming connection. We check if we
	// can accommodate this peer in checkAcceptIncomingConn and then we
	// assign its access permissions and then insert into the map.
	assertInboundConnection(t, a, peerKey4, peerStatusRestricted)

	// Connect the three peers. This should happen without any issue.
	assertInboundConnection(t, a, peerKey1, peerStatusProtected)
	assertInboundConnection(t, a, peerKey2, peerStatusProtected)
	assertInboundConnection(t, a, peerKey3, peerStatusTemporary)

	// Check that a pending-open channel promotes the restricted peer.
	err = a.newPendingOpenChan(peerKey4)
	require.NoError(t, err)
	assertAccessState(t, a, peerKey4, peerStatusTemporary)

	// Assert that accessman's internal state is updated with peer4. We
	// expect this new peer to have 1 pending open count.
	peerCount4, ok := a.peerChanInfo[string(peerKey4.SerializeCompressed())]
	require.True(t, ok)
	require.False(t, peerCount4.HasOpenOrClosedChan)
	require.Equal(t, 1, int(peerCount4.PendingOpenCount))

	// Check that an open channel promotes the temporary peer.
	err = a.newOpenChan(peerKey3)
	require.NoError(t, err)
	assertAccessState(t, a, peerKey3, peerStatusProtected)

	// Assert that accessman's internal state is updated with peer3. We
	// expect this existing peer to decrement its pending open count and the
	// flag `HasOpenOrClosedChan` should be true.
	peerCount3, ok = a.peerChanInfo[peerKeySer3]
	require.True(t, ok)
	require.True(t, peerCount3.HasOpenOrClosedChan)
	require.Equal(t, peer3PendingCount-1, int(peerCount3.PendingOpenCount))

	// We should be able to accommodate a new peer.
	peerPriv5, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey5 := peerPriv5.PubKey()

	assertInboundConnection(t, a, peerKey5, peerStatusRestricted)

	// Check that a pending-close channel event for peer 4 demotes the
	// peer.
	err = a.newPendingCloseChan(peerKey4)
	require.ErrorIs(t, err, ErrNoMoreRestrictedAccessSlots)

	// Assert that peer4 is removed.
	_, ok = a.peerChanInfo[string(peerKey4.SerializeCompressed())]
	require.False(t, ok)
}

// TestAssignPeerPerms asserts that the peer's access status is correctly
// assigned.
func TestAssignPeerPerms(t *testing.T) {
	t.Parallel()

	// genPeerPub is a helper closure that generates a random public key.
	genPeerPub := func() *btcec.PublicKey {
		peerPriv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return peerPriv.PubKey()
	}

	disconnect := func(_ *btcec.PublicKey) (bool, error) {
		return true, nil
	}

	noDisconnect := func(_ *btcec.PublicKey) (bool, error) {
		return false, nil
	}

	var testCases = []struct {
		name             string
		peerPub          *btcec.PublicKey
		chanCount        channeldb.ChanCount
		shouldDisconnect func(*btcec.PublicKey) (bool, error)
		numRestricted    int

		expectedStatus peerAccessStatus
		expectedErr    error
	}{
		// peer1 has a channel with us, and we expect it to have a
		// protected status.
		{
			name:    "peer with channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: true,
			},
			shouldDisconnect: noDisconnect,
			expectedStatus:   peerStatusProtected,
			expectedErr:      nil,
		},
		// peer2 has a channel open and a pending channel with us, we
		// expect it to have a protected status.
		{
			name:    "peer with channels and pending channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: true,
				PendingOpenCount:    1,
			},
			shouldDisconnect: noDisconnect,
			expectedStatus:   peerStatusProtected,
			expectedErr:      nil,
		},
		// peer3 has a pending channel with us, and we expect it to have
		// a temporary status.
		{
			name:    "peer with pending channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: false,
				PendingOpenCount:    1,
			},
			shouldDisconnect: noDisconnect,
			expectedStatus:   peerStatusTemporary,
			expectedErr:      nil,
		},
		// peer4 has no channel with us, and we expect it to have a
		// restricted status.
		{
			name:    "peer with no channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: false,
				PendingOpenCount:    0,
			},
			shouldDisconnect: noDisconnect,
			expectedStatus:   peerStatusRestricted,
			expectedErr:      nil,
		},
		// peer5 has no channel with us, and we expect it to have a
		// restricted status. We also expect the error `ErrGossiperBan`
		// to be returned given we will use a mocked `shouldDisconnect`
		// in this test to disconnect on peer5 only.
		{
			name:    "peer with no channels and banned",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: false,
				PendingOpenCount:    0,
			},
			shouldDisconnect: disconnect,
			expectedStatus:   peerStatusRestricted,
			expectedErr:      ErrGossiperBan,
		},
		// peer6 has no channel with us, and we expect it to have a
		// restricted status. Since this peer is seen, we don't expect
		// the error `ErrNoMoreRestrictedAccessSlots` to be returned.
		{
			name:    "peer with no channels and restricted",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: false,
				PendingOpenCount:    0,
			},
			shouldDisconnect: noDisconnect,
			numRestricted:    1,

			expectedStatus: peerStatusRestricted,
			expectedErr:    nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			peerStr := string(tc.peerPub.SerializeCompressed())

			initPerms := func() (map[string]channeldb.ChanCount,
				error) {

				return map[string]channeldb.ChanCount{
					peerStr: tc.chanCount,
				}, nil
			}

			cfg := &accessManConfig{
				initAccessPerms:    initPerms,
				shouldDisconnect:   tc.shouldDisconnect,
				maxRestrictedSlots: 1,
			}

			a, err := newAccessMan(cfg)
			require.NoError(t, err)

			// Initialize the internal state of the accessman.
			a.numRestricted = int64(tc.numRestricted)

			status, err := a.assignPeerPerms(tc.peerPub)
			require.Equal(t, tc.expectedStatus, status)
			require.ErrorIs(t, tc.expectedErr, err)
		})
	}
}

// TestAssignPeerPermsBypassRestriction asserts that when a peer has a channel
// with us, either it being open, pending, or closed, no restriction is placed
// on this peer.
func TestAssignPeerPermsBypassRestriction(t *testing.T) {
	t.Parallel()

	// genPeerPub is a helper closure that generates a random public key.
	genPeerPub := func() *btcec.PublicKey {
		peerPriv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return peerPriv.PubKey()
	}

	// Mock shouldDisconnect to always return true and assert that it has no
	// effect on the peer.
	disconnect := func(_ *btcec.PublicKey) (bool, error) {
		return true, nil
	}

	var testCases = []struct {
		name           string
		peerPub        *btcec.PublicKey
		chanCount      channeldb.ChanCount
		expectedStatus peerAccessStatus
	}{
		// peer1 has a channel with us, and we expect it to have a
		// protected status.
		{
			name:    "peer with channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: true,
			},
			expectedStatus: peerStatusProtected,
		},
		// peer2 has a channel open and a pending channel with us, we
		// expect it to have a protected status.
		{
			name:    "peer with channels and pending channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: true,
				PendingOpenCount:    1,
			},
			expectedStatus: peerStatusProtected,
		},
		// peer3 has a pending channel with us, and we expect it to have
		// a temporary status.
		{
			name:    "peer with pending channels",
			peerPub: genPeerPub(),
			chanCount: channeldb.ChanCount{
				HasOpenOrClosedChan: false,
				PendingOpenCount:    1,
			},
			expectedStatus: peerStatusTemporary,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			peerStr := string(tc.peerPub.SerializeCompressed())

			initPerms := func() (map[string]channeldb.ChanCount,
				error) {

				return map[string]channeldb.ChanCount{
					peerStr: tc.chanCount,
				}, nil
			}

			// Config the accessman such that it has zero max slots
			// and always return true on `shouldDisconnect`. We
			// should see the peers in this test are not affected by
			// these checks.
			cfg := &accessManConfig{
				initAccessPerms:    initPerms,
				shouldDisconnect:   disconnect,
				maxRestrictedSlots: 0,
			}

			a, err := newAccessMan(cfg)
			require.NoError(t, err)

			status, err := a.assignPeerPerms(tc.peerPub)
			require.NoError(t, err)
			require.Equal(t, tc.expectedStatus, status)
		})
	}
}

// TestAssignPeerPermsBypassExisting asserts that when the peer is a
// pre-existing peer, it won't be restricted.
func TestAssignPeerPermsBypassExisting(t *testing.T) {
	t.Parallel()

	// genPeerPub is a helper closure that generates a random public key.
	genPeerPub := func() *btcec.PublicKey {
		peerPriv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return peerPriv.PubKey()
	}

	// peer1 exists in `peerChanInfo` map.
	peer1 := genPeerPub()
	peer1Str := string(peer1.SerializeCompressed())

	// peer2 exists in `peerScores` map.
	peer2 := genPeerPub()
	peer2Str := string(peer2.SerializeCompressed())

	// peer3 is a new peer.
	peer3 := genPeerPub()

	// Create params to init the accessman.
	initPerms := func() (map[string]channeldb.ChanCount, error) {
		return map[string]channeldb.ChanCount{
			peer1Str: {},
		}, nil
	}

	disconnect := func(*btcec.PublicKey) (bool, error) {
		return false, nil
	}

	cfg := &accessManConfig{
		initAccessPerms:    initPerms,
		shouldDisconnect:   disconnect,
		maxRestrictedSlots: 0,
	}

	a, err := newAccessMan(cfg)
	require.NoError(t, err)

	// Add peer2 to the `peerScores`.
	a.peerScores[peer2Str] = peerSlotStatus{
		state: peerStatusTemporary,
	}

	// Assigning to peer1 should not return an error.
	status, err := a.assignPeerPerms(peer1)
	require.NoError(t, err)
	require.Equal(t, peerStatusRestricted, status)

	// Assigning to peer2 should not return an error.
	status, err = a.assignPeerPerms(peer2)
	require.NoError(t, err)
	require.Equal(t, peerStatusTemporary, status)

	// Assigning to peer3 should return an error.
	status, err = a.assignPeerPerms(peer3)
	require.ErrorIs(t, err, ErrNoMoreRestrictedAccessSlots)
	require.Equal(t, peerStatusRestricted, status)
}

// TestHasPeer asserts `hasPeer` returns the correct results.
func TestHasPeer(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create a testing accessMan.
	a := &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// peer1 exists with an open channel.
	peer1 := "peer1"
	a.peerChanInfo[peer1] = channeldb.ChanCount{
		HasOpenOrClosedChan: true,
	}
	peer1Access := peerStatusProtected

	// peer2 exists with a pending channel.
	peer2 := "peer2"
	a.peerChanInfo[peer2] = channeldb.ChanCount{
		PendingOpenCount: 1,
	}
	peer2Access := peerStatusTemporary

	// peer3 exists without any channels.
	peer3 := "peer3"
	a.peerChanInfo[peer3] = channeldb.ChanCount{}
	peer3Access := peerStatusRestricted

	// peer4 exists with a score.
	peer4 := "peer4"
	peer4Access := peerStatusTemporary
	a.peerScores[peer4] = peerSlotStatus{state: peer4Access}

	// peer5 doesn't exist.
	peer5 := "peer5"

	// We now assert `hasPeer` returns the correct results.
	//
	// peer1 should be found with peerStatusProtected.
	access, found := a.hasPeer(ctx, peer1)
	require.True(t, found)
	require.Equal(t, peer1Access, access)

	// peer2 should be found with peerStatusTemporary.
	access, found = a.hasPeer(ctx, peer2)
	require.True(t, found)
	require.Equal(t, peer2Access, access)

	// peer3 should be found with peerStatusRestricted.
	access, found = a.hasPeer(ctx, peer3)
	require.True(t, found)
	require.Equal(t, peer3Access, access)

	// peer4 should be found with peerStatusTemporary.
	access, found = a.hasPeer(ctx, peer4)
	require.True(t, found)
	require.Equal(t, peer4Access, access)

	// peer5 should NOT be found.
	access, found = a.hasPeer(ctx, peer5)
	require.False(t, found)
	require.Equal(t, peerStatusRestricted, access)
}

// TestAddPeerAccessInbound asserts the num of slots is correctly incremented
// only for a new inbound peer with restricted access.
func TestAddPeerAccessInbound(t *testing.T) {
	t.Parallel()

	// Create a testing accessMan.
	a := &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// Create a testing key.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	pubStr := string(pub.SerializeCompressed())

	// Add this peer as an inbound peer with peerStatusRestricted.
	a.addPeerAccess(pub, peerStatusRestricted, true)

	// Assert the accessMan's internal state.
	//
	// We expect to see one peer found in the score map, and one slot is
	// taken, and this peer is not found in the counts map.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(1), a.numRestricted)
	require.NotContains(t, a.peerChanInfo, pubStr)

	// The peer should be found in the score map.
	score, ok := a.peerScores[pubStr]
	require.True(t, ok)

	expecedScore := peerSlotStatus{state: peerStatusRestricted}
	require.Equal(t, expecedScore, score)

	// Add this peer again, we expect the available slots to stay unchanged.
	a.addPeerAccess(pub, peerStatusRestricted, true)

	// Assert the internal state is not changed.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(1), a.numRestricted)
	require.NotContains(t, a.peerChanInfo, pubStr)

	// Reset the accessMan.
	a = &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// Add this peer as an inbound peer with peerStatusTemporary.
	a.addPeerAccess(pub, peerStatusTemporary, true)

	// Assert the accessMan's internal state.
	//
	// We expect to see one peer found in the score map, and no slot is
	// taken since this peer is not restricted.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(0), a.numRestricted)

	// NOTE: in reality this is not possible as the peer must have been put
	// into the map `peerChanInfo` before its perm can be upgraded.
	require.NotContains(t, a.peerChanInfo, pubStr)

	// The peer should be found in the score map.
	score, ok = a.peerScores[pubStr]
	require.True(t, ok)

	expecedScore = peerSlotStatus{state: peerStatusTemporary}
	require.Equal(t, expecedScore, score)
}

// TestAddPeerAccessOutbound asserts that outbound peer is not restricted and
// its perm is upgraded when it has peerStatusRestricted.
func TestAddPeerAccessOutbound(t *testing.T) {
	t.Parallel()

	// Create a testing accessMan.
	a := &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// Create a testing key.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	pubStr := string(pub.SerializeCompressed())

	// Add this peer as an outbound peer with peerStatusRestricted.
	a.addPeerAccess(pub, peerStatusRestricted, false)

	// Assert the accessMan's internal state.
	//
	// We expect to see one peer found in the score map, and no slot is
	// taken, and this peer is found in the counts map.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(0), a.numRestricted)
	require.Contains(t, a.peerChanInfo, pubStr)

	// The peer should be found in the score map.
	score, ok := a.peerScores[pubStr]
	require.True(t, ok)

	// Its perm should be upgraded to temporary.
	expecedScore := peerSlotStatus{state: peerStatusTemporary}
	require.Equal(t, expecedScore, score)

	// The peer should be found in the peer counts map.
	count, ok := a.peerChanInfo[pubStr]
	require.True(t, ok)

	// The peer's count should be initialized correctly.
	require.Zero(t, count.PendingOpenCount)
	require.False(t, count.HasOpenOrClosedChan)

	// Add this peer again, we expect the available slots to stay unchanged.
	a.addPeerAccess(pub, peerStatusRestricted, true)

	// Assert the internal state is not changed.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(0), a.numRestricted)
	require.Contains(t, a.peerChanInfo, pubStr)

	// Reset the accessMan.
	a = &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// Add this peer as an inbound peer with peerStatusTemporary.
	a.addPeerAccess(pub, peerStatusTemporary, true)

	// Assert the accessMan's internal state.
	//
	// We expect to see one peer found in the score map, and no slot is
	// taken since this peer is not restricted.
	require.Len(t, a.peerScores, 1)
	require.Equal(t, int64(0), a.numRestricted)

	// NOTE: in reality this is not possible as the peer must have been put
	// into the map `peerChanInfo` before its perm can be upgraded.
	require.NotContains(t, a.peerChanInfo, pubStr)

	// The peer should be found in the score map.
	score, ok = a.peerScores[pubStr]
	require.True(t, ok)

	expecedScore = peerSlotStatus{state: peerStatusTemporary}
	require.Equal(t, expecedScore, score)
}

// TestRemovePeerAccess asserts `removePeerAccess` correctly update the
// accessman's internal state based on the peer's status.
func TestRemovePeerAccess(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Create a testing accessMan.
	a := &accessMan{
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	// numRestrictedExpected specifies the final value to expect once the
	// test finishes.
	var numRestrictedExpected int

	// peer1 exists with an open channel, which should not be removed. Since
	// it has protected status, the numRestricted should stay unchanged.
	peer1 := "peer1"
	a.peerChanInfo[peer1] = channeldb.ChanCount{
		HasOpenOrClosedChan: true,
	}
	peer1Access := peerStatusProtected
	a.peerScores[peer1] = peerSlotStatus{state: peer1Access}

	// peer2 exists with a pending channel, which should not be removed.
	// Since it has temporary status, the numRestricted should stay
	// unchanged.
	peer2 := "peer2"
	a.peerChanInfo[peer2] = channeldb.ChanCount{
		PendingOpenCount: 1,
	}
	peer2Access := peerStatusTemporary
	a.peerScores[peer2] = peerSlotStatus{state: peer2Access}

	// peer3 exists without any channels, which will be removed. Since it
	// has restricted status, the numRestricted should be decremented.
	peer3 := "peer3"
	a.peerChanInfo[peer3] = channeldb.ChanCount{}
	peer3Access := peerStatusRestricted
	a.peerScores[peer3] = peerSlotStatus{state: peer3Access}
	numRestrictedExpected--

	// peer4 exists with a score and a temporary status, which will be
	// removed.
	peer4 := "peer4"
	peer4Access := peerStatusTemporary
	a.peerScores[peer4] = peerSlotStatus{state: peer4Access}

	// peer5 doesn't exist, removing it will be a NOOP.
	peer5 := "peer5"

	// We now assert `removePeerAccess` behaves as expected.
	//
	// Remove peer1 should change nothing.
	a.removePeerAccess(ctx, peer1)

	// peer1 should be removed from peerScores but not peerChanInfo.
	_, found := a.peerScores[peer1]
	require.False(t, found)
	_, found = a.peerChanInfo[peer1]
	require.True(t, found)

	// Remove peer2 should change nothing.
	a.removePeerAccess(ctx, peer2)

	// peer2 should be removed from peerScores but not peerChanInfo.
	_, found = a.peerScores[peer2]
	require.False(t, found)
	_, found = a.peerChanInfo[peer2]
	require.True(t, found)

	// Remove peer3 should remove it from the maps.
	a.removePeerAccess(ctx, peer3)

	// peer3 should be removed from peerScores and peerChanInfo.
	_, found = a.peerScores[peer3]
	require.False(t, found)
	_, found = a.peerChanInfo[peer3]
	require.False(t, found)

	// Remove peer4 should remove it from the maps.
	a.removePeerAccess(ctx, peer4)

	// peer4 should be removed from peerScores and NOT be found in
	// peerChanInfo.
	_, found = a.peerScores[peer4]
	require.False(t, found)
	_, found = a.peerChanInfo[peer4]
	require.False(t, found)

	// Remove peer5 should be NOOP.
	a.removePeerAccess(ctx, peer5)

	// peer5 should NOT be found.
	_, found = a.peerScores[peer5]
	require.False(t, found)
	_, found = a.peerChanInfo[peer5]
	require.False(t, found)

	// Finally, assert the numRestricted is decremented as expected. Given
	// we only have peer3 which has restricted access, it should decrement
	// once.
	//
	// NOTE: The value is actually negative here, which is allowed in
	// accessman.
	require.EqualValues(t, numRestrictedExpected, a.numRestricted)
}
