package chanstate

import (
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// KVStore is the KV-backed implementation of the channel-state store facets.
// Store facets are moved onto this type incrementally while channeldb keeps
// compatibility wrappers for callers that still depend on the old package.
type KVStore struct {
	backend                   kvdb.Backend
	noRevLogAmtData           bool
	storeFinalHtlcResolutions bool
	tombstoneClosedChannels   bool
	extractFwdResponses       func(kvdb.RwTx, lnwire.ShortChannelID) error
}

// NewKVStore creates a KV-backed channel-state store.
func NewKVStore(
	backend kvdb.Backend,
	storeFinalHtlcResolutions, noRevLogAmtData,
	tombstoneClosedChannels bool,
	extractFwdResponses func(kvdb.RwTx, lnwire.ShortChannelID) error,
) *KVStore {

	return &KVStore{
		backend:                   backend,
		noRevLogAmtData:           noRevLogAmtData,
		storeFinalHtlcResolutions: storeFinalHtlcResolutions,
		tombstoneClosedChannels:   tombstoneClosedChannels,
		extractFwdResponses:       extractFwdResponses,
	}
}

var _ ChannelSetupStore = (*KVStore)(nil)
var _ FinalHTLCStore = (*KVStore)(nil)
var _ OpenChannelFwdPkgStore = (*KVStore)(nil)
var _ OpenChannelShutdownStore = (*KVStore)(nil)
var _ OpenChannelCloseTxStore = (*KVStore)(nil)
var _ OpenChannelStatusStore = (*KVStore)(nil)
var _ OpenChannelCommitmentStore = (*KVStore)(nil)
var _ HistoricalChannelStore = (*KVStore)(nil)
