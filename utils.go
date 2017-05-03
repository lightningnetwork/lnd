package main

import (
	"sync"
	"sync/atomic"
)

// CountableMutex lets the mutex lock/unlock operations to be countable, this
// functionality is needed to properly delete mutex.
type CountableMutex struct {
	lock  *sync.Mutex
	count int32
}

func NewCountableMutex() *CountableMutex {
	return &CountableMutex{
		lock:  &sync.Mutex{},
		count: 0,
	}
}

func (m *CountableMutex) Lock() {
	atomic.AddInt32(&m.count, 1)
	m.lock.Lock()
}

func (m *CountableMutex) Unlock() {
	m.lock.Unlock()
	atomic.AddInt32(&m.count, -1)
}

func (m *CountableMutex) Used() bool {
	if atomic.LoadInt32(&m.count) != 0 {
		return true
	}
	return false
}

// ConcurrentMap structure adds ability for the map to be safely used in
// multiple goroutines simulteniosly. The additional lock/unlock methods helps
// lock only one element rather then overall map structure. This structure
// should only be used where the fast access to the map structure isn't crucial
// necessity.
type ConcurrentMap struct {
	lock    sync.RWMutex
	mutexes map[int]*CountableMutex
	data    map[int]interface{}
}

func NewConcurrentMap() *ConcurrentMap {
	return &ConcurrentMap{
		lock:    sync.RWMutex{},
		mutexes: make(map[int]*CountableMutex),
		data:    make(map[int]interface{}),
	}
}

func (cm *ConcurrentMap) Get(key int) (interface{}, bool) {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	if elem, present := cm.data[key]; !present {
		return nil, false
	} else {
		return elem, true
	}
}

func (cm *ConcurrentMap) Set(key int, elem interface{}) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.data[key] = elem
}

func (cm *ConcurrentMap) Remove(key int) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	delete(cm.data, key)
}

func (cm *ConcurrentMap) Len() int {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	return len(cm.data)
}

func (cm *ConcurrentMap) Lock(key int) {
	mutex := cm.getMutexByKey(key)
	mutex.Lock()
}

func (cm *ConcurrentMap) Unlock(key int) {
	mutex := cm.getMutexByKey(key)
	mutex.lock.Unlock()

	// Delete the mutex if nobody uses it.
	cm.lock.Lock()
	if !mutex.Used() {
		delete(cm.mutexes, key)
	}
	cm.lock.Unlock()
}

func (cm *ConcurrentMap) Iterate(key int) map[int]interface{} {
	cm.lock.RLock()
	defer cm.lock.RUnlock()

	newMap := make(map[int]interface{}, len(cm.data))
	for k, v := range cm.data {
		newMap[k] = v
	}

	return newMap
}

// getMutexByKey function creates/gets the mutex by the key. This code is
// stolen from Catena[1] project. For more information about the
// optimization decisions please see the Preetam Jinka article[2].
// [1] https://github.com/Cistern/catena
// [2] https://www.misfra.me/optimizing-concurrent-map-access-in-go
func (cm *ConcurrentMap) getMutexByKey(key int) *CountableMutex {
	var mutex *CountableMutex
	var present bool

	cm.lock.RLock()
	if mutex, present = cm.mutexes[key]; !present {
		cm.lock.RUnlock()

		cm.lock.Lock()
		if mutex, present = cm.mutexes[key]; !present {
			mutex = NewCountableMutex()
			cm.mutexes[key] = mutex
		}
		cm.lock.Unlock()

	} else {
		cm.lock.RUnlock()
	}

	return mutex
}
