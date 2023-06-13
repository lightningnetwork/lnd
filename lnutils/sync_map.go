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

// Range iterates the map and applies the `visitor` function. If the `visitor`
// returns false, the iteration will be stopped.
func (m *SyncMap[K, V]) Range(visitor func(K, V) bool) {
	m.Map.Range(func(k any, v any) bool {
		return visitor(k.(K), v.(V))
	})
}

// ForEach iterates the map and applies the `visitor` function. Unlike the
// `Range` method, the `visitor` function will be applied to all the items
// unless there's an error.
func (m *SyncMap[K, V]) ForEach(visitor func(K, V) error) {
	// rangeVisitor wraps the `visitor` function and returns false if
	// there's an error returned from the `visitor` function.
	rangeVisitor := func(k K, v V) bool {
		if err := visitor(k, v); err != nil {
			// Break the iteration if there's an error.
			return false
		}

		return true
	}

	m.Range(rangeVisitor)
}

// Len returns the number of items in the map.
func (m *SyncMap[K, V]) Len() int {
	var count int
	m.Range(func(_ K, _ V) bool {
		count++

		return true
	})

	return count
}

// LoadOrStore queries an item from the map using the specified key. If the
// item cannot be found, the `value` will be stored in the map and returned.
// If the stored item fails the type assertion, a nil value and false will be
// returned.
func (m *SyncMap[K, V]) LoadOrStore(key K, value V) (V, bool) {
	result, loaded := m.Map.LoadOrStore(key, value)
	item, ok := result.(V)
	if !ok {
		return *new(V), false
	}

	return item, loaded
}
