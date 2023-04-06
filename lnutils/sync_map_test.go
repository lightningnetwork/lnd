package lnutils_test

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/stretchr/testify/require"
)

// TestSyncMapStore tests the Store method of the SyncMap type.
func TestSyncMapStore(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap of string keys and integer values.
	m := &lnutils.SyncMap[string, int]{}

	// Test storing a new key-value pair.
	m.Store("foo", 42)
	value, ok := m.Load("foo")
	require.True(t, ok)
	require.Equal(t, 42, value)

	// Test overwriting an existing key-value pair.
	m.Store("foo", 99)
	value, ok = m.Load("foo")
	require.True(t, ok)
	require.Equal(t, 99, value)
}

// TestSyncMapLoad tests the Load method of the SyncMap type.
func TestSyncMapLoad(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap of string keys and integer values.
	m := &lnutils.SyncMap[string, int]{}

	// Add some key-value pairs to the map.
	m.Store("foo", 42)
	m.Store("bar", 99)

	// Test loading an existing key-value pair.
	value, ok := m.Load("foo")
	require.True(t, ok)
	require.Equal(t, 42, value)

	// Test loading a non-existing key-value pair.
	value, ok = m.Load("baz")
	require.False(t, ok)
	require.Equal(t, 0, value)
}

// TestSyncMapDelete tests the Delete method of the SyncMap type.
func TestSyncMapDelete(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap of string keys and integer values.
	m := &lnutils.SyncMap[string, int]{}

	// Add some key-value pairs to the map.
	m.Store("foo", 42)
	m.Store("bar", 99)

	// Test deleting an existing key-value pair.
	m.Delete("foo")
	_, ok := m.Load("foo")
	require.False(t, ok)

	// Test deleting a non-existing key-value pair.
	m.Delete("baz")
	_, ok = m.Load("baz")
	require.False(t, ok)
}

// TestSyncMapLoadAndDelete tests the LoadAndDelete method of the SyncMap type.
func TestSyncMapLoadAndDelete(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap of string keys and integer values.
	m := &lnutils.SyncMap[string, int]{}

	// Add some key-value pairs to the map.
	m.Store("foo", 42)
	m.Store("bar", 99)

	// Test loading and deleting an existing key-value pair.
	value, ok := m.LoadAndDelete("foo")
	require.True(t, ok)
	require.Equal(t, 42, value)

	// Verify that the pair was deleted from the map.
	_, ok = m.Load("foo")
	require.False(t, ok)

	// Test loading and deleting a non-existing key-value pair.
	value, ok = m.LoadAndDelete("baz")
	require.False(t, ok)
	require.Equal(t, 0, value)

	// Verify that the map is unchanged.
	require.Equal(t, 1, m.Len())
}

// TestSyncMapRange tests the Range method of the SyncMap type.
func TestSyncMapRange(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap and populate it with some values.
	m := &lnutils.SyncMap[int, string]{}
	m.Store(1, "one")
	m.Store(2, "two")
	m.Store(3, "three")

	// Use Range to iterate over the map.
	visited := 0
	m.Range(func(key int, value string) bool {
		visited++

		return visited != 2
	})

	// Check we've only visited twice.
	require.Equal(t, 2, visited)
}

// TestSyncMapRange tests the ForEach method of the SyncMap.
func TestSyncMapForEach(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap and add some items to it.
	m := &lnutils.SyncMap[int, string]{}
	m.Store(1, "one")
	m.Store(2, "two")
	m.Store(3, "three")

	// Define the visitor function that will be applied to each item.
	visited := 0
	visitor := func(key int, value string) error {
		visited++
		if visited == 2 {
			// Return an error to stop the iteration.
			return errors.New("stop iteration")
		}

		return nil
	}

	// Apply the visitor function to each item in the SyncMap.
	m.ForEach(visitor)

	// Verify that the iteration was stopped because of the error returned
	// by the visitor function.
	require.Equal(t, 2, visited)
}

// TestSyncMapRange tests the Len method of the SyncMap.
func TestSyncMapLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a new SyncMap instance.
	m := &lnutils.SyncMap[int, string]{}

	// Add a few items to the map.
	m.Store(1, "foo")
	m.Store(2, "bar")
	m.Store(3, "baz")

	// Check that the map length is correct.
	require.Equal(3, m.Len())

	// Remove an item from the map.
	m.Delete(2)

	// Check that the map length is updated.
	require.Equal(2, m.Len())
}

// TestSyncMapRange tests the LoadOrStore method of the SyncMap.
func TestSyncMapLoadOrStore(t *testing.T) {
	t.Parallel()

	// Create a new SyncMap.
	sm := &lnutils.SyncMap[int, string]{}

	// Test loading non-existent items.
	item, loaded := sm.LoadOrStore(1, "one")
	require.False(t, loaded)
	require.Equal(t, "one", item)

	item, loaded = sm.LoadOrStore(2, "two")
	require.False(t, loaded)
	require.Equal(t, "two", item)

	// Test loading existing items.
	item, loaded = sm.LoadOrStore(1, "new one")
	require.True(t, loaded)
	require.Equal(t, "one", item)

	item, loaded = sm.LoadOrStore(2, "new two")
	require.True(t, loaded)
	require.Equal(t, "two", item)
}
