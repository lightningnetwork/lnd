package chanstate

import "github.com/lightningnetwork/lnd/kvdb"

// KVStore is the KV-backed implementation of the channel-state store facets.
// Store facets are moved onto this type incrementally while channeldb keeps
// compatibility wrappers for callers that still depend on the old package.
type KVStore struct {
	backend kvdb.Backend
}

// NewKVStore creates a KV-backed channel-state store.
func NewKVStore(backend kvdb.Backend) *KVStore {
	return &KVStore{
		backend: backend,
	}
}

var _ ChannelSetupStore = (*KVStore)(nil)
