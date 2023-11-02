package fn

import (
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
	// current is an immediately available copy of whatever is inside the
	// value channel that is served to readers. It is updated whenever a
	// change to the value channel is successful.
	current *atomic.Pointer[A]
	// readers is used to wake all blocked readers when a new value is
	// written.
	readers chan chan A

	// takers is used to wake a single taker when a new value is written.
	takers chan chan A

	// value is a bounded channel of size 1 that represents the core state
	// oof the channel.
	value chan A
}

// Zero initializes an MVar that has no values in it. In this state, TakeMVar
// will block and PutMVar will immediately succeed.
//
// Zero : () -> MVar[A].
func Zero[A any]() MVar[A] {
	ptr := atomic.Pointer[A]{}

	return MVar[A]{
		current: &ptr,
		readers: make(chan chan A),
		takers:  make(chan chan A),
		value:   make(chan A, 1),
	}
}

// NewMVar initializes an MVar that has a value in it from the getgo. In this
// state, TakeMVar will succeed immediately and PutMVar will block.
//
// NewMVar : A -> MVar[A].
func NewMVar[A any](a A) MVar[A] {
	z := Zero[A]()
	z.value <- a
	z.current.Store(&a)

	return z
}

// Take will wait for a value to be put into the MVar and then immediately
// take it out.
//
// Take : MVar[A] -> A.
func (m *MVar[A]) Take() A {
	select {
	case v := <-m.value:
		m.current.Store(nil)
		return v
	default:
		t := make(chan A)
		m.takers <- t
		return <-t
	}
}

// TryTake is the non-blocking version of TakeMVar, it will return an
// None() Option if it would have blocked.
//
// TryTake : MVar[A] -> Option[A].
func (m *MVar[A]) TryTake() Option[A] {
	select {
	case v := <-m.value:
		m.current.Store(nil)
		return Some(v)
	default:
		return None[A]()
	}
}

// Put will wait for a value to be made empty and will immediately replace it
// with the argument.
//
// Put : (MVar[A], A) -> ().
func (m *MVar[A]) Put(a A) {
readLoop:
	// Give the newly put value to all of the waiting readers.
	for {
		select {
		case r := <-m.readers:
			r <- a
		default:
			break readLoop
		}
	}

	// Give the newly put value to a single taker if one exists. If there
	// are no available takers, then store it in the MVar. Since the value
	// channel is bounded with capacity 1, subsequent put operations will
	// block.
	select {
	case t := <-m.takers:
		t <- a
	default:
		m.value <- a
		m.current.Store(&a)
	}
}

// TryPut is the non-blocking version of Put and will return true if the MVar is
// successfully set.
//
// TryPut : (MVar[A], A) -> bool.
func (m *MVar[A]) TryPut(a A) bool {
	select {
	case m.value <- a:
		m.current.Store(&a)
		return true
	default:
		return false
	}
}

// Read will atomically read the contents of the MVar. If the MVar is empty,
// Read will block until a value is put in. Callers of Read are guaranteed to
// be woken up before callers of Take.
//
// Read : MVar[A] -> A.
func (m *MVar[A]) Read() A {
	// Check to see if MVar has something in it.
	if ptr := m.current.Load(); ptr != nil {
		return *ptr
	}

	// It's empty so we need to wait.
	r := make(chan A)
	m.readers <- r

	return <-r
}

// TryRead will atomically read the contents of the MVar if it is full.
// Otherwise, it will return None.
//
// TryRead : MVar[A] -> Option[A].
func (m *MVar[A]) TryRead() Option[A] {
	if ptr := m.current.Load(); ptr != nil {
		return Some(*ptr)
	}

	return None[A]()
}

// IsFull will return true if the MVar currently has a value in it.
//
// IsFull : MVar[A] -> bool.
func (m *MVar[A]) IsFull() bool {
	return m.current.Load() != nil
}

// IsEmpty will return true if the MVar currently does not have a value in it.
//
// IsEmpty : MVar[A] -> bool.
func (m *MVar[A]) IsEmpty() bool {
	return m.current.Load() == nil
}
