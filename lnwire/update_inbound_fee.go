package lnwire

import (
	"bytes"
	"io"
)

type UpdateInboundFee struct {
	// ChanID is the particular active channel that this
	// UpdateInboundDiscount is bound to.
	ChanID ChannelID

	BaseFee int32

	FeeRate int32

	ExtraData ExtraOpaqueData
}

// NewUpdateAddHTLC returns a new empty UpdateAddHTLC message.
func NewUpdateInboundDiscount() *UpdateInboundFee {
	return &UpdateInboundFee{}
}

// A compile time check to ensure UpdateAddHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateInboundFee)(nil)

// Decode deserializes a serialized UpdateAddHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateInboundFee) Decode(r io.Reader, pver uint32) error {
	var baseFee, feeRate uint32

	err := ReadElements(r,
		&c.ChanID,
		&baseFee,
		&feeRate,
	)
	if err != nil {
		return err
	}

	c.BaseFee = int32(baseFee)
	c.FeeRate = int32(feeRate)

	return c.ExtraData.Decode(r)
}

// Encode serializes the target UpdateAddHTLC into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateInboundFee) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteUint32(w, uint32(c.BaseFee)); err != nil {
		return err
	}

	if err := WriteUint32(w, uint32(c.FeeRate)); err != nil {
		return err
	}

	// Finally, append any extra opaque data.
	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateInboundFee) MsgType() MessageType {
	return MsgUpdateInboundFee
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *UpdateInboundFee) TargetChanID() ChannelID {
	return c.ChanID
}
