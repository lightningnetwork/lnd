package discovery

import (
	"testing"
	"time"

	"github.com/lightninglabs/neutrino/cache"
	"github.com/stretchr/testify/require"
)

// TestPurgeBanEntries tests that we properly purge ban entries on a timer.
func TestPurgeBanEntries(t *testing.T) {
	t.Parallel()

	testBanThreshold := uint64(10)

	b := newBanman(testBanThreshold)

	// Ban a peer by repeatedly incrementing its ban score.
	peer1 := [33]byte{0x00}

	for range testBanThreshold {
		b.incrementBanScore(peer1)
	}

	// Assert that the peer is now banned.
	require.True(t, b.isBanned(peer1))

	// A call to purgeBanEntries should not remove the peer from the index.
	b.purgeBanEntries()
	require.True(t, b.isBanned(peer1))

	// Now set the peer's last update time to two banTimes in the past so
	// that we can assert that purgeBanEntries does remove it from the
	// index.
	banInfo, err := b.peerBanIndex.Get(peer1)
	require.NoError(t, err)

	banInfo.lastUpdate = time.Now().Add(-2 * banTime)

	b.purgeBanEntries()
	_, err = b.peerBanIndex.Get(peer1)
	require.ErrorIs(t, err, cache.ErrElementNotFound)

	// Increment the peer's ban score again but don't get it banned.
	b.incrementBanScore(peer1)
	require.False(t, b.isBanned(peer1))

	// Assert that purgeBanEntries does nothing.
	b.purgeBanEntries()
	banInfo, err = b.peerBanIndex.Get(peer1)
	require.Nil(t, err)

	// Set its lastUpdate time to 2 resetDelta's in the past so that
	// purgeBanEntries removes it.
	banInfo.lastUpdate = time.Now().Add(-2 * resetDelta)

	b.purgeBanEntries()

	_, err = b.peerBanIndex.Get(peer1)
	require.ErrorIs(t, err, cache.ErrElementNotFound)
}
