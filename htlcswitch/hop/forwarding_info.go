package hop

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// Network is the target blockchain network that the HTLC will travel
	// over next.
	Network Network

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
}
