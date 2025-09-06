package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

// OnionMessage is a message that carries an onion-encrypted payload.
// This is used for BOLT12 messages.
type OnionMessage struct {
	// BlindingPoint is the route blinding ephemeral pubkey to be used for
	// the onion message.
	BlindingPoint *btcec.PublicKey

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte
}

// NewOnionMessage creates a new OnionMessage.
func NewOnionMessage(blindingPoint *btcec.PublicKey,
	onion []byte) *OnionMessage {

	return &OnionMessage{
		BlindingPoint: blindingPoint,
		OnionBlob:     onion,
	}
}

// A compile-time check to ensure OnionMessage implements the Message interface.
var _ Message = (*OnionMessage)(nil)

// Decode reads the bytes stream and converts it to the object.
func (o *OnionMessage) Decode(r io.Reader, _ uint32) error {
	if err := ReadElement(r, &o.BlindingPoint); err != nil {
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
	if err := WritePublicKey(w, o.BlindingPoint); err != nil {
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
