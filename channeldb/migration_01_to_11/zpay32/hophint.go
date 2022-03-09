package zpay32

import "github.com/btcsuite/btcd/btcec/v2"

const (
	// DefaultFinalCLTVDelta is the default value to be used as the final
	// CLTV delta for a route if one is unspecified.
	DefaultFinalCLTVDelta = 9
)

// HopHint is a routing hint that contains the minimum information of a channel
// required for an intermediate hop in a route to forward the payment to the
// next. This should be ideally used for private channels, since they are not
// publicly advertised to the network for routing.
type HopHint struct {
	// NodeID is the public key of the node at the start of the channel.
	NodeID *btcec.PublicKey

	// ChannelID is the unique identifier of the channel.
	ChannelID uint64

	// FeeBaseMSat is the base fee of the channel in millisatoshis.
	FeeBaseMSat uint32

	// FeeProportionalMillionths is the fee rate, in millionths of a
	// satoshi, for every satoshi sent through the channel.
	FeeProportionalMillionths uint32

	// CLTVExpiryDelta is the time-lock delta of the channel.
	CLTVExpiryDelta uint16
}

// Copy returns a deep copy of the hop hint.
func (h HopHint) Copy() HopHint {
	nodeID := *h.NodeID
	return HopHint{
		NodeID:                    &nodeID,
		ChannelID:                 h.ChannelID,
		FeeBaseMSat:               h.FeeBaseMSat,
		FeeProportionalMillionths: h.FeeProportionalMillionths,
		CLTVExpiryDelta:           h.CLTVExpiryDelta,
	}
}
