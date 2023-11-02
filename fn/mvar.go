package fn

import (
	"sync"
	"sync/atomic"
)

// MVar[A any] is a structure that is designed to store a single value in an API
// that dispenses with data races. Think of it as a box for a value.
//
// It has two states: full and empty.
//
// It supports two operations: take and put.
//
// The state transition rules are as follows:
//  1. put while full blocks.
//  2. put while empty sets.
//  3. take while full resets.
//  4. take while empty blocks.
//  5. read while full nops.
//  6. read while empty blocks.
type MVar[A any] struct {
	// value is the core state of the MVar
	value *atomic.Pointer[A]

	// rCond is the Cond used to wake blocked Read operations.
	rCond *sync.Cond

	// tCond is the Cond used to wake blocked Take operations.
	tCond *sync.Cond

	// pCond is the Cond used to wake blocked Put operations.
	pCond *sync.Cond

	// reads is a WaitGroup that Put will wait for before signaling takers.
	reads *sync.WaitGroup

	// wgMtx is a Mutex used to protect the WaitGroup to ensure we don't
	// do new Adds while the Wait is finishing out.
	wgMtx *sync.Mutex
}

// NewEmptyMVar initializes an MVar that has no values in it. In this state,
// TakeMVar will block and PutMVar will immediately succeed.
//
// NewEmptyMVar : () -> MVar[A].
func NewEmptyMVar[A any]() MVar[A] {
	mutex := &sync.Mutex{}

	return MVar[A]{
		value: &atomic.Pointer[A]{},
		rCond: sync.NewCond(mutex),
		tCond: sync.NewCond(mutex),
		pCond: sync.NewCond(mutex),
		reads: &sync.WaitGroup{},
		wgMtx: &sync.Mutex{},
	}
}

// NewMVar initializes an MVar that has a value in it from the getgo. In this
// state, TakeMVar will succeed immediately and PutMVar will block.
//
// NewMVar : A -> MVar[A].
func NewMVar[A any](a A) MVar[A] {
	z := NewEmptyMVar[A]()
	z.value.Store(&a)
	return z
}

// Take will wait for a value to be put into the MVar and then immediately
// take it out.
//
// Take : MVar[A] -> A.
func (m *MVar[A]) Take() A {
	var val *A

	// Here we use a Cond pattern and try to Swap nil into value. If we get
	// a non-nil out it means we can actually finish the Take.
	m.tCond.L.Lock()
	for val = m.value.Swap(nil); val == nil; val = m.value.Swap(nil) {
		m.tCond.Wait()
	}
	m.tCond.L.Unlock()

	// now that we've successfully swapped a non-nil value out of the
	// atomic, we can now wake any Put operations which would replace the
	// value we just removed.
	m.pCond.Signal()

	return *val
}

// TryTake is the non-blocking version of TakeMVar, it will return an
// None() Option if it would have blocked.
//
// TryTake : MVar[A] -> Option[A].
func (m *MVar[A]) TryTake() Option[A] {
	// If Swap is non-nil it means we can actually Take a value out.
	if val := m.value.Swap(nil); val != nil {
		// We just managed to Swap a nil in so we need to give blocked
		// Puts the opportunity to proceed.
		m.pCond.Signal()

		return Some(*val)
	}

	return None[A]()
}

// Put will wait for a value to be made empty and will immediately replace it
// with the argument.
//
// Put : (MVar[A], A) -> ().
func (m *MVar[A]) Put(a A) {
	// Here we use a Cond pattern and try to CompareAndSwap expecting value
	// to be nil. If we successfully swap, it means we can actually finish
	// the Put.
	m.pCond.L.Lock()
	swapped := m.value.CompareAndSwap(nil, &a)
	for ; !swapped; swapped = m.value.CompareAndSwap(nil, &a) {
		m.pCond.Wait()
	}
	m.pCond.L.Unlock()

	// Now that we set value, we wake the Reads.
	m.rCond.Broadcast()

	// Here we wait for all the Reads to finish.
	m.wgMtx.Lock()
	m.reads.Wait()
	m.wgMtx.Unlock()

	// Now that the readers have finished we wake a single Take.
	m.tCond.Signal()
}

// TryPut is the non-blocking version of Put and will return true if the MVar is
// successfully set.
//
// TryPut : (MVar[A], A) -> bool.
func (m *MVar[A]) TryPut(a A) bool {
	// We use CompareAndSwap expecting value to be nil indicating an empty
	// state.
	if !m.value.CompareAndSwap(nil, &a) {
		return false
	}

	// Now that we set value, we wake the Reads.
	m.rCond.Broadcast()

	// Here we wait for all the Reads to finish.
	m.wgMtx.Lock()
	m.reads.Wait()
	m.wgMtx.Unlock()

	// Now that the readers have finished we wake a single Take.
	m.tCond.Signal()

	return true
}

// Read will atomically read the contents of the MVar. If the MVar is empty,
// Read will block until a value is put in. Callers of Read are guaranteed to
// be woken up before callers of Take.
//
// Read : MVar[A] -> A.
func (m *MVar[A]) Read() A {
	var val *A

	// While the Read is going we need to increment the WaitGroup so that
	// a waiting Put operation knows when it can actually wake any waiting
	// Takes
	m.wgMtx.Lock()
	m.reads.Add(1)
	m.wgMtx.Unlock()
	defer m.reads.Done()

	// Here we use the Cond pattern waiting for value to be non-nil.
	m.rCond.L.Lock()
	for val = m.value.Load(); val == nil; val = m.value.Load() {
		m.rCond.Wait()
	}
	m.rCond.L.Unlock()

	return *val
}

// TryRead will atomically read the contents of the MVar if it is full.
// Otherwise, it will return None.
//
// TryRead : MVar[A] -> Option[A].
func (m *MVar[A]) TryRead() Option[A] {
	if ptr := m.value.Load(); ptr != nil {
		return Some(*ptr)
	}

	return None[A]()
}

// IsFull will return true if the MVar currently has a value in it.
//
// IsFull : MVar[A] -> bool.
func (m *MVar[A]) IsFull() bool {
	return m.TryRead().IsSome()
}

// IsEmpty will return true if the MVar currently does not have a value in it.
//
// IsEmpty : MVar[A] -> bool.
func (m *MVar[A]) IsEmpty() bool {
	return !m.IsFull()
}
