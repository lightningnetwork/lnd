package wtclient

import (
	"encoding/binary"
	"math/rand"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

func init() {
	rand.Seed(time.Now().Unix())
}

func randAddr(t *testing.T) net.Addr {
	var ip [4]byte
	if _, err := rand.Read(ip[:]); err != nil {
		t.Fatal(err)
	}
	var port [2]byte
	if _, err := rand.Read(port[:]); err != nil {
		t.Fatal(err)

	}
	return &net.TCPAddr{
		IP:   net.IP(ip[:]),
		Port: int(binary.BigEndian.Uint16(port[:])),
	}
}

func randTower(t *testing.T) *wtdb.Tower {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to create private key: %v", err)
	}
	pubKey := priv.PubKey()
	pubKey.Curve = nil
	return &wtdb.Tower{
		ID:          wtdb.TowerID(rand.Uint64()),
		IdentityKey: pubKey,
		Addresses:   []net.Addr{randAddr(t)},
	}
}

func copyTower(tower *wtdb.Tower) *wtdb.Tower {
	t := &wtdb.Tower{
		ID:          tower.ID,
		IdentityKey: tower.IdentityKey,
		Addresses:   make([]net.Addr, len(tower.Addresses)),
	}
	copy(t.Addresses, tower.Addresses)
	return t
}

func assertActiveCandidate(t *testing.T, i TowerCandidateIterator,
	c *wtdb.Tower, active bool) {

	isCandidate := i.IsActive(c.ID)
	if isCandidate && !active {
		t.Fatalf("expected tower %v to no longer be an active candidate",
			c.ID)
	}
	if !isCandidate && active {
		t.Fatalf("expected tower %v to be an active candidate", c.ID)
	}
}

func assertNextCandidate(t *testing.T, i TowerCandidateIterator, c *wtdb.Tower) {
	t.Helper()

	tower, err := i.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tower, c) {
		t.Fatalf("expected tower: %v\ngot: %v", spew.Sdump(c),
			spew.Sdump(tower))
	}
}

// TestTowerCandidateIterator asserts the internal state of a
// TowerCandidateIterator after a series of updates to its candidates.
func TestTowerCandidateIterator(t *testing.T) {
	t.Parallel()

	// We'll start our test by creating an iterator of four candidate
	// towers. We'll use copies of these towers within the iterator to
	// ensure the iterator properly updates the state of its candidates.
	const numTowers = 4
	towers := make([]*wtdb.Tower, 0, numTowers)
	for i := 0; i < numTowers; i++ {
		towers = append(towers, randTower(t))
	}
	towerCopies := make([]*wtdb.Tower, 0, numTowers)
	for _, tower := range towers {
		towerCopies = append(towerCopies, copyTower(tower))
	}
	towerIterator := newTowerListIterator(towerCopies...)

	// We should expect to see all of our candidates in the order that they
	// were added.
	for _, expTower := range towers {
		tower, err := towerIterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tower, expTower) {
			t.Fatalf("expected tower: %v\ngot: %v",
				spew.Sdump(expTower), spew.Sdump(tower))
		}
	}

	if _, err := towerIterator.Next(); err != ErrTowerCandidatesExhausted {
		t.Fatalf("expected ErrTowerCandidatesExhausted, got %v", err)
	}
	towerIterator.Reset()

	// We'll then attempt to test the RemoveCandidate behavior of the
	// iterator. We'll remove the address of the first tower, which should
	// result in it not having any addresses left, but still being an active
	// candidate.
	firstTower := towers[0]
	firstTowerAddr := firstTower.Addresses[0]
	firstTower.RemoveAddress(firstTowerAddr)
	towerIterator.RemoveCandidate(firstTower.ID, firstTowerAddr)
	assertActiveCandidate(t, towerIterator, firstTower, true)
	assertNextCandidate(t, towerIterator, firstTower)

	// We'll then remove the second tower completely from the iterator by
	// not providing the optional address. Since it's been removed, we
	// should expect to see the third tower next.
	secondTower, thirdTower := towers[1], towers[2]
	towerIterator.RemoveCandidate(secondTower.ID, nil)
	assertActiveCandidate(t, towerIterator, secondTower, false)
	assertNextCandidate(t, towerIterator, thirdTower)

	// We'll then update the fourth candidate with a new address. A
	// duplicate shouldn't be added since it already exists within the
	// iterator, but the new address should be.
	fourthTower := towers[3]
	assertActiveCandidate(t, towerIterator, fourthTower, true)
	fourthTower.AddAddress(randAddr(t))
	towerIterator.AddCandidate(fourthTower)
	assertNextCandidate(t, towerIterator, fourthTower)

	// Finally, we'll attempt to add a new candidate to the end of the
	// iterator. Since it didn't already exist and we've reached the end, it
	// should be available as the next candidate.
	towerIterator.AddCandidate(secondTower)
	assertActiveCandidate(t, towerIterator, secondTower, true)
	assertNextCandidate(t, towerIterator, secondTower)
}
