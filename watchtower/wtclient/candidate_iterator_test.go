package wtclient

import (
	"encoding/binary"
	"math/rand"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/stretchr/testify/require"
)

func init() {
	rand.Seed(time.Now().Unix())
}

func randAddr(t *testing.T) net.Addr {
	t.Helper()

	var ip [4]byte
	_, err := rand.Read(ip[:])
	require.NoError(t, err)

	var port [2]byte
	_, err = rand.Read(port[:])
	require.NoError(t, err)

	return &net.TCPAddr{
		IP:   net.IP(ip[:]),
		Port: int(binary.BigEndian.Uint16(port[:])),
	}
}

func randTower(t *testing.T) *Tower {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to create private key")
	pubKey := priv.PubKey()
	addrs, err := newAddressIterator(randAddr(t))
	require.NoError(t, err)

	return &Tower{
		ID:          wtdb.TowerID(rand.Uint64()),
		IdentityKey: pubKey,
		Addresses:   addrs,
	}
}

func copyTower(t *testing.T, tower *Tower) *Tower {
	t.Helper()

	return &Tower{
		ID:          tower.ID,
		IdentityKey: tower.IdentityKey,
		Addresses:   tower.Addresses.Copy(),
	}
}

func assertActiveCandidate(t *testing.T, i TowerCandidateIterator, c *Tower,
	active bool) {

	t.Helper()

	isCandidate := i.IsActive(c.ID)
	if isCandidate {
		require.Truef(t, active, "expected tower %v to no longer be "+
			"an active candidate", c.ID)
		return
	}
	require.Falsef(t, active, "expected tower %v to be an active candidate",
		c.ID)
}

func assertNextCandidate(t *testing.T, i TowerCandidateIterator, c *Tower) {
	t.Helper()

	tower, err := i.Next()
	require.NoError(t, err)
	assertTowersEqual(t, c, tower)
}

func assertTowersEqual(t *testing.T, expected, actual *Tower) {
	t.Helper()

	require.True(t, expected.IdentityKey.IsEqual(actual.IdentityKey))
	require.Equal(t, expected.ID, actual.ID)
	require.Equal(t, expected.Addresses.GetAll(), actual.Addresses.GetAll())
}

// TestTowerCandidateIterator asserts the internal state of a
// TowerCandidateIterator after a series of updates to its candidates.
func TestTowerCandidateIterator(t *testing.T) {
	t.Parallel()

	// We'll start our test by creating an iterator of four candidate
	// towers. We'll use copies of these towers within the iterator to
	// ensure the iterator properly updates the state of its candidates.
	const numTowers = 4
	towers := make([]*Tower, 0, numTowers)
	for i := 0; i < numTowers; i++ {
		towers = append(towers, randTower(t))
	}
	towerCopies := make([]*Tower, 0, numTowers)
	for _, tower := range towers {
		towerCopies = append(towerCopies, copyTower(t, tower))
	}
	towerIterator := newTowerListIterator(towerCopies...)

	// We should expect to see all of our candidates in the order that they
	// were added.
	for _, expTower := range towers {
		tower, err := towerIterator.Next()
		require.NoError(t, err)
		require.Equal(t, expTower, tower)
	}

	_, err := towerIterator.Next()
	require.ErrorIs(t, err, ErrTowerCandidatesExhausted)

	towerIterator.Reset()

	// We'll then attempt to test the RemoveCandidate behavior of the
	// iterator. We'll attempt to remove the address of the first tower,
	// which should result in an error due to it being the last address of
	// the tower.
	firstTower := towers[0]
	firstTowerAddr := firstTower.Addresses.Peek()
	err = towerIterator.RemoveCandidate(firstTower.ID, firstTowerAddr)
	require.ErrorIs(t, err, wtdb.ErrLastTowerAddr)
	assertActiveCandidate(t, towerIterator, firstTower, true)
	assertNextCandidate(t, towerIterator, firstTower)

	// We'll then remove the second tower completely from the iterator by
	// not providing the optional address. Since it's been removed, we
	// should expect to see the third tower next.
	secondTower, thirdTower := towers[1], towers[2]
	err = towerIterator.RemoveCandidate(secondTower.ID, nil)
	require.NoError(t, err)
	assertActiveCandidate(t, towerIterator, secondTower, false)
	assertNextCandidate(t, towerIterator, thirdTower)

	// We'll then update the fourth candidate with a new address. A
	// duplicate shouldn't be added since it already exists within the
	// iterator, but the new address should be.
	fourthTower := towers[3]
	assertActiveCandidate(t, towerIterator, fourthTower, true)
	fourthTower.Addresses.Add(randAddr(t))
	towerIterator.AddCandidate(fourthTower)
	assertNextCandidate(t, towerIterator, fourthTower)

	// Finally, we'll attempt to add a new candidate to the end of the
	// iterator. Since it didn't already exist and we've reached the end, it
	// should be available as the next candidate.
	towerIterator.AddCandidate(secondTower)
	assertActiveCandidate(t, towerIterator, secondTower, true)
	assertNextCandidate(t, towerIterator, secondTower)

	// Assert that the GetTower correctly returns the tower too.
	tower, err := towerIterator.GetTower(secondTower.ID)
	require.NoError(t, err)
	assertTowersEqual(t, secondTower, tower)

	// Now remove the tower and assert that GetTower returns expected error.
	err = towerIterator.RemoveCandidate(secondTower.ID, nil)
	require.NoError(t, err)

	_, err = towerIterator.GetTower(secondTower.ID)
	require.ErrorIs(t, err, ErrTowerNotInIterator)
}
