package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ClosingSignedFeeRangeType is the type used to store the optional fee
	// range field into the ClosingSigned message.
	ClosingSignedFeeRangeType = 1

	// FeeRangeRecordSize is the amount of bytes that fee range record size
	// occupies (two 8 byte integers).
	FeeRangeRecordSize = 16
)

// FeeRange is a structured TLV contained within the ClosingSigned message that
// allows both parties to opt into a more constrained negotiation algorithm. In
// the ideal case, the funder sends over their fee range, which is accepted by
// the responder and negotiation completes. The non-initiator is also able to
// initiate the new process by sending over a fee rate (with their max), with
// the funder echo'ing back the value if they find it to be acceptable.
type FeeRange struct {
	// MinFeeSats is the smallest acceptable fee that the sending party will
	// accept.
	MinFeeSats btcutil.Amount

	// MaxFeeSats is the largest acceptable fee that the sending party will
	// accept.
	MaxFeeSats btcutil.Amount
}

// NewRecord returns a TLV record that can be used to optionally encode a
// specified fee range to allow the fee negotiation process to be comprssed
// down to a minimal amount of steps.
func (f *FeeRange) NewRecord() tlv.Record {
	return tlv.MakeStaticRecord(
		ClosingSignedFeeRangeType, f, FeeRangeRecordSize,
		eFeeRange, dFeeRange,
	)
}

// eFeeRange is a function used to encode the fee range struct as a fixed sized
// TLV record.
func eFeeRange(w io.Writer, val interface{}, buf *[8]byte) error {
	// Simply write out the stored as part of the fee range back to back.
	if feeRange, ok := val.(*FeeRange); ok {
		err := tlv.EUint64T(w, uint64(feeRange.MinFeeSats), buf)
		if err != nil {
			return err
		}

		return tlv.EUint64T(w, uint64(feeRange.MaxFeeSats), buf)
	}
	return tlv.NewTypeForEncodingErr(val, "FeeRange")
}

// dFeeRange attempts to decode the encoded TLV record within the passed
// io.Reader into the target val (FeeRange pointer).
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

// packFeeRange attempts to encode a FeeRange TLV into the passed io.Writer if
// specified.
func packFeeRange(feeRange *FeeRange,
	extraData ExtraOpaqueData) (ExtraOpaqueData, error) {

	// We only need to encode this field if it's specified, so if it isn't
	// present, then we'll just return the set of opaque bytes as is.
	if feeRange == nil {
		return extraData, nil
	}

	// Otherwise, we'll pack the feeRange directly in as a set of TLV
	// records.
	var tlvRecords ExtraOpaqueData
	err := tlvRecords.PackRecords(feeRange.NewRecord())
	if err != nil {
		return nil, fmt.Errorf("unable to pack fee range as TLV "+
			"record: %v", err)
	}

	return tlvRecords, nil
}

// parseFeeRange attempts to decode a FeeRange TLV along with any other TLV
// types that we may not recognize.
func parseFeeRange(tlvRecords ExtraOpaqueData) (
	*FeeRange, ExtraOpaqueData, error) {

	// If no TLV data is present there can't be any script available.
	if len(tlvRecords) == 0 {
		return nil, tlvRecords, nil
	}

	// We have some TLV data, so we'll attempt to parse out the optional
	// fee range record.
	feeRange := new(FeeRange)
	parsedTLVs, err := tlvRecords.ExtractRecords(feeRange.NewRecord())
	if err != nil {
		return nil, nil, err
	}

	// If the set of parsed TLVs doesn't include the optional fee range
	// type, then we'll just return a nil value, and the opaque data as is.
	_, ok := parsedTLVs[ClosingSignedFeeRangeType]
	if !ok {
		return nil, tlvRecords, nil
	}

	// Otherwise, we've found the type we're looking for (has been decoded
	// into the value), so we'll return it directly, snipping off the bytes
	// of the extra data.
	//
	// TODO(roasbeef): bring back old method by wpaulino that does this?
	tlvRecords = tlvRecords[FeeRangeRecordSize+2:]
	return feeRange, tlvRecords, nil
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

	// FeeRange is an optional set of fee range hints both sides can
	// provide in order to compress the co-op close negotiation process.
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
		FeeSatoshis: fs,
		FeeRange:    fr,
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
		r, &c.ChannelID, &c.FeeSatoshis, &c.Signature,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	c.FeeRange, c.ExtraData, err = parseFeeRange(
		tlvRecords,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target ClosingSigned into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSigned) Encode(w *bytes.Buffer, pver uint32) error {
	tlvRecords, err := packFeeRange(c.FeeRange, c.ExtraData)
	if err != nil {
		return err
	}

	if err := WriteChannelID(w, c.ChannelID); err != nil {
		return err
	}

	if err := WriteSatoshi(w, c.FeeSatoshis); err != nil {
		return err
	}

	if err := WriteSig(w, c.Signature); err != nil {
		return err
	}

	return WriteBytes(w, tlvRecords)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSigned) MsgType() MessageType {
	return MsgClosingSigned
}
