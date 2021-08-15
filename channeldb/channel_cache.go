package channeldb

import "github.com/lightningnetwork/lnd/routing/route"

// channelCache is an in-memory cache used to improve the performance of
// ChanUpdatesInHorizon. It caches the chan info and edge policies for a
// particular channel.
type channelCache struct {
	n        int
	channels map[uint64]ChannelEdge

	nodeChannels map[route.Vertex][]ChannelEdge
}

// newChannelCache creates a new channelCache with maximum capacity of n
// channels.
func newChannelCache(n int) *channelCache {
	return &channelCache{
		n:        n,
		channels: make(map[uint64]ChannelEdge),

		nodeChannels: make(map[route.Vertex][]ChannelEdge),
	}
}

// get returns the channel from the cache, if it exists.
func (c *channelCache) get(chanid uint64) (ChannelEdge, bool) {
	channel, ok := c.channels[chanid]
	return channel, ok
}

func (c *channelCache) getNodeChannels(node route.Vertex) ([]ChannelEdge, bool) {
	channels, ok := c.nodeChannels[node]
	return channels, ok
}

func (c *channelCache) insertNodeChannels(node route.Vertex, channels []ChannelEdge) {
	// TODO: Manage cache capacity.

	c.nodeChannels[node] = channels
}

// insert adds the entry to the channel cache. If an entry for chanid already
// exists, it will be replaced with the new entry. If the entry doesn't exist,
// it will be inserted to the cache, performing a random eviction if the cache
// is at capacity.
func (c *channelCache) insert(chanid uint64, channel ChannelEdge) {
	if _, ok := c.channels[chanid]; !ok {
		if len(c.channels) == c.n {
			for id := range c.channels {
				channel := c.channels[id]

				delete(c.channels, id)

				// Invalidate node channels cache.
				delete(c.nodeChannels, channel.Info.NodeKey1Bytes)
				delete(c.nodeChannels, channel.Info.NodeKey2Bytes)

				break
			}
		}
	}

	c.channels[chanid] = channel

	// Invalidate node channels cache.
	delete(c.nodeChannels, channel.Info.NodeKey1Bytes)
	delete(c.nodeChannels, channel.Info.NodeKey2Bytes)
}

// remove deletes an edge for chanid from the cache, if it exists.
func (c *channelCache) remove(chanid uint64) {
	channel, ok := c.channels[chanid]
	if !ok {
		return
	}

	delete(c.channels, chanid)

	// Invalidate node channels cache.
	delete(c.nodeChannels, channel.Info.NodeKey1Bytes)
	delete(c.nodeChannels, channel.Info.NodeKey2Bytes)
}
