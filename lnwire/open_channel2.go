package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// OpenChannel2 is the message Alice sends to Bob if she would like to create a
// channel with Bob where they can both contribute funds to the channel.
type OpenChannel2 struct {
	// ChainHash is the target chain that the initiator wishes to open a
	// channel within.
	ChainHash chainhash.Hash

	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated dual funder workflow. It is initialized as:
	// SHA256(zeroed-revocation-basepoint || opener-revocation-basepoint).
	PendingChannelID [32]byte

	// FundingFeePerKWeight is the fee rate to be used for the funding
	// transaction. This value is expressed in sats per kilo-weight.
	//
	// TODO(halseth): make SatPerKWeight when fee estimation is in own
	// package. Currently this will cause an import cycle.
	FundingFeePerKWeight uint32

	// CommitFeePerKWeight is the initial fee rate that the initiator
	// suggests for both commitment transactions. This value is expressed in
	// sats per kilo-weight.
	//
	// TODO(halseth): make SatPerKWeight when fee estimation is in own
	// package. Currently this will cause an import cycle.
	CommitFeePerKWeight uint32

	// FundingAmount is the amount of satoshis that the initiator of the
	// channel wishes to contribute to the total capacity of the channel.
	// The non-initiator may also choose to contribute satoshis.
	FundingAmount btcutil.Amount

	// DustLimit is the specific dust limit the sender of this message
	// would like enforced on their version of the commitment transaction.
	// Any output below this value will be "trimmed" from the commitment
	// transaction, with the amount of the HTLC going to dust.
	DustLimit btcutil.Amount

	// MaxValueInFlight represents the maximum amount of coins that can be
	// pending within the channel at any given time. If the amount of funds
	// in limbo exceeds this amount, then the channel will be failed.
	MaxValueInFlight MilliSatoshi

	// HtlcMinimum is the smallest HTLC that the sender of this message
	// will accept.
	HtlcMinimum MilliSatoshi

	// CsvDelay is the number of blocks to use for the non-initiator's
	// relative time lock in the pay-to-self output of their commitment
	// transaction.
	CsvDelay uint16

	// MaxAcceptedHTLCs is the total number of incoming HTLC's that the
	// sender of this channel will accept.
	MaxAcceptedHTLCs uint16

	// LockTime is the locktime to be used for the funding transaction.
	LockTime uint32

	// FundingKey is the key that should be used on behalf of the sender
	// within the 2-of-2 multi-sig output that it contained within the
	// funding transaction.
	FundingKey *btcec.PublicKey

	// RevocationPoint is the base revocation point for the sending party.
	// Any commitment transaction belonging to the receiver of this message
	// should use this key and their per-commitment point to derive the
	// revocation key for the commitment transaction.
	RevocationPoint *btcec.PublicKey

	// PaymentPoint is used for outputs in the commitment transaction paying
	// immediately (no time-delay) to the sending party. Depending on the
	// option static_remote_key this will be tweaked by the per commitment
	// point. An example for this is the to_remote output of the receiving
	// party's commitment transaction.
	PaymentPoint *btcec.PublicKey

	// DelayedPaymentPoint is the delay point for the sending party. This
	// key should be combined with the per commitment point to derive the
	// keys that are used in outputs of the sender's commitment transaction
	// where they claim funds.
	DelayedPaymentPoint *btcec.PublicKey

	// HtlcPoint is the base point used to derive the set of keys for this
	// party that will be used within the HTLC public key scripts. This
	// value is combined with the receiver's revocation base point in order
	// to derive the keys that are used within HTLC scripts.
	HtlcPoint *btcec.PublicKey

	// FirstCommitmentPoint is the first commitment point for the sending
	// party. This value should be combined with the receiver's revocation
	// base point in order to derive the revocation keys that are placed
	// within the commitment transaction of the sender.
	FirstCommitmentPoint *btcec.PublicKey

	// ChannelFlags is a bit-field which allows the initiator of the
	// channel to specify further behavior surrounding the channel.
	// Currently, the least significant bit of this bit field indicates the
	// initiator of the channel wishes to advertise this channel publicly.
	ChannelFlags FundingFlag

	// UpfrontShutdownScript is the script to which the channel funds should
	// be paid when mutually closing the channel. This field is optional,
	// but we always write a TLV record for it. So if this field is not set,
	// a zero-length TLV record is encoded.
	UpfrontShutdownScript DeliveryAddress

	// ChannelType is the explicit channel type the initiator wishes to
	// open.
	ChannelType *ChannelType

	// LeaseExpiry represents the absolute expiration height of a channel
	// lease. This is a custom TLV record that will only apply when a leased
	// channel is being opened using the script enforced lease commitment
	// type.
	LeaseExpiry *LeaseExpiry

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure OpenChannel2 implements the lnwire.Message
// interface.
var _ Message = (*OpenChannel2)(nil)

// Encode serializes the target OpenChannel2 into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
//nolint:dupl
func (o *OpenChannel2) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := []tlv.RecordProducer{&o.UpfrontShutdownScript}
	if o.ChannelType != nil {
		recordProducers = append(recordProducers, o.ChannelType)
	}
	if o.LeaseExpiry != nil {
		recordProducers = append(recordProducers, o.LeaseExpiry)
	}
	err := EncodeMessageExtraData(&o.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteBytes(w, o.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteBytes(w, o.PendingChannelID[:]); err != nil {
		return err
	}

	if err := WriteUint32(w, o.FundingFeePerKWeight); err != nil {
		return err
	}

	if err := WriteUint32(w, o.CommitFeePerKWeight); err != nil {
		return err
	}

	if err := WriteSatoshi(w, o.FundingAmount); err != nil {
		return err
	}

	if err := WriteSatoshi(w, o.DustLimit); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, o.MaxValueInFlight); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, o.HtlcMinimum); err != nil {
		return err
	}

	if err := WriteUint16(w, o.CsvDelay); err != nil {
		return err
	}

	if err := WriteUint16(w, o.MaxAcceptedHTLCs); err != nil {
		return err
	}

	if err := WriteUint32(w, o.LockTime); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.FundingKey); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.RevocationPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.PaymentPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.DelayedPaymentPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.HtlcPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, o.FirstCommitmentPoint); err != nil {
		return err
	}

	if err := WriteFundingFlag(w, o.ChannelFlags); err != nil {
		return err
	}

	return WriteBytes(w, o.ExtraData)
}

