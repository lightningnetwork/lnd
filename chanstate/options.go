package chanstate

import (
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// StoreOptions holds parameters for the KVStore.
type StoreOptions struct {
	// StoreFinalHtlcResolutions determines whether to persistently store
	// the final resolution of incoming htlcs.
	StoreFinalHtlcResolutions bool

	// NoRevLogAmtData when set to true, indicates that amount data should
	// not be stored in the revocation log.
	NoRevLogAmtData bool

	// TombstoneClosedChannels, when true, instructs CloseChannel to skip
	// the cascading deletion of nested per-channel state and rely on the
	// outpoint-index flip to mark the channel as closed.
	TombstoneClosedChannels bool

	// ExtractFwdResponses persists forwarding responses before their
	// forwarding packages are removed during channel cleanup.
	ExtractFwdResponses func(kvdb.RwTx, lnwire.ShortChannelID) error
}

// DefaultOptions returns a StoreOptions populated with default values.
func DefaultOptions() *StoreOptions {
	return &StoreOptions{
		ExtractFwdResponses: func(kvdb.RwTx,
			lnwire.ShortChannelID) error {

			return nil
		},
	}
}

// OptionModifier is a function signature for modifying the default
// StoreOptions.
type OptionModifier func(*StoreOptions)

// WithStoreFinalHtlcResolutions controls whether to persistently store the
// final resolution of incoming htlcs.
func WithStoreFinalHtlcResolutions(enabled bool) OptionModifier {
	return func(o *StoreOptions) {
		o.StoreFinalHtlcResolutions = enabled
	}
}

// WithNoRevLogAmtData controls whether amount data should be omitted from the
// revocation log.
func WithNoRevLogAmtData(enabled bool) OptionModifier {
	return func(o *StoreOptions) {
		o.NoRevLogAmtData = enabled
	}
}

// WithTombstoneClosedChannels controls whether CloseChannel skips the
// cascading deletion of nested per-channel state and relies on the
// outpoint-index flip to mark the channel as closed.
func WithTombstoneClosedChannels(enabled bool) OptionModifier {
	return func(o *StoreOptions) {
		o.TombstoneClosedChannels = enabled
	}
}

// WithExtractFwdResponses configures the forwarding-response persistence step
// performed during channel cleanup.
func WithExtractFwdResponses(
	extract func(kvdb.RwTx, lnwire.ShortChannelID) error) OptionModifier {

	return func(o *StoreOptions) {
		o.ExtractFwdResponses = extract
	}
}
