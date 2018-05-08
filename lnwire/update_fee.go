package lnwire

import (
	"io"
)

// UpdateFee is the message the channel initiator sends to the other peer if
// the channel commitment fee needs to be updated.
type UpdateFee struct {
	// ChanID is the channel that this UpdateFee is meant for.
	ChanID ChannelID

	// FeePerKw is the fee-per-kw on commit transactions that the sender of
	// this message wants to use for this channel.
	//
	// TODO(halseth): make SatPerKWeight when fee estimation is moved to
	// own package. Currently this will cause an import cycle.
	FeePerKw uint32
}

// NewUpdateFee creates a new UpdateFee message.
func NewUpdateFee(chanID ChannelID, feePerKw uint32) *UpdateFee {
	return &UpdateFee{
		ChanID:   chanID,
		FeePerKw: feePerKw,
	}
}

// A compile time check to ensure UpdateFee implements the lnwire.Message
// interface.
var _ Message = (*UpdateFee)(nil)

// Decode deserializes a serialized UpdateFee message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFee) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChanID,
		&c.FeePerKw,
	)
}

// Encode serializes the target UpdateFee into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFee) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.FeePerKw,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFee) MsgType() MessageType {
	return MsgUpdateFee
}

// MaxPayloadLength returns the maximum allowed payload size for an UpdateFee
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFee) MaxPayloadLength(uint32) uint32 {
	// 32 + 4
	return 36
}
