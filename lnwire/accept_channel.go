package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// AcceptChannel is the message Bob sends to Alice after she initiates the
// single funder channel workflow via an AcceptChannel message. Once Alice
// receives Bob's response, then she has all the items necessary to construct
// the funding transaction, and both commitment transactions.
type AcceptChannel struct {
	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated single funder workflow.
	PendingChannelID [32]byte

	// DustLimit is the specific dust limit the sender of this message
	// would like enforced on their version of the commitment transaction.
	// Any output below this value will be "trimmed" from the commitment
	// transaction, with the amount of the HTLC going to dust.
	DustLimit btcutil.Amount

	// MaxValueInFlight represents the maximum amount of coins that can be
	// pending within the channel at any given time. If the amount of funds
	// in limbo exceeds this amount, then the channel will be failed.
	MaxValueInFlight MilliSatoshi

	// ChannelReserve is the amount of BTC that the receiving party MUST
	// maintain a balance above at all times. This is a safety mechanism to
	// ensure that both sides always have skin in the game during the
	// channel's lifetime.
	ChannelReserve btcutil.Amount

	// HtlcMinimum is the smallest HTLC that the sender of this message
	// will accept.
	HtlcMinimum MilliSatoshi

	// MinAcceptDepth is the minimum depth that the initiator of the
	// channel should wait before considering the channel open.
	MinAcceptDepth uint32

	// CsvDelay is the number of blocks to use for the relative time lock
	// in the pay-to-self output of both commitment transactions.
	CsvDelay uint16

	// MaxAcceptedHTLCs is the total number of incoming HTLC's that the
	// sender of this channel will accept.
	//
	// TODO(roasbeef): acks the initiator's, same with max in flight?
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

	// PaymentPoint is the base payment point for the sending party. This
	// key should be combined with the per commitment point for a
	// particular commitment state in order to create the key that should
	// be used in any output that pays directly to the sending party, and
	// also within the HTLC covenant transactions.
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
	// be paid when mutually closing the channel. This field is optional, and
	// and has a length prefix, so a zero will be written if it is not set
	// and its length followed by the script will be written if it is set.
	UpfrontShutdownScript DeliveryAddress

	// ChannelType is the explicit channel type the initiator wishes to
	// open.
	ChannelType *ChannelType

	// LeaseExpiry represents the absolute expiration height of a channel
	// lease. This is a custom TLV record that will only apply when a leased
	// channel is being opened using the script enforced lease commitment
	// type.
	LeaseExpiry *LeaseExpiry

	// LocalNonce is an optional field that transmits the
	// local/verification nonce for a party. This nonce will be used to
	// verify the very first commitment transaction signature.
	// This will only be populated if the simple taproot channels type was
	// negotiated.
	LocalNonce OptMusig2NonceTLV

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	//
	// NOTE: Since the upfront shutdown script MUST be present (though can
	// be zero-length) if any TLV data is available, the script will be
	// extracted and removed from this blob when decoding. ExtraData will
	// contain all TLV records _except_ the DeliveryAddress record in that
	// case.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure AcceptChannel implements the lnwire.Message
// interface.
var _ Message = (*AcceptChannel)(nil)

// A compile time check to ensure AcceptChannel implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*AcceptChannel)(nil)

// Encode serializes the target AcceptChannel into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := []tlv.RecordProducer{&a.UpfrontShutdownScript}
	if a.ChannelType != nil {
		recordProducers = append(recordProducers, a.ChannelType)
	}
	if a.LeaseExpiry != nil {
		recordProducers = append(recordProducers, a.LeaseExpiry)
	}
	a.LocalNonce.WhenSome(func(localNonce Musig2NonceTLV) {
		recordProducers = append(recordProducers, &localNonce)
	})
	err := EncodeMessageExtraData(&a.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteBytes(w, a.PendingChannelID[:]); err != nil {
		return err
	}

	if err := WriteSatoshi(w, a.DustLimit); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, a.MaxValueInFlight); err != nil {
		return err
	}

	if err := WriteSatoshi(w, a.ChannelReserve); err != nil {
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

// Decode deserializes the serialized AcceptChannel stored in the passed
// io.Reader into the target AcceptChannel using the deserialization rules
// defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) Decode(r io.Reader, pver uint32) error {
	// Read all the mandatory fields in the accept message.
	err := ReadElements(r,
		a.PendingChannelID[:],
		&a.DustLimit,
		&a.MaxValueInFlight,
		&a.ChannelReserve,
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

	// For backwards compatibility, the optional extra data blob for
	// AcceptChannel must contain an entry for the upfront shutdown script.
	// We'll read it out and attempt to parse it.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	// Next we'll parse out the set of known records, keeping the raw tlv
	// bytes untouched to ensure we don't drop any bytes erroneously.
	var (
		chanType    ChannelType
		leaseExpiry LeaseExpiry
		localNonce  = a.LocalNonce.Zero()
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&a.UpfrontShutdownScript, &chanType, &leaseExpiry,
		&localNonce,
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
	if val, ok := typeMap[a.LocalNonce.TlvType()]; ok && val == nil {
		a.LocalNonce = tlv.SomeRecordT(localNonce)
	}

	a.ExtraData = tlvRecords

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as an AcceptChannel on the wire.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) MsgType() MessageType {
	return MsgAcceptChannel
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (a *AcceptChannel) SerializedSize() (uint32, error) {
	return MessageSerializedSize(a)
}
