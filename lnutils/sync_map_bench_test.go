package lnutils_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/lnutils"
)

func BenchmarkReadMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a general mutex.
	var mu sync.Mutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock read.
			mu.Lock()
			_ = m[k]
			mu.Unlock()
		}
	})
}

func BenchmarkReadRWMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a read write mutex.
	var mu sync.RWMutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock read.
			mu.RLock()
			_ = m[k]
			mu.RUnlock()
		}
	})
}

func BenchmarkReadSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &sync.Map{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Read the value.
			syncMap.Load(k)
		}
	})
}

func BenchmarkReadLndSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &lnutils.SyncMap[int64, struct{}]{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Read the value.
			syncMap.Load(k)
		}
	})
}

func BenchmarkWriteMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a general mutex.
	var mu sync.Mutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock write.
			mu.Lock()
			m[k] = struct{}{}
			mu.Unlock()
		}
	})
}

func BenchmarkWriteRWMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a read write mutex.
	var mu sync.RWMutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock write.
			mu.Lock()
			m[k] = struct{}{}
			mu.Unlock()
		}
	})
}

func BenchmarkWriteSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &sync.Map{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Write the value.
			syncMap.Store(k, struct{}{})
		}
	})
}

func BenchmarkWriteLndSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &lnutils.SyncMap[int64, struct{}]{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Write the value.
			syncMap.Store(k, struct{}{})
		}
	})
}

func BenchmarkDeleteMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a general mutex.
	var mu sync.Mutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock delete.
			mu.Lock()
			delete(m, k)
			mu.Unlock()
		}
	})
}

func BenchmarkDeleteRWMutexMap(b *testing.B) {
	// Create a map with a mutex.
	m := make(map[int64]struct{})

	// k is the unique key for each goroutine.
	k := int64(0)

	// Create a read write mutex.
	var mu sync.RWMutex

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Perform a lock delete.
			mu.Lock()
			delete(m, k)
			mu.Unlock()
		}
	})
}

func BenchmarkDeleteSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &sync.Map{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Delete the value.
			syncMap.Delete(k)
		}
	})
}

func BenchmarkDeleteLndSyncMap(b *testing.B) {
	// Create a sync.Map.
	syncMap := &lnutils.SyncMap[int64, struct{}]{}

	// k is the unique key for each goroutine.
	k := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Increment k.
			atomic.AddInt64(&k, 1)

			// Delete the value.
			syncMap.Delete(k)
		}
	})
}
