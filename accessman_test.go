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

	isSlotAvailable, err := a.checkIncomingConnBanScore(remotePub)
	require.NoError(t, err)
	require.True(t, isSlotAvailable)

	peerAccess, err := a.assignPeerPerms(remotePub)
	require.NoError(t, err)
	require.Equal(t, status, peerAccess)

	a.addPeerAccess(remotePub, peerAccess)
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

	initPerms := func() (map[string]channeldb.ChanCount, error) {
		return map[string]channeldb.ChanCount{
			peerKeySer1: {
				HasOpenOrClosedChan: true,
			},
			peerKeySer2: {
				HasOpenOrClosedChan: true,
				PendingOpenCount:    1,
			},
			peerKeySer3: {
				HasOpenOrClosedChan: false,
				PendingOpenCount:    1,
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

	// Check that the peerCounts map is correctly populated with three
	// peers.
	require.Equal(t, 0, int(a.numRestricted))
	require.Equal(t, 3, len(a.peerCounts))

	peerCount1, ok := a.peerCounts[peerKeySer1]
	require.True(t, ok)
	require.True(t, peerCount1.HasOpenOrClosedChan)
	require.Equal(t, 0, int(peerCount1.PendingOpenCount))

	peerCount2, ok := a.peerCounts[peerKeySer2]
	require.True(t, ok)
	require.True(t, peerCount2.HasOpenOrClosedChan)
	require.Equal(t, 1, int(peerCount2.PendingOpenCount))

	peerCount3, ok := a.peerCounts[peerKeySer3]
	require.True(t, ok)
	require.False(t, peerCount3.HasOpenOrClosedChan)
	require.Equal(t, 1, int(peerCount3.PendingOpenCount))

	// We'll now start to connect the peers. We'll add a new fourth peer
	// that will take up the restricted slot. The first three peers should
	// be able to bypass this restricted slot mechanism.
	peerPriv4, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey4 := peerPriv4.PubKey()

	// Follow the normal process of an incoming connection. We check if we
	// can accommodate this peer in checkIncomingConnBanScore and then we
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

	// Check that an open channel promotes the temporary peer.
	err = a.newOpenChan(peerKey3)
	require.NoError(t, err)
	assertAccessState(t, a, peerKey3, peerStatusProtected)

	// We should be able to accommodate a new peer.
	peerPriv5, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peerKey5 := peerPriv5.PubKey()

	assertInboundConnection(t, a, peerKey5, peerStatusRestricted)

	// Check that a pending-close channel event for peer 4 demotes the
	// peer.
	err = a.newPendingCloseChan(peerKey4)
	require.ErrorIs(t, err, ErrNoMoreRestrictedAccessSlots)
}
