package migration29

import (
	"encoding/binary"
	"encoding/hex"
	"io"

	"github.com/btcsuite/btcd/wire"
)

var (
	byteOrder = binary.BigEndian
)

// ChannelID is a series of 32-bytes that uniquely identifies all channels
// within the network. The ChannelID is computed using the outpoint of the
// funding transaction (the txid, and output index). Given a funding output the
// ChannelID can be calculated by XOR'ing the big-endian serialization of the
// txid and the big-endian serialization of the output index, truncated to
// 2 bytes.
type ChannelID [32]byte

// String returns the string representation of the ChannelID. This is just the
// hex string encoding of the ChannelID itself.
func (c ChannelID) String() string {
	return hex.EncodeToString(c[:])
}

// NewChanIDFromOutPoint converts a target OutPoint into a ChannelID that is
// usable within the network. In order to convert the OutPoint into a ChannelID,
// we XOR the lower 2-bytes of the txid within the OutPoint with the big-endian
// serialization of the Index of the OutPoint, truncated to 2-bytes.
func NewChanIDFromOutPoint(op *wire.OutPoint) ChannelID {
	// First we'll copy the txid of the outpoint into our channel ID slice.
	var cid ChannelID
	copy(cid[:], op.Hash[:])

	// With the txid copied over, we'll now XOR the lower 2-bytes of the
	// partial channelID with big-endian serialization of output index.
	xorTxid(&cid, uint16(op.Index))

	return cid
}

// xorTxid performs the transformation needed to transform an OutPoint into a
// ChannelID. To do this, we expect the cid parameter to contain the txid
// unaltered and the outputIndex to be the output index
func xorTxid(cid *ChannelID, outputIndex uint16) {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], outputIndex)

	cid[30] ^= buf[0]
	cid[31] ^= buf[1]
}

// readOutpoint reads an outpoint from the passed reader.
func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Read(r, byteOrder, &o.Index); err != nil {
		return err
	}

	return nil
}
