package lnwire

import (
	"bytes"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// ChannelAnnouncement message is used to announce the existence of a channel
// between two peers in the overlay, which is propagated by the discovery
// service over broadcast handler.
type ChannelAnnouncement struct {
	// This signatures are used by nodes in order to create cross
	// references between node's channel and node. Requiring both nodes
	// to sign indicates they are both willing to route other payments via
	// this node.
	FirstNodeSig  *btcec.Signature
	SecondNodeSig *btcec.Signature

	// ChannelID is the unique description of the funding transaction.
	ChannelID ChannelID

	// This signatures are used by nodes in order to create cross
	// references between node's channel and node. Requiring the bitcoin
	// signatures proves they control the channel.
	FirstBitcoinSig  *btcec.Signature
	SecondBitcoinSig *btcec.Signature

	// The public keys of the two nodes who are operating the channel, such
	// that is FirstNodeID the numerically-lesser of the two DER encoded
	// keys (ascending numerical order).
	FirstNodeID  *btcec.PublicKey
	SecondNodeID *btcec.PublicKey

	// Public keys which corresponds to the keys which was declared in
	// multisig funding transaction output.
	FirstBitcoinKey  *btcec.PublicKey
	SecondBitcoinKey *btcec.PublicKey
}

// A compile time check to ensure ChannelAnnouncement implements the
// lnwire.Message interface.
var _ Message = (*ChannelAnnouncement)(nil)

// Validate performs any necessary sanity checks to ensure all fields present
// on the ChannelAnnouncement are valid.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Validate() error {
	return nil
}

// Decode deserializes a serialized ChannelAnnouncement stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.FirstNodeSig,
		&a.SecondNodeSig,
		&a.ChannelID,
		&a.FirstBitcoinSig,
		&a.SecondBitcoinSig,
		&a.FirstNodeID,
		&a.SecondNodeID,
		&a.FirstBitcoinKey,
		&a.SecondBitcoinKey,
	)
}

// Encode serializes the target ChannelAnnouncement into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.FirstNodeSig,
		a.SecondNodeSig,
		a.ChannelID,
		a.FirstBitcoinSig,
		a.SecondBitcoinSig,
		a.FirstNodeID,
		a.SecondNodeID,
		a.FirstBitcoinKey,
		a.SecondBitcoinKey,
	)
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Command() uint32 {
	return CmdChannelAnnoucmentMessage
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// FirstNodeSig - 64 bytes
	length += 64

	// SecondNodeSig - 64 bytes
	length += 64

	// ChannelID - 8 bytes
	length += 8

	// FirstBitcoinSig - 64 bytes
	length += 64

	// SecondBitcoinSig - 64 bytes
	length += 64

	// FirstNodeID - 33 bytes
	length += 33

	// SecondNodeID - 33 bytes
	length += 33

	// FirstBitcoinKey - 33 bytes
	length += 33

	// SecondBitcoinKey - 33 bytes
	length += 33

	return length
}

// DataToSign is used to retrieve part of the announcement message which
// should be signed.
func (a *ChannelAnnouncement) DataToSign() ([]byte, error) {
	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		a.ChannelID,
		a.FirstBitcoinSig,
		a.SecondBitcoinSig,
		a.FirstNodeID,
		a.SecondNodeID,
		a.FirstBitcoinKey,
		a.SecondBitcoinKey,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
