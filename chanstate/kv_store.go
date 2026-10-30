package chanstate

import (
	"github.com/lightningnetwork/lnd/kvdb"
)

// KVStore is the KV-backed implementation of the channel-state store facets.
type KVStore struct {
	backend                   kvdb.Backend
	noRevLogAmtData           bool
	storeFinalHtlcResolutions bool
	tombstoneClosedChannels   bool
	fwdResponseExtractor      KVFwdResponseExtractor
}

// NewKVStore creates a KV-backed channel-state store.
func NewKVStore(backend kvdb.Backend, options ...OptionModifier) *KVStore {
	opts := DefaultOptions()
	for _, applyOption := range options {
		applyOption(opts)
	}

	return &KVStore{
		backend:                   backend,
		noRevLogAmtData:           opts.NoRevLogAmtData,
		storeFinalHtlcResolutions: opts.StoreFinalHtlcResolutions,
		tombstoneClosedChannels:   opts.TombstoneClosedChannels,
		fwdResponseExtractor:      opts.FwdResponseExtractor,
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
var _ Store = (*KVStore)(nil)
