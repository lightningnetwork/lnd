package channeldb

import (
	"reflect"
	"testing"
)

// TestEdgeCache checks the behavior of the edgeCache with respect to insertion,
// eviction, and removal of cache entries.
func TestEdgeCache(t *testing.T) {
	const cacheSize = 100

	// Create a new edge cache with the configured max size.
	c := newEdgeCache(cacheSize)

	// As a sanity check, assert that querying the empty cache does not
	// return an entry.
	_, ok := c.get(0)
	if ok {
		t.Fatalf("edge cache should be empty")
	}

	// Now, fill up the cache entirely.
	for i := uint64(0); i < cacheSize; i++ {
		c.insert(i, entryForInt(i))
	}

	// Assert that the cache has all of the entries just inserted, since no
	// eviction should occur until we try to surpass the max size.
	assertHasEntries(t, c, 0, cacheSize)

	// Now, insert a new element that causes the cache to evict an element.
	c.insert(cacheSize, entryForInt(cacheSize))

	// Assert that the cache has this last entry, as the cache should evict
	// some prior element and not the newly inserted one.
	assertHasEntries(t, c, cacheSize, cacheSize)

	// Iterate over all inserted elements and construct a set of the evicted
	// elements.
	evicted := make(map[uint64]struct{})
	for i := uint64(0); i < cacheSize+1; i++ {
		_, ok := c.get(i)
		if !ok {
			evicted[i] = struct{}{}
		}
	}

	// Assert that exactly one element has been evicted.
	numEvicted := len(evicted)
	if numEvicted != 1 {
		t.Fatalf("expected one evicted entry, got: %d", numEvicted)
	}

	// Remove the highest item which initially caused the eviction and
	// reinsert the element that was evicted prior.
	c.remove(cacheSize)
	for i := range evicted {
		c.insert(i, entryForInt(i))
	}

	// Since the removal created an extra slot, the last insertion should
	// not have caused an eviction and the entries for all channels in the
	// original set that filled the cache should be present.
	assertHasEntries(t, c, 0, cacheSize)

	// Finally, reinsert the existing set back into the cache and test that
	// the cache still has all the entries. If the randomized eviction were
	// happening on inserts for existing cache items, we expect this to fail
	// with high probability.
	for i := uint64(0); i < cacheSize; i++ {
		c.insert(i, entryForInt(i))
	}
	assertHasEntries(t, c, 0, cacheSize)

}

// assertHasEntries queries the edge cache for all channels in the range [start,
// end), asserting that they exist and their value matches the entry produced by
// entryForInt.
func assertHasEntries(t *testing.T, c *edgeCache, start, end uint64) {
	t.Helper()

	for i := start; i < end; i++ {
		entry, ok := c.get(i)
		if !ok {
			t.Fatalf("edge cache should contain chan %d", i)
		}

		expEntry := entryForInt(i)
		if !reflect.DeepEqual(entry, expEntry) {
			t.Fatalf("entry mismatch, want: %v, got: %v",
				expEntry, entry)
		}
	}
}

// entryForInt generates a unique edgeCacheEntry given an integer.
func entryForInt(i uint64) edgeCacheEntry {
	return edgeCacheEntry{
		upd1Time: uint32(2 * i),
		upd2Time: uint32(2*i + 1),
		exists:   i%2 == 0,
	}
}
