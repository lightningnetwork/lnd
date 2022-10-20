package wtclient

import (
	"container/list"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

var (
	// ErrAddressesExhausted signals that a addressIterator has cycled
	// through all available addresses.
	ErrAddressesExhausted = errors.New("exhausted all addresses")

	// ErrAddrInUse indicates that an address is locked and cannot be
	// removed from the addressIterator.
	ErrAddrInUse = errors.New("address in use")
)

// AddressIterator handles iteration over a list of addresses. It strictly
// disallows the list of addresses it holds to be empty. It also allows callers
// to place locks on certain addresses in order to prevent other callers from
// removing the addresses in question from the iterator.
type AddressIterator interface {
	// Next returns the next candidate address. This iterator will always
	// return candidates in the order given when the iterator was
	// instantiated. If no more candidates are available,
	// ErrAddressesExhausted is returned.
	Next() (net.Addr, error)

	// NextAndLock does the same as described for Next, and it also places a
	// lock on the returned address so that the address can not be removed
	// until the lock on it has been released via ReleaseLock.
	NextAndLock() (net.Addr, error)

	// Peek returns the currently selected address in the iterator. If the
	// end of the iterator has been reached then it is reset and the first
	// item in the iterator is returned. Since the AddressIterator will
	// never have an empty address list, this function will never return a
	// nil value.
	Peek() net.Addr

	// PeekAndLock does the same as described for Peek, and it also places
	// a lock on the returned address so that the address can not be removed
	// until the lock on it has been released via ReleaseLock.
	PeekAndLock() net.Addr

	// ReleaseLock releases the lock held on the given address.
	ReleaseLock(addr net.Addr)

	// Add adds a new address to the iterator.
	Add(addr net.Addr)

	// Remove removes an existing address from the iterator. It disallows
	// the address from being removed if it is the last address in the
	// iterator or if there is currently a lock on the address.
	Remove(addr net.Addr) error

	// HasLocked returns true if the addressIterator has any locked
	// addresses.
	HasLocked() bool

	// GetAll returns a copy of all the addresses in the iterator.
	GetAll() []net.Addr

	// Reset clears the iterators state, and makes the address at the front
	// of the list the next item to be returned.
	Reset()
}

// A compile-time check to ensure that addressIterator implements the
// AddressIterator interface.
var _ AddressIterator = (*addressIterator)(nil)

// addressIterator is a linked-list implementation of an AddressIterator.
type addressIterator struct {
	mu             sync.Mutex
	addrList       *list.List
	currentTopAddr *list.Element
	candidates     map[string]*candidateAddr
	totalLockCount int
}

type candidateAddr struct {
	addr     net.Addr
	numLocks int
}

// newAddressIterator constructs a new addressIterator.
func newAddressIterator(addrs ...net.Addr) (*addressIterator, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("must have at least one address")
	}

	iter := &addressIterator{
		addrList:   list.New(),
		candidates: make(map[string]*candidateAddr),
	}

	for _, addr := range addrs {
		addrID := addr.String()
		iter.addrList.PushBack(addrID)
		iter.candidates[addrID] = &candidateAddr{addr: addr}
	}
	iter.Reset()

	return iter, nil
}

// Reset clears the iterators state, and makes the address at the front of the
// list the next item to be returned.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.unsafeReset()
}

// unsafeReset clears the iterator state and makes the address at the front of
// the list the next item to be returned.
//
// NOTE: this method is not thread safe and so should only be called if the
// appropriate mutex is being held.
func (a *addressIterator) unsafeReset() {
	// Reset the next candidate to the front of the linked-list.
	a.currentTopAddr = a.addrList.Front()
}

// Next returns the next candidate address. This iterator will always return
// candidates in the order given when the iterator was instantiated. If no more
// candidates are available, ErrAddressesExhausted is returned.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) Next() (net.Addr, error) {
	return a.next(false)
}

// NextAndLock does the same as described for Next, and it also places a lock on
// the returned address so that the address can not be removed until the lock on
// it has been released via ReleaseLock.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) NextAndLock() (net.Addr, error) {
	return a.next(true)
}

