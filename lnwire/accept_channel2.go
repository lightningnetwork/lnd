package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// AcceptChannel2 is the message Bob sends to Alice after she initiates the dual
// funding channel workflow via an OpenChannel2 message. Once Alice receives
// Bob's response, they can begin the collaborative construction of the funding
// transaction for this channel.
type AcceptChannel2 struct {
	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated single funder workflow. It is initialized to
	// SHA256(zeroed-revocation-basepoint || opener-revocation-basepoint).
	PendingChannelID [32]byte

	// FundingAmount is the amount of satoshis that the non-initiator of the
	// channel wishes to contribute to the total capacity of the channel.
	// The initator may have also contributed an amount in OpenChannel2.
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

	// MinAcceptDepth is the minimum depth that the initiator of the
	// channel should wait before considering the channel open.
	MinAcceptDepth uint32

	// CsvDelay is the number of blocks to use for the initiator's relative
	// time lock in the pay-to-self output of their commitment transaction.
	CsvDelay uint16

	// MaxAcceptedHTLCs is the total number of incoming HTLC that the sender
	// of this message will accept.
	MaxAcceptedHTLCs uint16

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
	// party that will be used within the HTLC public key scripts.  This
	// value is combined with the receiver's revocation base point in order
	// to derive the keys that are used within HTLC scripts.
	HtlcPoint *btcec.PublicKey

	// FirstCommitmentPoint is the first commitment point for the sending
	// party. This value should be combined with the receiver's revocation
	// base point in order to derive the revocation keys that are placed
	// within the commitment transaction of the sender.
	FirstCommitmentPoint *btcec.PublicKey

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

// A compile time check to ensure AcceptChannel2 implements the lnwire.Message
// interface.
var _ Message = (*AcceptChannel2)(nil)

// Encode serializes the target AcceptChannel2 into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
//
//nolint:dupl
func (a *AcceptChannel2) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := []tlv.RecordProducer{&a.UpfrontShutdownScript}
	if a.ChannelType != nil {
		recordProducers = append(recordProducers, a.ChannelType)
	}
	if a.LeaseExpiry != nil {
		recordProducers = append(recordProducers, a.LeaseExpiry)
	}
	err := EncodeMessageExtraData(&a.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteBytes(w, a.PendingChannelID[:]); err != nil {
		return err
	}

	if err := WriteSatoshi(w, a.FundingAmount); err != nil {
		return err
	}

	if err := WriteSatoshi(w, a.DustLimit); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, a.MaxValueInFlight); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, a.HtlcMinimum); err != nil {
		return err
	}

	if err := WriteUint32(w, a.MinAcceptDepth); err != nil {
		return err
	}

	if err := WriteUint16(w, a.CsvDelay); err != nil {
		return err
	}

	if err := WriteUint16(w, a.MaxAcceptedHTLCs); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.FundingKey); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.RevocationPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.PaymentPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.DelayedPaymentPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.HtlcPoint); err != nil {
		return err
	}

	if err := WritePublicKey(w, a.FirstCommitmentPoint); err != nil {
		return err
	}

	return WriteBytes(w, a.ExtraData)
}

// Decode deserializes the serialized AcceptChannel2 stored in the passed
// io.Reader into the target AcceptChannel2 using the deserialization rules
// defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel2) Decode(r io.Reader, pver uint32) error {
	// Read all the mandatory fields in the accept message.
	err := ReadElements(r,
		a.PendingChannelID[:],
		&a.FundingAmount,
		&a.DustLimit,
		&a.MaxValueInFlight,
		&a.HtlcMinimum,
		&a.MinAcceptDepth,
		&a.CsvDelay,
		&a.MaxAcceptedHTLCs,
		&a.FundingKey,
		&a.RevocationPoint,
		&a.PaymentPoint,
		&a.DelayedPaymentPoint,
		&a.HtlcPoint,
		&a.FirstCommitmentPoint,
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
		&a.UpfrontShutdownScript, &chanType, &leaseExpiry,
	)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[ChannelTypeRecordType]; ok && val == nil {
		a.ChannelType = &chanType
	}
	if val, ok := typeMap[LeaseExpiryRecordType]; ok && val == nil {
		a.LeaseExpiry = &leaseExpiry
	}

	a.ExtraData = tlvRecords

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as an AcceptChannel2 on the wire.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel2) MsgType() MessageType {
	return MsgAcceptChannel2
}
