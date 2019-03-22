package channeldb

// edgeCacheEntry caches frequently accessed information about a channel,
// including the timestamps of its latest edge policies and whether or not the
// channel exists in the graph.
type edgeCacheEntry struct {
	upd1Time uint32
	upd2Time uint32
	exists   bool
}

// edgeCache is an in-memory cache used to improve the performance of
// HasChannelEdge. It caches information about the whether or channel exists, as
// well as the most recent timestamps for each policy (if they exists).
type edgeCache struct {
	n     int
	edges map[uint64]edgeCacheEntry
}

// newEdgeCache creates a new edgeCache with maximum capacity of n entries.
func newEdgeCache(n int) *edgeCache {
	return &edgeCache{
		n:     n,
		edges: make(map[uint64]edgeCacheEntry, n),
	}
}

// get returns the entry from the cache for chanid, if it exists.
func (c *edgeCache) get(chanid uint64) (edgeCacheEntry, bool) {
	entry, ok := c.edges[chanid]
	return entry, ok
}

// insert adds the entry to the edge cache. If an entry for chanid already
// exists, it will be replaced with the new entry. If the entry doesn't exists,
// it will be inserted to the cache, performing a random eviction if the cache
// is at capacity.
func (c *edgeCache) insert(chanid uint64, entry edgeCacheEntry) {
	// If entry exists, replace it.
	if _, ok := c.edges[chanid]; ok {
		c.edges[chanid] = entry
		return
	}

	// Otherwise, evict an entry at random and insert.
	if len(c.edges) == c.n {
		for id := range c.edges {
			delete(c.edges, id)
			break
		}
	}
	c.edges[chanid] = entry
}

// remove deletes an entry for chanid from the cache, if it exists.
func (c *edgeCache) remove(chanid uint64) {
	delete(c.edges, chanid)
}
