package multimutex_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/multimutex"
	"github.com/stretchr/testify/require"
)

// newTestMutex initializes a new mutex.
func newTestMutex[T comparable]() *multimutex.Mutex[T] {
	return multimutex.NewMutex[T]()
}

// TestMultiMutexLockUnlock verifies basic locking/unlocking functionality.
func TestMultiMutexLockUnlock(t *testing.T) {
	t.Parallel()
	mtx := newTestMutex[string]()

	// Lock and unlock the mutex with a single ID.
	mtx.Lock("id1")
	mtx.Unlock("id1")

	// Lock and unlock with multiple IDs.
	mtx.Lock("id1")
	mtx.Lock("id2")
	mtx.Unlock("id1")
	mtx.Unlock("id2")
}

// TestMultiMutexConcurrency validates proper handling of concurrent access.
func TestMultiMutexConcurrency(t *testing.T) {
	t.Parallel()
	mtx := newTestMutex[string]()

	var counter int32
	var wg sync.WaitGroup
	const numOps = 100

	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			mtx.Lock("shared")
			current := counter

			// Simulate some work to check atomicity.
			time.Sleep(time.Millisecond)
			counter = current + 1
			mtx.Unlock("shared")
		}()
	}

	wg.Wait()
	require.Equal(t, int32(numOps), counter)
}

// TestMultiMutexMultipleIDs validates independent locking per ID.
func TestMultiMutexMultipleIDs(t *testing.T) {
	t.Parallel()
	mtx := newTestMutex[string]()

	counters := map[string]*int32{
		"id1": new(int32),
		"id2": new(int32),
	}
	var wg sync.WaitGroup
	const numOps = 50

	for i := 0; i < numOps; i++ {
		wg.Add(2)

		go func(id string) {
			defer wg.Done()

			mtx.Lock(id)
			defer mtx.Unlock(id)

			current := *counters[id]

			// Simulate some work to check atomicity.
			time.Sleep(time.Millisecond)
			*counters[id] = current + 1
		}("id1")

		go func(id string) {
			defer wg.Done()

			mtx.Lock(id)
			defer mtx.Unlock(id)

			current := *counters[id]

			// Simulate some work to check atomicity.
			time.Sleep(time.Millisecond)
			*counters[id] = current + 1
		}("id2")
	}

	wg.Wait()
	require.Equal(t, int32(numOps), *counters["id1"])
	require.Equal(t, int32(numOps), *counters["id2"])
}

// TestMultiMutexReuse validates mutex reuse after unlocking.
func TestMultiMutexReuse(t *testing.T) {
	t.Parallel()
	mtx := newTestMutex[int]()

	for i := 0; i < 5; i++ {
		mtx.Lock(1)
		mtx.Unlock(1)
	}

	for i := 1; i <= 5; i++ {
		mtx.Lock(i)
	}
	for i := 1; i <= 5; i++ {
		mtx.Unlock(i)
	}
}

// TestMultiMutexPanic validates proper panic behavior on invalid unlocks.
func TestMultiMutexPanic(t *testing.T) {
	t.Parallel()

	t.Run("unlock never-locked ID", func(t *testing.T) {
		mtx := newTestMutex[int]()
		require.Panics(t, func() { mtx.Unlock(1) })
	})

	t.Run("double unlock", func(t *testing.T) {
		mtx := newTestMutex[int]()
		mtx.Lock(1)
		mtx.Unlock(1)
		require.Panics(t, func() { mtx.Unlock(1) })
	})
}

// TestMultiMutexCustomType validates compatibility with custom types.
func TestMultiMutexCustomType(t *testing.T) {
	t.Parallel()

	type CustomID struct {
		ID   string
		Type int
	}

	mtx := newTestMutex[CustomID]()
	id1, id2 := CustomID{"test", 1}, CustomID{"test", 2}

	mtx.Lock(id1)
	mtx.Lock(id2)
	mtx.Unlock(id1)
	mtx.Unlock(id2)
}
