package lncfg

import "fmt"

const (
	// MinRejectCacheSize is a floor on the maximum capacity allowed for
	// channeldb's reject cache. This amounts to roughly 125 KB when full.
	MinRejectCacheSize = 5000

	// MinChannelCacheSize is a floor on the maximum capacity allowed for
	// channeldb's channel cache. This amounts to roughly 2 MB when full.
	MinChannelCacheSize = 1000
)

// Caches holds the configuration for various caches within lnd.
type Caches struct {
	// RejectCacheSize is the maximum number of entries stored in lnd's
	// reject cache, which is used for efficiently rejecting gossip updates.
	// Memory usage is roughly 25b per entry.
	RejectCacheSize int `long:"reject-cache-size" description:"Maximum number of entries contained in the reject cache, which is used to speed up filtering of new channel announcements and channel updates from peers. Each entry requires 25 bytes."`

	// ChannelCacheSize is the maximum number of entries stored in lnd's
	// channel cache, which is used reduce memory allocations in reply to
	// peers querying for gossip traffic. Memory usage is roughly 2Kb per
	// entry.
	ChannelCacheSize int `long:"channel-cache-size" description:"Maximum number of entries contained in the channel cache, which is used to reduce memory allocations from gossip queries from peers. Each entry requires roughly 2Kb."`
}

// Validate checks the Caches configuration for values that are too small to be
// sane.
func (c *Caches) Validate() error {
	if c.RejectCacheSize < MinRejectCacheSize {
		return fmt.Errorf("reject cache size %d is less than min: %d",
			c.RejectCacheSize, MinRejectCacheSize)
	}
	if c.ChannelCacheSize < MinChannelCacheSize {
		return fmt.Errorf("channel cache size %d is less than min: %d",
			c.ChannelCacheSize, MinChannelCacheSize)
	}

	return nil
}

// Compile-time constraint to ensure Caches implements the Validator interface.
var _ Validator = (*Caches)(nil)
