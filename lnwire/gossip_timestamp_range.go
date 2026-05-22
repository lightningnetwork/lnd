package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// GossipTimestampRange is a message that allows the sender to restrict the set
// of future gossip announcements sent by the receiver. Nodes should send this
// if they have the gossip-queries feature bit active. Nodes are able to send
// new GossipTimestampRange messages to replace the prior window.
type GossipTimestampRange struct {
	// ChainHash denotes the chain that the sender wishes to restrict the
	// set of received announcements of.
	ChainHash chainhash.Hash

	// FirstTimestamp is the timestamp of the earliest announcement message
	// that should be sent by the receiver. This is only to be used for
	// querying message of gossip 1.0 which are timestamped using Unix
	// timestamps. FirstBlockHeight and BlockRange should be used to
	// query for announcement messages timestamped using block heights.
	FirstTimestamp uint32

	// TimestampRange is the horizon beyond the FirstTimestamp that any
	// announcement messages should be sent for. The receiving node MUST
	// NOT send any announcements that have a timestamp greater than
	// FirstTimestamp + TimestampRange. This is used together with
	// FirstTimestamp to query for gossip 1.0 messages timestamped with
	// Unix timestamps.
	TimestampRange uint32

	// BlockHeightRange is the block-height equivalent of (FirstTimestamp,
	// TimestampRange) used for gossip messages timestamped by block
	// height. The receiving node MUST NOT send any announcements that have
	// a block height greater than BlockHeightRange.FirstBlockHeight +
	// BlockHeightRange.NumBlocks.
	BlockHeightRange tlv.OptionalRecordT[tlv.TlvType2, BlockHeightRange]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewGossipTimestampRange creates a new empty GossipTimestampRange message.
func NewGossipTimestampRange() *GossipTimestampRange {
	return &GossipTimestampRange{}
}

// BlockHeightRange describes a window of blocks that an announcement may have
// been timestamped within. It is serialised as a u32 first_block_height
// followed by a tu32 num_blocks; the tu32 (truncated uint32) drops any
// leading zero bytes from num_blocks to save space on the wire.
type BlockHeightRange struct {
	// FirstBlockHeight is the height of the earliest announcement message
	// that should be sent by the receiver.
	FirstBlockHeight uint32

	// NumBlocks is the size of the window beyond FirstBlockHeight that
	// announcements should be sent for.
	NumBlocks uint32
}

// Record returns the TLV record used to encode/decode a BlockHeightRange. The
// type number is supplied by the wrapping RecordT.
func (b *BlockHeightRange) Record() tlv.Record {
	sizeFunc := func() uint64 {
		return 4 + tlv.SizeTUint32(b.NumBlocks)
	}

	return tlv.MakeDynamicRecord(
		0, b, sizeFunc, blockHeightRangeEncoder,
		blockHeightRangeDecoder,
	)
}

func blockHeightRangeEncoder(w io.Writer, val interface{},
	buf *[8]byte) error {

	v, ok := val.(*BlockHeightRange)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "lnwire.BlockHeightRange")
	}

	if err := tlv.EUint32T(w, v.FirstBlockHeight, buf); err != nil {
		return err
	}

	return tlv.ETUint32T(w, v.NumBlocks, buf)
}

func blockHeightRangeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	v, ok := val.(*BlockHeightRange)
	if !ok || l < 4 {
		return tlv.NewTypeForDecodingErr(
			val, "lnwire.BlockHeightRange", l, 4,
		)
	}

	if err := tlv.DUint32(r, &v.FirstBlockHeight, buf, 4); err != nil {
		return err
	}

	return tlv.DTUint32(r, &v.NumBlocks, buf, l-4)
}

// A compile time check to ensure GossipTimestampRange implements the
// lnwire.Message interface.
var _ Message = (*GossipTimestampRange)(nil)

// A compile time check to ensure GossipTimestampRange implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*GossipTimestampRange)(nil)

// Decode deserializes a serialized GossipTimestampRange message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (g *GossipTimestampRange) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(r,
		g.ChainHash[:],
		&g.FirstTimestamp,
		&g.TimestampRange,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	bhRange := tlv.ZeroRecordT[tlv.TlvType2, BlockHeightRange]()
	typeMap, err := tlvRecords.ExtractRecords(&bhRange)
	if err != nil {
		return err
	}

	if val, ok := typeMap[g.BlockHeightRange.TlvType()]; ok && val == nil {
		g.BlockHeightRange = tlv.SomeRecordT(bhRange)
	}

	if len(tlvRecords) != 0 {
		g.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target GossipTimestampRange into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (g *GossipTimestampRange) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteBytes(w, g.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteUint32(w, g.FirstTimestamp); err != nil {
		return err
	}

	if err := WriteUint32(w, g.TimestampRange); err != nil {
		return err
	}

	recordProducers := make([]tlv.RecordProducer, 0, 1)
	g.BlockHeightRange.WhenSome(
		func(bhr tlv.RecordT[tlv.TlvType2, BlockHeightRange]) {
			recordProducers = append(recordProducers, &bhr)
		},
	)
	err := EncodeMessageExtraData(&g.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, g.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (g *GossipTimestampRange) MsgType() MessageType {
	return MsgGossipTimestampRange
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (g *GossipTimestampRange) SerializedSize() (uint32, error) {
	return MessageSerializedSize(g)
}
