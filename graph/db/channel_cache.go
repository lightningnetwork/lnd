package graphdb

import "github.com/lightningnetwork/lnd/lnwire"

// channelCacheKey uniquely identifies a channel entry in the channel cache by
// gossip version and channel ID.
type channelCacheKey struct {
	version lnwire.GossipVersion
	chanID  uint64
}

// channelCache is an in-memory cache used to improve the performance of
// ChanUpdatesInHorizon. It caches the chan info and edge policies for a
// particular channel.
type channelCache struct {
	n        int
	channels map[channelCacheKey]ChannelEdge
}

// newChannelCache creates a new channelCache with maximum capacity of n
// channels.
func newChannelCache(n int) *channelCache {
	return &channelCache{
		n:        n,
		channels: make(map[channelCacheKey]ChannelEdge),
	}
}

// get returns the channel from the cache, if it exists.
func (c *channelCache) get(version lnwire.GossipVersion,
	chanid uint64) (ChannelEdge, bool) {

	channel, ok := c.channels[channelCacheKey{
		version: version,
		chanID:  chanid,
	}]
	return channel, ok
}

// insert adds the entry to the channel cache. If an entry for chanid already
// exists, it will be replaced with the new entry. If the entry doesn't exist,
// it will be inserted to the cache, performing a random eviction if the cache
// is at capacity.
func (c *channelCache) insert(version lnwire.GossipVersion, chanid uint64,
	channel ChannelEdge) {

	key := channelCacheKey{
		version: version,
		chanID:  chanid,
	}

	// If entry exists, replace it.
	if _, ok := c.channels[key]; ok {
		c.channels[key] = channel
		return
	}

	// Otherwise, evict an entry at random and insert.
	if len(c.channels) == c.n {
		for id := range c.channels {
			delete(c.channels, id)
			break
		}
	}
	c.channels[key] = channel
}

// remove deletes an edge for chanid from the cache, if it exists.
func (c *channelCache) remove(version lnwire.GossipVersion, chanid uint64) {
	delete(c.channels, channelCacheKey{
		version: version,
		chanID:  chanid,
	})
}
