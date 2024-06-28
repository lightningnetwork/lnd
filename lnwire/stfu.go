package lnwire

import (
	"bytes"
	"io"
)

// Stfu is a message that is sent to lock the channel state prior to some other
// interactive protocol where channel updates need to be paused.
type Stfu struct {
	// ChanID identifies which channel needs to be frozen.
	ChanID ChannelID

	// Initiator is a byte that identifies whether we are the initiator of
	// this process.
	Initiator bool

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure Stfu implements the lnwire.Message interface.
var _ Message = (*Stfu)(nil)

// Encode serializes the target Stfu into the passed io.Writer.
// Serialization will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (shh *Stfu) Encode(w *bytes.Buffer, _ uint32) error {
	var extraBytesWriter bytes.Buffer
	shh.ExtraData = ExtraOpaqueData(extraBytesWriter.Bytes())

	if err := WriteChannelID(w, shh.ChanID); err != nil {
		return err
	}

	if err := WriteBool(w, shh.Initiator); err != nil {
		return err
	}

	return WriteBytes(w, shh.ExtraData)
}

// Decode deserializes the serialized Stfu stored in the passed io.Reader
// into the target Stfu using the deserialization rules defined by the
// passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (shh *Stfu) Decode(r io.Reader, _ uint32) error {
	var extra ExtraOpaqueData
	if err := ReadElements(
		r, &shh.ChanID, &shh.Initiator, &extra,
	); err != nil {
		return err
	}

	if len(extra) != 0 {
		shh.ExtraData = extra
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a Stfu on the wire.
//
// This is part of the lnwire.Message interface.
func (shh *Stfu) MsgType() MessageType {
	return MsgStfu
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (shh *Stfu) TargetChanID() ChannelID {
	return shh.ChanID
}
