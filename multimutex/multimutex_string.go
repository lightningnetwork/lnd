package multimutex

import (
	"fmt"
	"sync"
)

// StringMutex is a struct that keeps track of a set of mutexes with a given
// string. It can be used for making sure only one goroutine gets given the
// mutex per string.
type StringMutex struct {
	// mutexes is a map of stringss to a cntMutex. The cntMutex for a given
	// string will hold the mutex to be used by all callers requesting
	// access for the string, in addition to the count of callers.
	mutexes map[string]*cntMutex

	// mapMtx is used to give synchronize concurrent access to the mutexes
	// map.
	mapMtx sync.Mutex
}

// NewStringMutex creates a new Mutex.
func NewStringMutex() *StringMutex {
	return &StringMutex{
		mutexes: make(map[string]*cntMutex),
	}
}

// Lock locks the mutex by the given string. If the mutex is already locked by
// this string, Lock blocks until the mutex is available.
func (c *StringMutex) Lock(key string) {
	c.mapMtx.Lock()
	mtx, ok := c.mutexes[key]
	if ok {
		// If the mutex already existed in the map, we increment its
		// counter, to indicate that there now is one more goroutine
		// waiting for it.
		mtx.cnt++
	} else {
		// If it was not in the map, it means no other goroutine has
		// locked the mutex for this string, and we can create a new
		// mutex with count 1 and add it to the map.
		mtx = &cntMutex{
			cnt: 1,
		}
		c.mutexes[key] = mtx
	}
	c.mapMtx.Unlock()

	// Acquire the mutex for this string.
	mtx.Lock()
}

// Unlock unlocks the mutex by the given string. It is a run-time error if the
// mutex is not locked by the string on entry to Unlock.
func (c *StringMutex) Unlock(key string) {
	// Since we are done with all the work for this update, we update the
	// map to reflect that.
	c.mapMtx.Lock()

	mtx, ok := c.mutexes[key]
	if !ok {
		// The mutex not existing in the map means an unlock for an
		// string not currently locked was attempted.
		panic(fmt.Sprintf("double unlock for key %x", key))
	}

	// Decrement the counter. If the count goes to zero, it means this
	// caller was the last one to wait for the mutex, and we can delete it
	// from the map. We can do this safely since we are under the mapMtx,
	// meaning that all other goroutines waiting for the mutex already have
	// incremented it, or will create a new mutex when they get the mapMtx.
	mtx.cnt--
	if mtx.cnt == 0 {
		delete(c.mutexes, key)
	}
	c.mapMtx.Unlock()

	// Unlock the mutex for this string.
	mtx.Unlock()
}
