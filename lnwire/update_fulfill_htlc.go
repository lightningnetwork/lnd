package lnwire

import (
	"bytes"
	"io"
)

// UpdateFulfillHTLC is sent by Alice to Bob when she wishes to settle a
// particular HTLC referenced by its HTLCKey within a specific active channel
// referenced by ChannelPoint.  A subsequent CommitSig message will be sent by
// Alice to "lock-in" the removal of the specified HTLC, possible containing a
// batch signature covering several settled HTLC's.
type UpdateFulfillHTLC struct {
	// ChanID references an active channel which holds the HTLC to be
	// settled.
	ChanID ChannelID

	// ID denotes the exact HTLC stage within the receiving node's
	// commitment transaction to be removed.
	ID uint64

	// PaymentPreimage is the R-value preimage required to fully settle an
	// HTLC.
	PaymentPreimage [32]byte

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewUpdateFulfillHTLC returns a new empty UpdateFulfillHTLC.
func NewUpdateFulfillHTLC(chanID ChannelID, id uint64,
	preimage [32]byte) *UpdateFulfillHTLC {

	return &UpdateFulfillHTLC{
		ChanID:          chanID,
		ID:              id,
		PaymentPreimage: preimage,
	}
}

// A compile time check to ensure UpdateFulfillHTLC implements the
// lnwire.Message interface.
var _ Message = (*UpdateFulfillHTLC)(nil)

// A compile time check to ensure UpdateFulfillHTLC implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*UpdateFulfillHTLC)(nil)

// Decode deserializes a serialized UpdateFulfillHTLC message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) Decode(r io.Reader, pver uint32) error {
	// msgExtraData is a temporary variable used to read the message extra
	// data field from the reader.
	var msgExtraData ExtraOpaqueData

	if err := ReadElements(r,
		&c.ChanID,
		&c.ID,
		c.PaymentPreimage[:],
		&msgExtraData,
	); err != nil {
		return err
	}

	// Extract custom records from the extra data field.
	customRecords, _, extraData, err := ParseAndExtractCustomRecords(
		msgExtraData,
	)
	if err != nil {
		return err
	}

	c.CustomRecords = customRecords
	c.ExtraData = extraData

	return nil
}

// Encode serializes the target UpdateFulfillHTLC into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteUint64(w, c.ID); err != nil {
		return err
	}

	if err := WriteBytes(w, c.PaymentPreimage[:]); err != nil {
		return err
	}

	// Combine the custom records and the extra data, then encode the
	// result as a byte slice.
	extraData, err := MergeAndEncode(nil, c.ExtraData, c.CustomRecords)
	if err != nil {
		return err
	}

	return WriteBytes(w, extraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateFulfillHTLC) MsgType() MessageType {
	return MsgUpdateFulfillHTLC
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *UpdateFulfillHTLC) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *UpdateFulfillHTLC) TargetChanID() ChannelID {
	return c.ChanID
}
