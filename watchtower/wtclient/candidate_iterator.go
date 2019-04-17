package wtclient

import (
	"container/list"
	"sync"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// TowerCandidateIterator provides an abstraction for iterating through possible
// watchtower addresses when attempting to create a new session.
type TowerCandidateIterator interface {
	// Reset clears any internal iterator state, making previously taken
	// candidates available as long as they remain in the set.
	Reset() error

	// Next returns the next candidate tower. The iterator is not required
	// to return results in any particular order.  If no more candidates are
	// available, ErrTowerCandidatesExhausted is returned.
	Next() (*wtdb.Tower, error)
}

// towerListIterator is a linked-list backed TowerCandidateIterator.
type towerListIterator struct {
	mu            sync.Mutex
	candidates    *list.List
	nextCandidate *list.Element
}

// Compile-time constraint to ensure *towerListIterator implements the
// TowerCandidateIterator interface.
var _ TowerCandidateIterator = (*towerListIterator)(nil)

// newTowerListIterator initializes a new towerListIterator from a variadic list
// of lnwire.NetAddresses.
func newTowerListIterator(candidates ...*wtdb.Tower) *towerListIterator {
	iter := &towerListIterator{
		candidates: list.New(),
	}

	for _, candidate := range candidates {
		iter.candidates.PushBack(candidate)
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
	t.nextCandidate = t.candidates.Front()

	return nil
}

// Next returns the next candidate tower. This iterator will always return
// candidates in the order given when the iterator was instantiated.  If no more
// candidates are available, ErrTowerCandidatesExhausted is returned.
func (t *towerListIterator) Next() (*wtdb.Tower, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If the next candidate is nil, we've exhausted the list.
	if t.nextCandidate == nil {
		return nil, ErrTowerCandidatesExhausted
	}

	// Propose the tower at the front of the list.
	tower := t.nextCandidate.Value.(*wtdb.Tower)

	// Set the next candidate to the subsequent element.
	t.nextCandidate = t.nextCandidate.Next()

	return tower, nil
}

// TODO(conner): implement graph-backed candidate iterator for public towers.
