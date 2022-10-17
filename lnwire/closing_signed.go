package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// FeeRangeType is the type used to store the optional fee range field
	// in the ClosingSigned message.
	FeeRangeType tlv.Type = 1

	// FeeRangeRecordSize is the amount of bytes the fee range record size
	// occupies (two uint64s).
	FeeRangeRecordSize uint64 = 16
)

// FeeRange is a TLV in the ClosingSigned message that allows the sender to
// specify the minimum and maximum fee it will accept for a coop closing
// transaction. This version of the coop closing flow will usually complete in
// 2 or 3 rounds, but may take more if each side's fee range doesn't overlap.
type FeeRange struct {
	// MinFeeSats is the minimum fee that the sender will accept.
	MinFeeSats btcutil.Amount

	// MaxFeeSats is the maximum fee that the sender will accept.
	MaxFeeSats btcutil.Amount
}

// InRange returns whether a fee is in the fee range.
func (f *FeeRange) InRange(fee btcutil.Amount) bool {
	return f.MinFeeSats <= fee && fee <= f.MaxFeeSats
}

// GetOverlap takes two FeeRanges and returns the overlapping FeeRange between
// the two. If there is no overlap, nil is returned.
func (f *FeeRange) GetOverlap(other *FeeRange) *FeeRange {
	var (
		minOfUpper btcutil.Amount
		maxOfLower btcutil.Amount
	)

	// Determine the maximum of the lower bounds.
	if f.MinFeeSats >= other.MinFeeSats {
		maxOfLower = f.MinFeeSats
	} else {
		maxOfLower = other.MinFeeSats
	}

	// Determine the minimum of the upper bounds.
	if f.MaxFeeSats <= other.MaxFeeSats {
		minOfUpper = f.MaxFeeSats
	} else {
		minOfUpper = other.MaxFeeSats
	}

	// If the maximum of the lower bounds is greater than the minimum of the
	// upper bounds, then there is no overlap.
	if maxOfLower > minOfUpper {
		return nil
	}

	// There is an overlap, so return the range.
	return &FeeRange{
		MinFeeSats: maxOfLower,
		MaxFeeSats: minOfUpper,
	}
}

// NewRecord returns a TLV record that can be used to optionally encode a fee
// range that allows the fee negotiation process to use less rounds.
func (f *FeeRange) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		FeeRangeType, f, FeeRangeRecordSize, eFeeRange, dFeeRange,
	)
}

// eFeeRange is used to encode the fee range struct as a TLV record.
func eFeeRange(w io.Writer, val interface{}, buf *[8]byte) error {
	if feeRange, ok := val.(*FeeRange); ok {
		err := tlv.EUint64T(w, uint64(feeRange.MinFeeSats), buf)
		if err != nil {
			return err
		}

		return tlv.EUint64T(w, uint64(feeRange.MaxFeeSats), buf)
	}

	return tlv.NewTypeForEncodingErr(val, "FeeRange")
}

// dFeeRange is used to decode the fee range TLV record into a FeeRange struct.
func dFeeRange(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if feeRange, ok := val.(*FeeRange); ok && l == FeeRangeRecordSize {
		var min, max uint64

		if err := tlv.DUint64(r, &min, buf, 8); err != nil {
			return err
		}
		if err := tlv.DUint64(r, &max, buf, 8); err != nil {
			return err
		}

		feeRange.MinFeeSats = btcutil.Amount(min)
		feeRange.MaxFeeSats = btcutil.Amount(max)
		return nil
	}
	return tlv.NewTypeForDecodingErr(val, "FeeRange", l, FeeRangeRecordSize)
}

// ClosingSigned is sent by both parties to a channel once the channel is clear
// of HTLCs, and is primarily concerned with negotiating fees for the close
// transaction. Each party provides a signature for a transaction with a fee
// that they believe is fair. The process terminates when both sides agree on
// the same fee, or when one side force closes the channel.
//
// NOTE: The responder is able to send a signature without any additional
// messages as all transactions are assembled observing BIP 69 which defines a
// canonical ordering for input/outputs. Therefore, both sides are able to
// arrive at an identical closure transaction as they know the order of the
// inputs/outputs.
type ClosingSigned struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// FeeSatoshis is the total fee in satoshis that the party to the
	// channel would like to propose for the close transaction.
	FeeSatoshis btcutil.Amount

	// Signature is for the proposed channel close transaction.
	Signature Sig

	// FeeRange is an optional range that the sender can set to compress the
	// number of rounds in the coop close process.
	FeeRange *FeeRange

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewClosingSigned creates a new empty ClosingSigned message.
func NewClosingSigned(cid ChannelID, fs btcutil.Amount, sig Sig,
	fr *FeeRange) *ClosingSigned {

	return &ClosingSigned{
		ChannelID:   cid,
		FeeRange:    fr,
		FeeSatoshis: fs,
		Signature:   sig,
	}
}

// A compile time check to ensure ClosingSigned implements the lnwire.Message
// interface.
var _ Message = (*ClosingSigned)(nil)

// Decode deserializes a serialized ClosingSigned message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSigned) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(
		r, &c.ChannelID, &c.FeeSatoshis, &c.Signature, &c.ExtraData,
	)
	if err != nil {
		return err
	}

	// Attempt to parse out the FeeRange record.
	var fr FeeRange
	typeMap, err := c.ExtraData.ExtractRecords(&fr)
	if err != nil {
		return err
	}

	// Only set the FeeRange record if the TLV type was included in the
	// stream.
	if val, ok := typeMap[FeeRangeType]; ok && val == nil {
		// Check that the fee range is sane before setting it.
		if fr.MinFeeSats > fr.MaxFeeSats {
			return fmt.Errorf("MinFeeSats greater than " +
				"MaxFeeSats")
		}

		// Check that FeeSatoshis is in the FeeRange.
		if c.FeeSatoshis < fr.MinFeeSats ||
			c.FeeSatoshis > fr.MaxFeeSats {

			return fmt.Errorf("FeeSatoshis is not in FeeRange")
		}

		c.FeeRange = &fr
	}

	return nil
}

// Encode serializes the target ClosingSigned into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSigned) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChannelID); err != nil {
		return err
	}

	if err := WriteSatoshi(w, c.FeeSatoshis); err != nil {
		return err
	}

	if err := WriteSig(w, c.Signature); err != nil {
		return err
	}

	// We'll only encode the FeeRange in a TLV segment if it exists.
	if c.FeeRange != nil {
		recordProducers := []tlv.RecordProducer{c.FeeRange}
		err := EncodeMessageExtraData(&c.ExtraData, recordProducers...)
		if err != nil {
			return err
		}
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSigned) MsgType() MessageType {
	return MsgClosingSigned
}
