package multimutex

import (
	"fmt"
	"sync"
)

// cntMutex is a struct that wraps a counter and a mutex, and is used to keep
// track of the number of goroutines waiting for access to the
// mutex, such that we can forget about it when the counter is zero.
type cntMutex struct {
	cnt int
	sync.Mutex
}

// Mutex is a struct that keeps track of a set of mutexes with a given ID. It
// can be used for making sure only one goroutine gets given the mutex per ID.
type Mutex[T comparable] struct {
	// mutexes is a map of IDs to a cntMutex. The cntMutex for a given ID
	// will hold the mutex to be used by all callers requesting access for
	// the ID, in addition to the count of callers.
	mutexes map[T]*cntMutex

	// mapMtx is used to give synchronize concurrent access to the mutexes
	// map.
	mapMtx sync.Mutex
}

// NewMutex creates a new Mutex.
func NewMutex[T comparable]() *Mutex[T] {
	return &Mutex[T]{
		mutexes: make(map[T]*cntMutex),
	}
}

// Lock locks the mutex by the given ID. If the mutex is already locked by this
// ID, Lock blocks until the mutex is available.
func (c *Mutex[T]) Lock(id T) {
	c.mapMtx.Lock()
	mtx, ok := c.mutexes[id]
	if ok {
		// If the mutex already existed in the map, we increment its
		// counter, to indicate that there now is one more goroutine
		// waiting for it.
		mtx.cnt++
	} else {
		// If it was not in the map, it means no other goroutine has
		// locked the mutex for this ID, and we can create a new mutex
		// with count 1 and add it to the map.
		mtx = &cntMutex{
			cnt: 1,
		}
		c.mutexes[id] = mtx
	}
	c.mapMtx.Unlock()

	// Acquire the mutex for this ID.
	mtx.Lock()
}

// Unlock unlocks the mutex by the given ID. It is a run-time error if the
// mutex is not locked by the ID on entry to Unlock.
func (c *Mutex[T]) Unlock(id T) {
	// Since we are done with all the work for this update, we update the
	// map to reflect that.
	c.mapMtx.Lock()

	mtx, ok := c.mutexes[id]
	if !ok {
		// The mutex not existing in the map means an unlock for an ID
		// not currently locked was attempted.
		panic(fmt.Sprintf("double unlock for id %v",
			id))
	}

	// Decrement the counter. If the count goes to zero, it means this
	// caller was the last one to wait for the mutex, and we can delete it
	// from the map. We can do this safely since we are under the mapMtx,
	// meaning that all other goroutines waiting for the mutex already have
	// incremented it, or will create a new mutex when they get the mapMtx.
	mtx.cnt--
	if mtx.cnt == 0 {
		delete(c.mutexes, id)
	}
	c.mapMtx.Unlock()

	// Unlock the mutex for this ID.
	mtx.Unlock()
}
