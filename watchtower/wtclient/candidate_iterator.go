package wtclient

import (
	"container/list"
	"net"
	"sync"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// TowerCandidateIterator provides an abstraction for iterating through possible
// watchtower addresses when attempting to create a new session.
type TowerCandidateIterator interface {
	// AddCandidate adds a new candidate tower to the iterator. If the
	// candidate already exists, then any new addresses are added to it.
	AddCandidate(*Tower)

	// RemoveCandidate removes an existing candidate tower from the
	// iterator. An optional address can be provided to indicate a stale
	// tower address to remove it. If it isn't provided, then the tower is
	// completely removed from the iterator.
	RemoveCandidate(wtdb.TowerID, net.Addr) error

	// IsActive determines whether a given tower is exists within the
	// iterator.
	IsActive(wtdb.TowerID) bool

	// Reset clears any internal iterator state, making previously taken
	// candidates available as long as they remain in the set.
	Reset() error

	// GetTower gets the tower with the given ID from the iterator. If no
	// such tower is found then ErrTowerNotInIterator is returned.
	GetTower(id wtdb.TowerID) (*Tower, error)

	// Next returns the next candidate tower. The iterator is not required
	// to return results in any particular order.  If no more candidates are
	// available, ErrTowerCandidatesExhausted is returned.
	Next() (*Tower, error)
}

// towerListIterator is a linked-list backed TowerCandidateIterator.
type towerListIterator struct {
	mu            sync.Mutex
	queue         *list.List
	nextCandidate *list.Element
	candidates    map[wtdb.TowerID]*Tower
}

// Compile-time constraint to ensure *towerListIterator implements the
// TowerCandidateIterator interface.
var _ TowerCandidateIterator = (*towerListIterator)(nil)

// newTowerListIterator initializes a new towerListIterator from a variadic list
// of lnwire.NetAddresses.
func newTowerListIterator(candidates ...*Tower) *towerListIterator {
	iter := &towerListIterator{
		queue:      list.New(),
		candidates: make(map[wtdb.TowerID]*Tower),
	}

	for _, candidate := range candidates {
		iter.queue.PushBack(candidate.ID)
		iter.candidates[candidate.ID] = candidate
	}
	iter.Reset()

	return iter
}

// Reset clears the iterators state, and makes the address at the front of the
// list the next item to be returned..
func (t *towerListIterator) Reset() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reset the next candidate to the front of the linked-list.
	t.nextCandidate = t.queue.Front()

	return nil
}

// GetTower gets the tower with the given ID from the iterator. If no such tower
// is found then ErrTowerNotInIterator is returned.
func (t *towerListIterator) GetTower(id wtdb.TowerID) (*Tower, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tower, ok := t.candidates[id]
	if !ok {
		return nil, ErrTowerNotInIterator
	}

	return tower, nil
}

// Next returns the next candidate tower. This iterator will always return
// candidates in the order given when the iterator was instantiated.  If no more
// candidates are available, ErrTowerCandidatesExhausted is returned.
func (t *towerListIterator) Next() (*Tower, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for t.nextCandidate != nil {
		// Propose the tower at the front of the list.
		towerID := t.nextCandidate.Value.(wtdb.TowerID)

		// Check whether this tower is still considered a candidate. If
		// it's not, we'll proceed to the next.
		tower, ok := t.candidates[towerID]
		if !ok {
			nextCandidate := t.nextCandidate.Next()
			t.queue.Remove(t.nextCandidate)
			t.nextCandidate = nextCandidate
			continue
		}

		// Set the next candidate to the subsequent element.
		t.nextCandidate = t.nextCandidate.Next()
		return tower, nil
	}

	return nil, ErrTowerCandidatesExhausted
}

// AddCandidate adds a new candidate tower to the iterator. If the candidate
// already exists, then any new addresses are added to it.
func (t *towerListIterator) AddCandidate(candidate *Tower) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if tower, ok := t.candidates[candidate.ID]; !ok {
		t.queue.PushBack(candidate.ID)
		t.candidates[candidate.ID] = candidate

		// If we've reached the end of our queue, then this candidate
		// will become the next.
		if t.nextCandidate == nil {
			t.nextCandidate = t.queue.Back()
		}
	} else {
		candidate.Addresses.Reset()
		firstAddr := candidate.Addresses.Peek()
		tower.Addresses.Add(firstAddr)
		for {
			next, err := candidate.Addresses.Next()
			if err != nil {
				candidate.Addresses.Reset()
				break
			}

			tower.Addresses.Add(next)
		}
	}
}

// RemoveCandidate removes an existing candidate tower from the iterator. An
// optional address can be provided to indicate a stale tower address to remove
// it. If it isn't provided, then the tower is completely removed from the
// iterator.
func (t *towerListIterator) RemoveCandidate(candidate wtdb.TowerID,
	addr net.Addr) error {

	t.mu.Lock()
	defer t.mu.Unlock()

	tower, ok := t.candidates[candidate]
	if !ok {
		return nil
	}
	if addr != nil {
		err := tower.Addresses.Remove(addr)
		if err != nil {
			return err
		}
	} else {
		if tower.Addresses.HasLocked() {
			return ErrAddrInUse
		}

		delete(t.candidates, candidate)
	}

	return nil
}

// IsActive determines whether a given tower is exists within the iterator.
func (t *towerListIterator) IsActive(tower wtdb.TowerID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, ok := t.candidates[tower]
	return ok
}

// TODO(conner): implement graph-backed candidate iterator for public towers.