// Decode deserializes the serialized OpenChannel2 stored in the passed
// io.Reader into the target OpenChannel2 using the deserialization rules
// defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel2) Decode(r io.Reader, pver uint32) error {
	// Read all the mandatory fields in the open message.
	err := ReadElements(r,
		o.ChainHash[:],
		o.PendingChannelID[:],
		&o.FundingFeePerKWeight,
		&o.CommitFeePerKWeight,
		&o.FundingAmount,
		&o.DustLimit,
		&o.MaxValueInFlight,
		&o.HtlcMinimum,
		&o.CsvDelay,
		&o.MaxAcceptedHTLCs,
		&o.LockTime,
		&o.FundingKey,
		&o.RevocationPoint,
		&o.PaymentPoint,
		&o.DelayedPaymentPoint,
		&o.HtlcPoint,
		&o.FirstCommitmentPoint,
		&o.ChannelFlags,
	)
	if err != nil {
		return err
	}

	// Next we'll parse out the set of known records, keeping the raw tlv
	// bytes untouched to ensure we don't drop any bytes erroneously.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var (
		chanType    ChannelType
		leaseExpiry LeaseExpiry
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&o.UpfrontShutdownScript, &chanType, &leaseExpiry,
	)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[ChannelTypeRecordType]; ok && val == nil {
		o.ChannelType = &chanType
	}
	if val, ok := typeMap[LeaseExpiryRecordType]; ok && val == nil {
		o.LeaseExpiry = &leaseExpiry
	}

	o.ExtraData = tlvRecords

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as an OpenChannel2 on the wire.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel2) MsgType() MessageType {
	return MsgOpenChannel2
}
