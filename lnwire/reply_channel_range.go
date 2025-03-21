package lnwire

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// ReplyChannelRange is the response to the QueryChannelRange message. It
// includes the original query, and the next streaming chunk of encoded short
// channel ID's as the response. We'll also include a byte that indicates if
// this is the last query in the message.
type ReplyChannelRange struct {
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

	// Complete denotes if this is the conclusion of the set of streaming
	// responses to the original query.
	Complete uint8

	// EncodingType is a signal to the receiver of the message that
	// indicates exactly how the set of short channel ID's that follow have
	// been encoded.
	EncodingType QueryEncoding

	// ShortChanIDs is a slice of decoded short channel ID's.
	ShortChanIDs []ShortChannelID

	// Timestamps is an optional set of timestamps corresponding to the
	// latest timestamps for the channel update messages corresponding to
	// those referenced in the ShortChanIDs list. If this field is used,
	// then the length must match the length of ShortChanIDs.
	Timestamps Timestamps

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData

	// noSort indicates whether or not to sort the short channel ids before
	// writing them out.
	//
	// NOTE: This should only be used for testing.
	noSort bool
}

// NewReplyChannelRange creates a new empty ReplyChannelRange message.
func NewReplyChannelRange() *ReplyChannelRange {
	return &ReplyChannelRange{
		ExtraData: make([]byte, 0),
	}
}

// A compile time check to ensure ReplyChannelRange implements the
// lnwire.Message interface.
var _ Message = (*ReplyChannelRange)(nil)

// A compile time check to ensure ReplyChannelRange implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ReplyChannelRange)(nil)

// Decode deserializes a serialized ReplyChannelRange message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r,
		c.ChainHash[:],
		&c.FirstBlockHeight,
		&c.NumBlocks,
		&c.Complete,
	)
	if err != nil {
		return err
	}

	c.EncodingType, c.ShortChanIDs, err = decodeShortChanIDs(r)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var timeStamps Timestamps
	typeMap, err := tlvRecords.ExtractRecords(&timeStamps)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[TimestampsRecordType]; ok && val == nil {
		c.Timestamps = timeStamps

		// Check that a timestamp was provided for each SCID.
		if len(c.Timestamps) != len(c.ShortChanIDs) {
			return fmt.Errorf("number of timestamps does not " +
				"match number of SCIDs")
		}
	}

	if len(tlvRecords) != 0 {
		c.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target ReplyChannelRange into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteBytes(w, c.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteUint32(w, c.FirstBlockHeight); err != nil {
		return err
	}

	if err := WriteUint32(w, c.NumBlocks); err != nil {
		return err
	}

	if err := WriteUint8(w, c.Complete); err != nil {
		return err
	}

	// For both of the current encoding types, the channel ID's are to be
	// sorted in place, so we'll do that now. The sorting is applied unless
	// we were specifically requested not to for testing purposes.
	if !c.noSort {
		var scidPreSortIndex map[uint64]int
		if len(c.Timestamps) != 0 {
			// Sanity check that a timestamp was provided for each
			// SCID.
			if len(c.Timestamps) != len(c.ShortChanIDs) {
				return fmt.Errorf("must provide a timestamp " +
					"pair for each of the given SCIDs")
			}

			// Create a map from SCID value to the original index of
			// the SCID in the unsorted list.
			scidPreSortIndex = make(
				map[uint64]int, len(c.ShortChanIDs),
			)
			for i, scid := range c.ShortChanIDs {
				scidPreSortIndex[scid.ToUint64()] = i
			}

			// Sanity check that there were no duplicates in the
			// SCID list.
			if len(scidPreSortIndex) != len(c.ShortChanIDs) {
				return fmt.Errorf("scid list should not " +
					"contain duplicates")
			}
		}

		// Now sort the SCIDs.
		sort.Slice(c.ShortChanIDs, func(i, j int) bool {
			return c.ShortChanIDs[i].ToUint64() <
				c.ShortChanIDs[j].ToUint64()
		})

		if len(c.Timestamps) != 0 {
			timestamps := make(Timestamps, len(c.Timestamps))

			for i, scid := range c.ShortChanIDs {
				timestamps[i] = []ChanUpdateTimestamps(
					c.Timestamps,
				)[scidPreSortIndex[scid.ToUint64()]]
			}
			c.Timestamps = timestamps
		}
	}

	err := encodeShortChanIDs(w, c.EncodingType, c.ShortChanIDs)
	if err != nil {
		return err
	}

	recordProducers := make([]tlv.RecordProducer, 0, 1)
	if len(c.Timestamps) != 0 {
		recordProducers = append(recordProducers, &c.Timestamps)
	}
	err = EncodeMessageExtraData(&c.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ReplyChannelRange) MsgType() MessageType {
	return MsgReplyChannelRange
}

// LastBlockHeight returns the last block height covered by the range of a
// QueryChannelRange message.
func (c *ReplyChannelRange) LastBlockHeight() uint32 {
	// Handle overflows by casting to uint64.
	lastBlockHeight := uint64(c.FirstBlockHeight) + uint64(c.NumBlocks) - 1
	if lastBlockHeight > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(lastBlockHeight)
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ReplyChannelRange) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
