package lnutils

import "sync"

// SyncMap wraps a sync.Map with type parameters such that it's easier to
// access the items stored in the map since no type assertion is needed. It
// also requires explicit type definition when declaring and initiating the
// variables, which helps us understanding what's stored in a given map.
type SyncMap[K comparable, V any] struct {
	sync.Map
}

// Store puts an item in the map.
func (m *SyncMap[K, V]) Store(key K, value V) {
	m.Map.Store(key, value)
}

// Load queries an item from the map using the specified key. If the item
// cannot be found, an empty value and false will be returned. If the stored
// item fails the type assertion, a nil value and false will be returned.
func (m *SyncMap[K, V]) Load(key K) (V, bool) {
	result, ok := m.Map.Load(key)
	if !ok {
		return *new(V), false // nolint: gocritic
	}

	item, ok := result.(V)
	return item, ok
}

// Delete removes an item from the map specified by the key.
func (m *SyncMap[K, V]) Delete(key K) {
	m.Map.Delete(key)
}

// LoadAndDelete queries an item and deletes it from the map using the
// specified key.
func (m *SyncMap[K, V]) LoadAndDelete(key K) (V, bool) {
	result, loaded := m.Map.LoadAndDelete(key)
	if !loaded {
		return *new(V), loaded // nolint: gocritic
	}

	item, ok := result.(V)
	return item, ok
}

// Range iterates the map.
func (m *SyncMap[K, V]) Range(visitor func(K, V) bool) {
	m.Map.Range(func(k any, v any) bool {
		return visitor(k.(K), v.(V))
	})
}
