package lnwire

import "io"

// OnionPacketSize is the size of the serialized Sphinx onion packet included
// in each UpdateAddHTLC message. The breakdown of the onion packet is as
// follows: 1-byte version, 33-byte ephemeral public key (for ECDH), 1300-bytes
// of per-hop data, and a 32-byte HMAC over the entire packet.
const OnionPacketSize = 1366

// UpdateAddHTLC is the message sent by Alice to Bob when she wishes to add an
// HTLC to his remote commitment transaction. In addition to information
// detailing the value, the ID, expiry, and the onion blob is also included
// which allows Bob to derive the next hop in the route. The HTLC added by this
// message is to be added to the remote node's "pending" HTLC's.  A subsequent
// CommitSig message will move the pending HTLC to the newly created commitment
// transaction, marking them as "staged".
type UpdateAddHTLC struct {
	// ChanID is the particular active channel that this UpdateAddHTLC is
	// bound to.
	ChanID ChannelID

	// ID is the identification server for this HTLC. This value is
	// explicitly included as it allows nodes to survive single-sided
	// restarts. The ID value for this sides starts at zero, and increases
	// with each offered HTLC.
	ID uint64

	// Amount is the amount of millisatoshis this HTLC is worth.
	Amount MilliSatoshi

	// PaymentHash is the payment hash to be included in the HTLC this
	// request creates. The pre-image to this HTLC must be revelaed by the
	// upstream peer in order to fully settle the HTLC.
	PaymentHash [32]byte

	// Expiry is the number of blocks after which this HTLC should expire.
	// It is the receiver's duty to ensure that the outgoing HTLC has a
	// sufficient expiry value to allow her to redeem the incoming HTLC.
	Expiry uint32

	// OnionBlob is the raw serialized mix header used to route an HTLC in
	// a privacy-preserving manner. The mix header is defined currently to
	// be parsed as a 4-tuple: (groupElement, routingInfo, headerMAC,
	// body).  First the receiving node should use the groupElement, and
	// its current onion key to derive a shared secret with the source.
	// Once the shared secret has been derived, the headerMAC should be
	// checked FIRST. Note that the MAC only covers the routingInfo field.
	// If the MAC matches, and the shared secret is fresh, then the node
	// should strip off a layer of encryption, exposing the next hop to be
	// used in the subsequent UpdateAddHTLC message.
	OnionBlob [OnionPacketSize]byte
}

// NewUpdateAddHTLC returns a new empty UpdateAddHTLC message.
func NewUpdateAddHTLC() *UpdateAddHTLC {
	return &UpdateAddHTLC{}
}

// A compile time check to ensure UpdateAddHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateAddHTLC)(nil)

// Decode deserializes a serialized UpdateAddHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChanID,
		&c.ID,
		&c.Amount,
		c.PaymentHash[:],
		&c.Expiry,
		c.OnionBlob[:],
	)
}

// Encode serializes the target UpdateAddHTLC into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.ID,
		c.Amount,
		c.PaymentHash[:],
		c.Expiry,
		c.OnionBlob[:],
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) MsgType() MessageType {
	return MsgUpdateAddHTLC
}

// MaxPayloadLength returns the maximum allowed payload size for a UpdateAddHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) MaxPayloadLength(uint32) uint32 {
	// 1450
	return 32 + 8 + 4 + 8 + 32 + 1366
}
