package graphdb

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// rejectFlags is a compact representation of various metadata stored by the
// reject cache about a particular channel.
type rejectFlags uint8

const (
	// rejectFlagExists is a flag indicating whether the channel exists,
	// i.e. the channel is open and has a recent channel update. If this
	// flag is not set, the channel is either a zombie or unknown.
	rejectFlagExists rejectFlags = 1 << iota

	// rejectFlagZombie is a flag indicating whether the channel is a
	// zombie, i.e. the channel is open but has no recent channel updates.
	rejectFlagZombie
)

// packRejectFlags computes the rejectFlags corresponding to the passed boolean
// values indicating whether the edge exists or is a zombie.
func packRejectFlags(exists, isZombie bool) rejectFlags {
	var flags rejectFlags
	if exists {
		flags |= rejectFlagExists
	}
	if isZombie {
		flags |= rejectFlagZombie
	}

	return flags
}

// unpack returns the booleans packed into the rejectFlags. The first indicates
// if the edge exists in our graph, the second indicates if the edge is a
// zombie.
func (f rejectFlags) unpack() (bool, bool) {
	return f&rejectFlagExists == rejectFlagExists,
		f&rejectFlagZombie == rejectFlagZombie
}

// rejectCacheEntry caches frequently accessed information about a channel,
// including the timestamps of its latest edge policies and whether or not the
// channel exists in the graph.
type rejectCacheEntry struct {
	// upd{1,2}Time are Unix timestamps for v1 policies.
	upd1Time int64
	upd2Time int64

	// upd{1,2}BlockHeight are the last known block heights for v2
	// policies.
	upd1BlockHeight uint32
	upd2BlockHeight uint32

	flags rejectFlags
}

// newRejectCacheEntryV1 constructs a reject cache entry for v1 policies.
func newRejectCacheEntryV1(upd1, upd2 time.Time, exists,
	isZombie bool) rejectCacheEntry {

	return rejectCacheEntry{
		upd1Time: upd1.Unix(),
		upd2Time: upd2.Unix(),
		flags:    packRejectFlags(exists, isZombie),
	}
}

// newRejectCacheEntryV2 constructs a reject cache entry for v2 policies.
func newRejectCacheEntryV2(upd1, upd2 uint32, exists,
	isZombie bool) rejectCacheEntry {

	return rejectCacheEntry{
		upd1BlockHeight: upd1,
		upd2BlockHeight: upd2,
		flags:           packRejectFlags(exists, isZombie),
	}
}

// updateRejectCacheEntryV1 updates the cached v1 timestamps.
func updateRejectCacheEntryV1(entry *rejectCacheEntry, isUpdate1 bool,
	lastUpdate time.Time) {

	if isUpdate1 {
		entry.upd1Time = lastUpdate.Unix()
	} else {
		entry.upd2Time = lastUpdate.Unix()
	}
}

// updateRejectCacheEntryV2 updates the cached v2 block heights.
func updateRejectCacheEntryV2(entry *rejectCacheEntry, isUpdate1 bool,
	blockHeight uint32) {

	if isUpdate1 {
		entry.upd1BlockHeight = blockHeight
	} else {
		entry.upd2BlockHeight = blockHeight
	}
}

// rejectCacheKey uniquely identifies a channel entry in the reject cache by
// gossip version and channel ID. This allows v1 and v2 policy state for the
// same channel ID to be cached independently.
type rejectCacheKey struct {
	version lnwire.GossipVersion
	chanID  uint64
}

// rejectCache is an in-memory cache used to improve the performance of
// HasChannelEdge. It caches information about the whether or channel exists, as
// well as the most recent timestamps for each policy (if they exists).
type rejectCache struct {
	n     int
	edges map[rejectCacheKey]rejectCacheEntry
}

// newRejectCache creates a new rejectCache with maximum capacity of n entries.
func newRejectCache(n int) *rejectCache {
	return &rejectCache{
		n:     n,
		edges: make(map[rejectCacheKey]rejectCacheEntry, n),
	}
}

// get returns the entry from the cache for chanid, if it exists.
func (c *rejectCache) get(version lnwire.GossipVersion, chanid uint64) (
	rejectCacheEntry, bool) {

	entry, ok := c.edges[rejectCacheKey{
		version: version,
		chanID:  chanid,
	}]
	return entry, ok
}

// insert adds the entry to the reject cache. If an entry for chanid already
// exists, it will be replaced with the new entry. If the entry doesn't exists,
// it will be inserted to the cache, performing a random eviction if the cache
// is at capacity.
func (c *rejectCache) insert(version lnwire.GossipVersion, chanid uint64,
	entry rejectCacheEntry) {

	key := rejectCacheKey{
		version: version,
		chanID:  chanid,
	}

	// If entry exists, replace it.
	if _, ok := c.edges[key]; ok {
		c.edges[key] = entry
		return
	}

	// Otherwise, evict an entry at random and insert.
	if len(c.edges) == c.n {
		for id := range c.edges {
			delete(c.edges, id)
			break
		}
	}
	c.edges[key] = entry
}

// remove deletes an entry for chanid from the cache, if it exists.
func (c *rejectCache) remove(version lnwire.GossipVersion, chanid uint64) {
	delete(c.edges, rejectCacheKey{
		version: version,
		chanID:  chanid,
	})
}
