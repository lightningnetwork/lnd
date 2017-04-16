package lnwire

import "io"

// FailCode specifies the precise reason that an upstream HTLC was cancelled.
// Each UpdateFailHTLC message carries a FailCode which is to be passed back
// unaltered to the source of the HTLC within the route.
//
// TODO(roasbeef): implement proper encrypted error messages as defined in spec
//  * these errors as it stands reveal the error cause to all links in the
//    route and are horrible for privacy
type FailCode uint16

const (
	// InsufficientCapacity indicates that a payment failed due to a link
	// in the ultimate route not having enough satoshi flow to successfully
	// carry the payment.
	InsufficientCapacity FailCode = 0

	// UpstreamTimeout indicates that an upstream link had to enforce the
	// absolute HTLC timeout, removing the HTLC.
	UpstreamTimeout FailCode = 1

	// UnknownPaymentHash indicates that the destination did not recognize
	// the payment hash.
	UnknownPaymentHash FailCode = 2

	// UnknownDestination indicates that the specified next hop within the
	// Sphinx packet at a point in the route contained an unknown or
	// invalid "next hop".
	UnknownDestination FailCode = 3

	// SphinxParseError indicates that an intermediate node was unable
	// properly parse the HTLC.
	SphinxParseError FailCode = 4

	// IncorrectValue indicates that the HTLC ultimately extended to the
	// destination did not match the value that was expected.
	IncorrectValue FailCode = 5
)

// String returns a human-readable version of the FailCode type.
func (c FailCode) String() string {
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

// OpaqueReason is an opaque encrypted byte slice that encodes the exact
// failure reason and additional some supplemental data. The contents of this
// slice can only be decrypted by the sender of the original HTLC.
type OpaqueReason []byte

// UpdateFailHTLC is sent by Alice to Bob in order to remove a previously added
// HTLC. Upon receipt of an UpdateFailHTLC the HTLC should be removed from the
// next commitment transaction, with the UpdateFailHTLC propagated backwards in
// the route to fully undo the HTLC.
type UpdateFailHTLC struct {
	// ChannelPoint is the particular active channel that this
	// UpdateFailHTLC is bound to.
	ChanID ChannelID

	// ID references which HTLC on the remote node's commitment transaction
	// has timed out.
	ID uint64

	// Reason is an onion-encrypted blob that details why the HTLC was
	// failed. This blob is only fully decryptable by the initiator of the
	// HTLC message.
	// TODO(roasbeef): properly format the encrypted failure reason
	Reason OpaqueReason
}

// A compile time check to ensure UpdateFailHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateFailHTLC)(nil)

// Decode deserializes a serialized UpdateFailHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChanID,
		&c.ID,
		&c.Reason,
	)
}

// Encode serializes the target UpdateFailHTLC into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.ID,
		c.Reason,
	)
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Command() uint32 {
	return CmdUpdateFailHTLC
}

// MaxPayloadLength returns the maximum allowed payload size for a UpdateFailHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) MaxPayloadLength(uint32) uint32 {
	// 32 +  8  + 154
	return 194
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the UpdateFailHTLC are valid.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFailHTLC) Validate() error {
	// We're good!
	return nil
}
