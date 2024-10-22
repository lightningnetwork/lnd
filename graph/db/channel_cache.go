package graphdb

// channelCache is an in-memory cache used to improve the performance of
// ChanUpdatesInHorizon. It caches the chan info and edge policies for a
// particular channel.
type channelCache struct {
	n        int
	channels map[uint64]ChannelEdge
}

// newChannelCache creates a new channelCache with maximum capacity of n
// channels.
func newChannelCache(n int) *channelCache {
	return &channelCache{
		n:        n,
		channels: make(map[uint64]ChannelEdge),
	}
}

// get returns the channel from the cache, if it exists.
func (c *channelCache) get(chanid uint64) (ChannelEdge, bool) {
	channel, ok := c.channels[chanid]
	return channel, ok
}

// insert adds the entry to the channel cache. If an entry for chanid already
// exists, it will be replaced with the new entry. If the entry doesn't exist,
// it will be inserted to the cache, performing a random eviction if the cache
// is at capacity.
func (c *channelCache) insert(chanid uint64, channel ChannelEdge) {
	// If entry exists, replace it.
	if _, ok := c.channels[chanid]; ok {
		c.channels[chanid] = channel
		return
	}

	// Otherwise, evict an entry at random and insert.
	if len(c.channels) == c.n {
		for id := range c.channels {
			delete(c.channels, id)
			break
		}
	}
	c.channels[chanid] = channel
}

// remove deletes an edge for chanid from the cache, if it exists.
func (c *channelCache) remove(chanid uint64) {
	delete(c.channels, chanid)
}
