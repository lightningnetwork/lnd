package peer

import (
	"sync/atomic"

	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/lightningnetwork/lnd/lnwire"
)

// cachedlnwireMsg is an early message that's saved to the `earlyMsgCache`.
type cachedlnwireMsg struct {
	// msg is the network message.
	msg lnwire.Message
}

// Size returns the size of the message.
func (c *cachedlnwireMsg) Size() (uint64, error) {
	// Return a constant 1.
	return 1, nil
}

// lnwireMsgCache embeds a `lru.Cache` with a message counter that's served as
// the unique ID when saving the message.
//
// TODO(yy): create a generic `cacheWithID[uint64, V]`?
type lnwireMsgCache struct {
	*lru.Cache[uint64, *cachedlnwireMsg]

	// msgID is a monotonically increased integer.
	msgID atomic.Uint64
}

// nextMsgID returns a unique message ID.
func (l *lnwireMsgCache) nextMsgID() uint64 {
	return l.msgID.Add(1)
}

// addMsg adds a cachedEarlyMsg with an automatically generated msgID.
func (l *lnwireMsgCache) addMsg(msg *cachedlnwireMsg) error {
	id := l.nextMsgID()
	_, err := l.Put(id, msg)

	return err
}

// Size returns the length of the cache.
//
// NOTE: part of the `cache.Value` interface.
func (l *lnwireMsgCache) Size() (uint64, error) {
	// Each lnwireMsgCache is a single message, so we return 1.
	return 1, nil
}

// newLnwireMsgCache creates a new lnwire message cache with the underlying lru
// cache being initialized with the specified capacity.
func newLnwireMsgCache(capacity uint64) *lnwireMsgCache {
	// Create a new cache.
	cache := lru.NewCache[uint64, *cachedlnwireMsg](capacity)

	return &lnwireMsgCache{
		Cache: cache,
	}
}
