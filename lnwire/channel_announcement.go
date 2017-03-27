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
	NodeSig1 *btcec.Signature
	NodeSig2 *btcec.Signature

	// This signatures are used by nodes in order to create cross
	// references between node's channel and node. Requiring the bitcoin
	// signatures proves they control the channel.
	BitcoinSig1 *btcec.Signature
	BitcoinSig2 *btcec.Signature

	// ShortChannelID is the unique description of the funding transaction.
	ShortChannelID ShortChannelID

	// The public keys of the two nodes who are operating the channel, such
	// that is NodeID1 the numerically-lesser than NodeID2 (ascending
	// numerical order).
	NodeID1 *btcec.PublicKey
	NodeID2 *btcec.PublicKey

	// Public keys which corresponds to the keys which was declared in
	// multisig funding transaction output.
	BitcoinKey1 *btcec.PublicKey
	BitcoinKey2 *btcec.PublicKey
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
		&a.NodeSig1,
		&a.NodeSig2,
		&a.ShortChannelID,
		&a.BitcoinSig1,
		&a.BitcoinSig2,
		&a.NodeID1,
		&a.NodeID2,
		&a.BitcoinKey1,
		&a.BitcoinKey2,
	)
}

// Encode serializes the target ChannelAnnouncement into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.NodeSig1,
		a.NodeSig2,
		a.ShortChannelID,
		a.BitcoinSig1,
		a.BitcoinSig2,
		a.NodeID1,
		a.NodeID2,
		a.BitcoinKey1,
		a.BitcoinKey2,
	)
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) Command() uint32 {
	return CmdChannelAnnouncement
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelAnnouncement) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// NodeSig1 - 64 bytes
	length += 64

	// NodeSig2 - 64 bytes
	length += 64

	// ShortChannelID - 8 bytes
	length += 8

	// BitcoinSig1 - 64 bytes
	length += 64

	// BitcoinSig2 - 64 bytes
	length += 64

	// NodeID1 - 33 bytes
	length += 33

	// NodeID2 - 33 bytes
	length += 33

	// BitcoinKey1 - 33 bytes
	length += 33

	// BitcoinKey2 - 33 bytes
	length += 33

	return length
}

// DataToSign is used to retrieve part of the announcement message which
// should be signed.
func (a *ChannelAnnouncement) DataToSign() ([]byte, error) {
	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		a.ShortChannelID,
		a.NodeID1,
		a.NodeID2,
		a.BitcoinKey1,
		a.BitcoinKey2,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
