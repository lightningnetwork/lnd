package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/wire"
)

// CancelReason specifies the precise reason that an upstream HTLC was
// cancelled. Each CancelHTLC message carries a CancelReason which is to be
// passed back unaltered to the source of the HTLC within the route.
//
// TODO(roasbeef): implement proper encrypted error messages as defined in spec
//  * these errors as it stands reveal the error cause to all links in the
//    route and are horrible for privacy
type CancelReason uint16

const (
	// InsufficientCapacity indicates that a payment failed due to a link
	// in the ultimate route not having enough satoshi flow to successfully
	// carry the payment.
	InsufficientCapacity = 0

	// UpstreamTimeout indicates that an upstream link had to enforce the
	// absolute HTLC timeout, removing the HTLC.
	UpstreamTimeout = 1

	// UnknownPaymentHash indicates that the destination did not recognize
	// the payment hash.
	UnknownPaymentHash = 2

	// UnknownDestination indicates that the specified next hop within the
	// Sphinx packet at a point in the route contained an unknown or
	// invalid "next hop".
	UnknownDestination = 3

	// SphinxParseError indicates that an intermediate node was unable
	// properly parse the HTLC.
	SphinxParseError = 4

	// IncorrectValue indicates that the HTLC ultimately extended to the
	// destination did not match the value that was expected.
	IncorrectValue = 5
)

// String returns a human-readable version of the CancelReason type.
func (c CancelReason) String() string {
	switch c {
	case InsufficientCapacity:
		return "InsufficientCapacity: next hop had insufficient " +
			"capacity for payment"

	case UpstreamTimeout:
		return "UpstreamTimeout: HTLC has timed out upstream"

	case UnknownPaymentHash:
		return "UnknownPaymentHash: the destination did not know the " +
			"preimage"

	case UnknownDestination:
		return "UnknownDestination: next hop unknown"

	case SphinxParseError:
		return "SphinxParseError: unable to parse sphinx packet"

	case IncorrectValue:
		return "IncorrectValue: htlc value was wrong"

	default:
		return "unknown reason"
	}
}

// CancelHTLC is sent by Alice to Bob in order to remove a previously added
// HTLC. Upon receipt of an CancelHTLC the HTLC should be removed from the next
// commitment transaction, with the CancelHTLC propagated backwards in the
// route to fully un-clear the HTLC.
type CancelHTLC struct {
	// ChannelPoint is the particular active channel that this CancelHTLC
	// is binded to.
	ChannelPoint *wire.OutPoint

	// HTLCKey references which HTLC on the remote node's commitment
	// transaction has timed out.
	HTLCKey HTLCKey

	// Reason described the event that caused the HTLC to be cancelled
	// within the route.
	Reason CancelReason
}

// Decode deserializes a serialized CancelHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// HTLCKey(8)
	err := readElements(r,
		&c.ChannelPoint,
		&c.HTLCKey,
		&c.Reason,
	)
	if err != nil {
		return err
	}

	return nil
}

// CancelHTLC creates a new CancelHTLC message.
func NewHTLCTimeoutRequest() *CancelHTLC {
	return &CancelHTLC{}
}

// A compile time check to ensure CancelHTLC implements the lnwire.Message
// interface.
var _ Message = (*CancelHTLC)(nil)

// Encode serializes the target CancelHTLC into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
		c.HTLCKey,
		c.Reason,
	)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Command() uint32 {
	return CmdCancelHTLC
}

// MaxPayloadLength returns the maximum allowed payload size for a CancelHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) MaxPayloadLength(uint32) uint32 {
	// 36 + 8 + 2
	return 46
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the CancelHTLC are valid.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target CancelHTLC.  This is
// part of the lnwire.Message interface.
func (c *CancelHTLC) String() string {
	return fmt.Sprintf("\n--- Begin CancelHTLC ---\n") +
		fmt.Sprintf("ChannelPoint:\t%d\n", c.ChannelPoint) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("Reason:\t%v\n", c.Reason) +
		fmt.Sprintf("--- End CancelHTLC ---\n")
}
