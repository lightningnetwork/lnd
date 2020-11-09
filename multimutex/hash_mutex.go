package multimutex

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/lntypes"
)

// HashMutex is a struct that keeps track of a set of mutexes with a given hash.
// It can be used for making sure only one goroutine gets given the mutex per
// hash.
type HashMutex struct {
	// mutexes is a map of hashes to a cntMutex. The cntMutex for
	// a given hash will hold the mutex to be used by all
	// callers requesting access for the hash, in addition to
	// the count of callers.
	mutexes map[lntypes.Hash]*cntMutex

	// mapMtx is used to give synchronize concurrent access
	// to the mutexes map.
	mapMtx sync.Mutex
}

// NewHashMutex creates a new Mutex.
func NewHashMutex() *HashMutex {
	return &HashMutex{
		mutexes: make(map[lntypes.Hash]*cntMutex),
	}
}

// Lock locks the mutex by the given hash. If the mutex is already
// locked by this hash, Lock blocks until the mutex is available.
func (c *HashMutex) Lock(hash lntypes.Hash) {
	c.mapMtx.Lock()
	mtx, ok := c.mutexes[hash]
	if ok {
		// If the mutex already existed in the map, we
		// increment its counter, to indicate that there
		// now is one more goroutine waiting for it.
		mtx.cnt++
	} else {
		// If it was not in the map, it means no other
		// goroutine has locked the mutex for this hash,
		// and we can create a new mutex with count 1
		// and add it to the map.
		mtx = &cntMutex{
			cnt: 1,
		}
		c.mutexes[hash] = mtx
	}
	c.mapMtx.Unlock()

	// Acquire the mutex for this hash.
	mtx.Lock()
}

// Unlock unlocks the mutex by the given hash. It is a run-time
// error if the mutex is not locked by the hash on entry to Unlock.
func (c *HashMutex) Unlock(hash lntypes.Hash) {
	// Since we are done with all the work for this
	// update, we update the map to reflect that.
	c.mapMtx.Lock()

	mtx, ok := c.mutexes[hash]
	if !ok {
		// The mutex not existing in the map means
		// an unlock for an hash not currently locked
		// was attempted.
		panic(fmt.Sprintf("double unlock for hash %v",
			hash))
	}

	// Decrement the counter. If the count goes to
	// zero, it means this caller was the last one
	// to wait for the mutex, and we can delete it
	// from the map. We can do this safely since we
	// are under the mapMtx, meaning that all other
	// goroutines waiting for the mutex already
	// have incremented it, or will create a new
	// mutex when they get the mapMtx.
	mtx.cnt--
	if mtx.cnt == 0 {
		delete(c.mutexes, hash)
	}
	c.mapMtx.Unlock()

	// Unlock the mutex for this hash.
	mtx.Unlock()
}
