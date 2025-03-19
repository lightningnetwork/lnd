package lnwire

import (
	"bytes"
	"io"
)

// KickoffSig is the message used to transmit the signature for a kickoff
// transaction during the execution phase of a dynamic commitment negotiation
// that requires a reanchoring step.
type KickoffSig struct {
	// ChanID identifies the channel id for which this signature is
	// intended.
	ChanID ChannelID

	// Signature contains the ECDSA signature that signs the kickoff
	// transaction.
	Signature Sig

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure that KickoffSig implements the lnwire.Message
// interface.
var _ Message = (*KickoffSig)(nil)

// A compile time check to ensure KickoffSig implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*KickoffSig)(nil)

// Encode serializes the target KickoffSig into the passed bytes.Buffer
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (ks *KickoffSig) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, ks.ChanID); err != nil {
		return err
	}
	if err := WriteSig(w, ks.Signature); err != nil {
		return err
	}

	return WriteBytes(w, ks.ExtraData)
}

// Decode deserializes a serialized KickoffSig message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (ks *KickoffSig) Decode(r io.Reader, _ uint32) error {
	return ReadElements(r, &ks.ChanID, &ks.Signature, &ks.ExtraData)
}

// MsgType returns the integer uniquely identifying KickoffSig on the wire.
//
// This is part of the lnwire.Message interface.
func (ks *KickoffSig) MsgType() MessageType { return MsgKickoffSig }

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (ks *KickoffSig) SerializedSize() (uint32, error) {
	return MessageSerializedSize(ks)
}
