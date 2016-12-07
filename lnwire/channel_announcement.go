package lnwire

import (
	"bytes"
	"fmt"
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"io"
)

// ChannelID represent the set of data which is needed to retrieve all
// necessary data to validate the channel existance.
type ChannelID struct {
	// BlockHeight is the height of the block where funding
	// transaction located.
	// NOTE: This field is limited to 3 bytes.
	BlockHeight uint32

	// TxIndex is a position of funding transaction within a block.
	// NOTE: This field is limited to 3 bytes.
	TxIndex uint32

	// TxPosition indicating transaction output which pays to the
	// channel.
	TxPosition uint16
}

func (c *ChannelID) String() string {
	return fmt.Sprintf("BlockHeight:\t\t\t%v\n", c.BlockHeight) +
		fmt.Sprintf("TxIndex:\t\t\t%v\n", c.TxIndex) +
		fmt.Sprintf("TxPosition:\t\t\t%v\n", c.TxPosition)
}

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
	ChannelID *ChannelID

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
	var sigHash []byte

	sigHash = wire.DoubleSha256(a.FirstNodeID.SerializeCompressed())
	if !a.FirstBitcoinSig.Verify(sigHash, a.FirstBitcoinKey) {
		return errors.New("can't verify first bitcoin signature")
	}

	sigHash = wire.DoubleSha256(a.SecondNodeID.SerializeCompressed())
	if !a.SecondBitcoinSig.Verify(sigHash, a.SecondBitcoinKey) {
		return errors.New("can't verify second bitcoin signature")
	}

	data, err := a.DataToSign()
	if err != nil {
		return err
	}
	dataHash := wire.DoubleSha256(data)

	if !a.FirstNodeSig.Verify(dataHash, a.FirstNodeID) {
		return errors.New("can't verify data in first node signature")
	}

	if !a.SecondNodeSig.Verify(dataHash, a.SecondNodeID) {
		return errors.New("can't verify data in second node signature")
	}

	return nil
}

// Decode deserializes a serialized ChannelAnnouncement stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement) Decode(r io.Reader, pver uint32) error {
	err := readElements(r,
		&c.FirstNodeSig,
		&c.SecondNodeSig,
		&c.ChannelID,
		&c.FirstBitcoinSig,
		&c.SecondBitcoinSig,
		&c.FirstNodeID,
		&c.SecondNodeID,
		&c.FirstBitcoinKey,
		&c.SecondBitcoinKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target ChannelAnnouncement into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.FirstNodeSig,
		c.SecondNodeSig,
		c.ChannelID,
		c.FirstBitcoinSig,
		c.SecondBitcoinSig,
		c.FirstNodeID,
		c.SecondNodeID,
		c.FirstBitcoinKey,
		c.SecondBitcoinKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement) Command() uint32 {
	return CmdChannelAnnoucmentMessage
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement) MaxPayloadLength(pver uint32) uint32 {
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

// String returns the string representation of the target ChannelAnnouncement.
//
// This is part of the lnwire.Message interface.
func (c *ChannelAnnouncement) String() string {
	return fmt.Sprintf("\n--- Begin ChannelAnnouncement ---\n") +
		fmt.Sprintf("FirstNodeSig:\t\t%v\n", c.FirstNodeSig) +
		fmt.Sprintf("SecondNodeSig:\t\t%v\n", c.SecondNodeSig) +
		fmt.Sprintf("ChannelID:\t\t%v\n", c.ChannelID.String()) +
		fmt.Sprintf("FirstBitcoinSig:\t\t%v\n", c.FirstBitcoinSig) +
		fmt.Sprintf("SecondBitcoinSig:\t\t%v\n", c.SecondBitcoinSig) +
		fmt.Sprintf("FirstNodeSig:\t\t%v\n", c.FirstNodeSig) +
		fmt.Sprintf("SecondNodeID:\t\t%v\n", c.SecondNodeID) +
		fmt.Sprintf("FirstBitcoinKey:\t\t%v\n", c.FirstBitcoinKey) +
		fmt.Sprintf("SecondBitcoinKey:\t\t%v\n", c.SecondBitcoinKey) +
		fmt.Sprintf("--- End ChannelAnnouncement ---\n")
}

// DataToSign is used to retrieve part of the announcement message which
// should be signed.
func (c *ChannelAnnouncement) DataToSign() ([]byte, error) {
	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		c.ChannelID,
		c.FirstBitcoinSig,
		c.SecondBitcoinSig,
		c.FirstNodeID,
		c.SecondNodeID,
		c.FirstBitcoinKey,
		c.SecondBitcoinKey,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
