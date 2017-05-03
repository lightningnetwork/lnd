package main

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSequentialKeyAccess check that the lock of the different keys don't
// produce locking problem.
func TestSequentialKeyAccess(t *testing.T) {
	if runtime.GOMAXPROCS(2) < 2 {
		t.Fatal("GOMAXPROCS should be greater or equal to '2'")
	}

	cm := NewConcurrentMap()
	firstKey := 1
	secondKey := 2

	waitAndExit := func(f func()) {
		select {
		case <-time.After(time.Second * 2):
			t.Fatal("timeout")
		default:
			f()
		}
	}

	waitAndExit(func() {
		cm.Lock(firstKey)
	})

	waitAndExit(func() {
		cm.Lock(secondKey)
	})

	waitAndExit(func() {
		cm.Unlock(firstKey)
	})

	waitAndExit(func() {
		cm.Unlock(secondKey)
	})
}

func TestMutexCountability(t *testing.T) {
	cm := NewCountableMutex()

	if cm.Lock(); !cm.Used() {
		t.Fatal("Mutex should be used")
	}

	if cm.Unlock(); cm.Used() {
		t.Fatal("Mutex shouldn't be used")
	}
}

// TestConcurrentElementIncrement checks that concurrent element incrementation
// don't produce data corruption and panic errors.
func TestConcurrentElementIncrement(t *testing.T) {
	// Setup parallel goroutine execution, otherwise we don't encounter
	// the parallel map access error.
	if runtime.GOMAXPROCS(2) < 2 {
		t.Fatal("GOMAXPROCS should be greater or equal to '2'")
	}

	// Init size and number of goroutines which will be spawned to
	// increment the map element value's
	size := 100
	n := 100

	// Create concurrent map and initaliaze it.
	cm := NewConcurrentMap()
	for i := 0; i < size; i++ {
		cm.Set(i, 0)
	}

	// Spawn number of goroutines which increments the elements value by
	// one, and wait them to finish.
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		wg.Add(1)

		go func() {
			for i := 0; i < size; i++ {
				cm.Lock(i)

				if elem, ok := cm.Get(i); !ok {
					cm.Unlock(i)
					t.Fatal("Can't find element")
				} else {
					cm.Set(i, elem.(int)+1)
				}

				cm.Unlock(i)
			}
			wg.Done()
		}()
	}
	wg.Wait()

	// Check that we have expected map values.
	for i := 0; i < size; i++ {
		if elem, ok := cm.Get(i); !ok {
			t.Fatal("Can't find element with %v key", i)
		} else if elem.(int) != n {
			t.Fatal("Value corruption was found: cm.Get(%v) != "+
				"%v", elem.(int), n)
		}
	}
}

// TestConcurrentElementIncrement another test which checks that concurrent
// element incrementation don't produce data corruption and panic errors.
func TestConcurrentElementAccess(t *testing.T) {
	// Setup parallel goroutine execution, otherwise we don't encounter
	// the parallel map access error.
	if runtime.GOMAXPROCS(2) < 2 {
		t.Fatal("GOMAXPROCS should be greater or equal to '2'")
	}

	key := 1

	// Create concurrent map and initialize it.
	cm := NewConcurrentMap()
	cm.Set(key, 0)

	// Concurrently set the same value trying to сause parallel map
	// write panic.
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				cm.Lock(key)
				if elem, ok := cm.Get(key); !ok {
					cm.Unlock(key)
					t.Fatal("Can't find element")
				} else {
					cm.Set(key, elem.(int))
					cm.Unlock(key)
				}
			}
		}
	}()

	// Check that locking of keys working properly, and parallel
	// goroutine can't access the key simulteniosly.
	checkAndIncrement := func(i int) {
		cm.Lock(key)
		defer cm.Unlock(key)

		if elem, ok := cm.Get(key); !ok {
			t.Fatal("Can't find element")
		} else {
			v := elem.(int)
			if v != i {
				t.Fatalf("Data is corrupted: %v != %v", v, i)
			} else {
				cm.Set(key, i+1)
			}
		}
	}

	for i := 0; i < 1000; i++ {
		checkAndIncrement(i)
	}

	close(done)
}

// TestConcurrentElementRemove checks the concurrent removing elements from
// map don't cause panic and corruption error.
func TestConcurrentElementRemove(t *testing.T) {
	// Setup parallel goroutine execution, otherwise we don't encounter
	// the parallel map access error.
	if runtime.GOMAXPROCS(2) < 2 {
		t.Fatal("GOMAXPROCS should be greater or equal to '2'")
	}

	// Init size and number of goroutines which will be spawned to
	// increments the map element value's
	size := 100

	// Creates concurrent map and initialise it.
	cm := NewConcurrentMap()
	for i := 0; i < size; i++ {
		cm.Set(i, 0)
	}

	// Spawn number of goroutines which increments the elements value by
	// one, and wait them to finish.
	var wg sync.WaitGroup
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func(i int) {
			cm.Remove(i)
			wg.Done()
		}(i)
	}
	wg.Wait()

	if cm.Len() != 0 {
		t.Fatalf("Not all elements was removed, %v != 0 ", cm.Len())
	}
}

// TestParallelUpdating checks parallel updating of different keys.
func TestParallelUpdating(t *testing.T) {
	// Setup parallel goroutine execution, otherwise we don't encounter
	// the parallel map access error.
	if runtime.GOMAXPROCS(2) < 2 {
		t.Fatal("GOMAXPROCS should be greater or equal to '2'")
	}

	// Create concurrent map and initialise it.
	cm := NewConcurrentMap()

	firstKey := 1
	secondKey := 2

	cm.Set(firstKey, 0)
	cm.Set(secondKey, 0)

	// Concurrently set the value in by different key trying to сause
	// parallel map write panic.
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				cm.Lock(firstKey)
				if elem, ok := cm.Get(firstKey); !ok {
					cm.Unlock(firstKey)
					t.Fatal("Can't find element")
				} else {
					cm.Set(firstKey, elem.(int))
					cm.Unlock(firstKey)
				}
			}
		}
	}()

	checkAndIncrement := func(i int) {
		cm.Lock(secondKey)
		defer cm.Unlock(secondKey)

		if elem, ok := cm.Get(secondKey); !ok {
			t.Fatal("Can't find element")
		} else {
			v := elem.(int)
			if v != i {
				t.Fatalf("Data is corrupted: %v != %v", v, i)
			} else {
				cm.Set(secondKey, i+1)
			}
		}
	}

	for i := 0; i < 1000; i++ {
		checkAndIncrement(i)
	}

	close(done)
}
