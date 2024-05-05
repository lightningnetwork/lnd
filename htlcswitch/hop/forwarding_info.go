package hop

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// NextHop is the channel ID of the next hop. The received HTLC should
	// be forwarded to this particular channel in order to continue the
	// end-to-end route.
	NextHop lnwire.ShortChannelID

	// AmountToForward is the amount of milli-satoshis that the receiving
	// node should forward to the next hop.
	AmountToForward lnwire.MilliSatoshi

	// OutgoingCTLV is the specified value of the CTLV timelock to be used
	// in the outgoing HTLC.
	OutgoingCTLV uint32

	// NextBlinding is an optional blinding point to be passed to the next
	// node in UpdateAddHtlc. This field is set if the htlc is part of a
	// blinded route.
	NextBlinding lnwire.BlindingPointRecord

	// PathID is a secret identifier that the creator of a blinded path
	// sets for itself to ensure that the blinded path has been used in the
	// correct context.
	PathID *chainhash.Hash
}
