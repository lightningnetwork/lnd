package lnwire

import (
	"bytes"
	"io"
	"math"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// QueryChannelRange is a message sent by a node in order to query the
// receiving node of the set of open channel they know of with short channel
// ID's after the specified block height, capped at the number of blocks beyond
// that block height. This will be used by nodes upon initial connect to
// synchronize their views of the network.
type QueryChannelRange struct {
	// ChainHash denotes the target chain that we're trying to synchronize
	// channel graph state for.
	ChainHash chainhash.Hash

	// FirstBlockHeight is the first block in the query range. The
	// responder should send all new short channel IDs from this block
	// until this block plus the specified number of blocks.
	FirstBlockHeight uint32

	// NumBlocks is the number of blocks beyond the first block that short
	// channel ID's should be sent for.
	NumBlocks uint32

	// QueryOptions is an optional feature bit vector that can be used to
	// specify additional query options.
	QueryOptions *QueryOptions

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewQueryChannelRange creates a new empty QueryChannelRange message.
func NewQueryChannelRange() *QueryChannelRange {
	return &QueryChannelRange{
		ExtraData: make([]byte, 0),
	}
}

// A compile time check to ensure QueryChannelRange implements the
// lnwire.Message interface.
var _ Message = (*QueryChannelRange)(nil)

// A compile time check to ensure QueryChannelRange implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*QueryChannelRange)(nil)

// Decode deserializes a serialized QueryChannelRange message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (q *QueryChannelRange) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(
		r, q.ChainHash[:], &q.FirstBlockHeight, &q.NumBlocks,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var queryOptions QueryOptions
	typeMap, err := tlvRecords.ExtractRecords(&queryOptions)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[QueryOptionsRecordType]; ok && val == nil {
		q.QueryOptions = &queryOptions
	}

	if len(tlvRecords) != 0 {
		q.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target QueryChannelRange into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (q *QueryChannelRange) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteBytes(w, q.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteUint32(w, q.FirstBlockHeight); err != nil {
		return err
	}

	if err := WriteUint32(w, q.NumBlocks); err != nil {
		return err
	}

	recordProducers := make([]tlv.RecordProducer, 0, 1)
	if q.QueryOptions != nil {
		recordProducers = append(recordProducers, q.QueryOptions)
	}
	err := EncodeMessageExtraData(&q.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, q.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (q *QueryChannelRange) MsgType() MessageType {
	return MsgQueryChannelRange
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (q *QueryChannelRange) SerializedSize() (uint32, error) {
	msgCpy := *q
	return MessageSerializedSize(&msgCpy)
}

// LastBlockHeight returns the last block height covered by the range of a
// QueryChannelRange message.
func (q *QueryChannelRange) LastBlockHeight() uint32 {
	// Handle overflows by casting to uint64.
	lastBlockHeight := uint64(q.FirstBlockHeight) + uint64(q.NumBlocks) - 1
	if lastBlockHeight > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(lastBlockHeight)
}

// WithTimestamps returns true if the query has asked for timestamps too.
func (q *QueryChannelRange) WithTimestamps() bool {
	if q.QueryOptions == nil {
		return false
	}

	queryOpts := RawFeatureVector(*q.QueryOptions)

	return queryOpts.IsSet(QueryOptionTimestampBit)
}