// next returns the next candidate address. This iterator will always return
// candidates in the order given when the iterator was instantiated. If no more
// candidates are available, ErrAddressesExhausted is returned.
func (a *addressIterator) next(lock bool) (net.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Set the next candidate to the subsequent element.
	a.currentTopAddr = a.currentTopAddr.Next()

	for a.currentTopAddr != nil {
		// Propose the address at the front of the list.
		addrID := a.currentTopAddr.Value.(string)

		// Check whether this address is still considered a candidate.
		// If it's not, we'll proceed to the next.
		candidate, ok := a.candidates[addrID]
		if !ok {
			nextCandidate := a.currentTopAddr.Next()
			a.addrList.Remove(a.currentTopAddr)
			a.currentTopAddr = nextCandidate
			continue
		}

		if lock {
			candidate.numLocks++
			a.totalLockCount++
		}

		return candidate.addr, nil
	}

	return nil, ErrAddressesExhausted
}

// Peek returns the currently selected address in the iterator. If the end of
// the list has been reached then the iterator is reset and the first item in
// the list is returned. Since the addressIterator will never have an empty
// address list, this function will never return a nil value.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) Peek() net.Addr {
	return a.peek(false)
}

// PeekAndLock does the same as described for Peek, and it also places a lock on
// the returned address so that the address can not be removed until the lock
// on it has been released via ReleaseLock.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) PeekAndLock() net.Addr {
	return a.peek(true)
}

// peek returns the currently selected address in the iterator. If the end of
// the list has been reached then the iterator is reset and the first item in
// the list is returned. Since the addressIterator will never have an empty
// address list, this function will never return a nil value. If lock is set to
// true, the address will be locked for removal until ReleaseLock has been
// called for the address.
func (a *addressIterator) peek(lock bool) net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()

	for {
		// If currentTopAddr is nil, it means we have reached the end of
		// the list, so we reset it here. The iterator always has at
		// least one address, so we can be sure that currentTopAddr will
		// be non-nil after calling reset here.
		if a.currentTopAddr == nil {
			a.unsafeReset()
		}

		addrID := a.currentTopAddr.Value.(string)
		candidate, ok := a.candidates[addrID]
		if !ok {
			nextCandidate := a.currentTopAddr.Next()
			a.addrList.Remove(a.currentTopAddr)
			a.currentTopAddr = nextCandidate
			continue
		}

		if lock {
			candidate.numLocks++
			a.totalLockCount++
		}

		return candidate.addr
	}
}

// ReleaseLock releases the lock held on the given address.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) ReleaseLock(addr net.Addr) {
	a.mu.Lock()
	defer a.mu.Unlock()

	candidateAddr, ok := a.candidates[addr.String()]
	if !ok {
		return
	}

	if candidateAddr.numLocks == 0 {
		return
	}

	candidateAddr.numLocks--
	a.totalLockCount--
}

// Add adds a new address to the iterator.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) Add(addr net.Addr) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.candidates[addr.String()]; ok {
		return
	}

	a.addrList.PushBack(addr.String())
	a.candidates[addr.String()] = &candidateAddr{addr: addr}

	// If we've reached the end of our queue, then this candidate
	// will become the next.
	if a.currentTopAddr == nil {
		a.currentTopAddr = a.addrList.Back()
	}
}

// Remove removes an existing address from the iterator. It disallows the
// address from being removed if it is the last address in the iterator or if
// there is currently a lock on the address.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) Remove(addr net.Addr) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	candidate, ok := a.candidates[addr.String()]
	if !ok {
		return nil
	}

	if len(a.candidates) == 1 {
		return wtdb.ErrLastTowerAddr
	}

	if candidate.numLocks > 0 {
		return ErrAddrInUse
	}

	delete(a.candidates, addr.String())
	return nil
}

// HasLocked returns true if the addressIterator has any locked addresses.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) HasLocked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.totalLockCount > 0
}

// GetAll returns a copy of all the addresses in the iterator.
//
// NOTE: This is part of the AddressIterator interface.
func (a *addressIterator) GetAll() []net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()

	var addrs []net.Addr
	cursor := a.addrList.Front()

	for cursor != nil {
		addrID := cursor.Value.(string)

		addr, ok := a.candidates[addrID]
		if !ok {
			cursor = cursor.Next()
			continue
		}

		addrs = append(addrs, addr.addr)
		cursor = cursor.Next()
	}

	return addrs
}
