package chanstate

import "github.com/lightningnetwork/lnd/kvdb"

// KVStore is the KV-backed implementation of the channel-state store facets.
// Store facets are moved onto this type incrementally while channeldb keeps
// compatibility wrappers for callers that still depend on the old package.
type KVStore struct {
	backend                   kvdb.Backend
	storeFinalHtlcResolutions bool
}

// NewKVStore creates a KV-backed channel-state store.
func NewKVStore(backend kvdb.Backend,
	storeFinalHtlcResolutions bool) *KVStore {

	return &KVStore{
		backend:                   backend,
		storeFinalHtlcResolutions: storeFinalHtlcResolutions,
	}
}

var _ ChannelSetupStore = (*KVStore)(nil)
var _ FinalHTLCStore = (*KVStore)(nil)
var _ OpenChannelFwdPkgStore = (*KVStore)(nil)
var _ OpenChannelShutdownStore = (*KVStore)(nil)
var _ OpenChannelCloseTxStore = (*KVStore)(nil)
var _ OpenChannelStatusStore = (*KVStore)(nil)
