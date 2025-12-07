package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// OnionMessage is a message that carries an onion-encrypted payload.
// This is used for BOLT12 messages.
type OnionMessage struct {
	// PathKey is the route blinding ephemeral pubkey to be used for
	// the onion message.
	PathKey *btcec.PublicKey

	// OnionBlob contains the onion_message_packet, the raw serialized
	// Sphinx onion packet (BOLT 4) containing the layered, per-hop
	// encrypted payloads and routing instructions used to forward this
	// message along its designated path. This blob should be handled in the
	// same manner as onion_routing_packet used to route HTLCs, with the
	// exception that it uses blinded routes by default.
	OnionBlob []byte
}

// NewOnionMessage creates a new OnionMessage.
func NewOnionMessage(pathKey *btcec.PublicKey,
	onion []byte) *OnionMessage {

	return &OnionMessage{
		PathKey:   pathKey,
		OnionBlob: onion,
	}
}

// A compile-time check to ensure OnionMessage implements the Message interface.
var _ Message = (*OnionMessage)(nil)

var _ SizeableMessage = (*OnionMessage)(nil)

// Decode reads the bytes stream and converts it to the object.
func (o *OnionMessage) Decode(r io.Reader, _ uint32) error {
	if err := ReadElement(r, &o.PathKey); err != nil {
		return err
	}

	var onionLen uint16
	if err := ReadElement(r, &onionLen); err != nil {
		return err
	}

	o.OnionBlob = make([]byte, onionLen)
	if err := ReadElement(r, o.OnionBlob); err != nil {
		return err
	}

	return nil
}

// Encode converts object to the bytes stream and write it into the
// write buffer.
func (o *OnionMessage) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WritePublicKey(w, o.PathKey); err != nil {
		return err
	}

	onionLen := len(o.OnionBlob)
	if err := WriteUint16(w, uint16(onionLen)); err != nil {
		return err
	}

	if err := WriteBytes(w, o.OnionBlob); err != nil {
		return err
	}

	return nil
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
func (o *OnionMessage) MsgType() MessageType {
	return MsgOnionMessage
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (o *OnionMessage) SerializedSize() (uint32, error) {
	return MessageSerializedSize(o)
}
