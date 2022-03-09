package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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
}

// A compile time check to ensure AcceptChannel implements the lnwire.Message
// interface.
var _ Message = (*AcceptChannel)(nil)

// Encode serializes the target AcceptChannel into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		a.PendingChannelID[:],
		a.DustLimit,
		a.MaxValueInFlight,
		a.ChannelReserve,
		a.HtlcMinimum,
		a.MinAcceptDepth,
		a.CsvDelay,
		a.MaxAcceptedHTLCs,
		a.FundingKey,
		a.RevocationPoint,
		a.PaymentPoint,
		a.DelayedPaymentPoint,
		a.HtlcPoint,
		a.FirstCommitmentPoint,
		a.UpfrontShutdownScript,
	)
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

	// Check for the optional upfront shutdown script field. If it is not there,
	// silence the EOF error.
	err = ReadElement(r, &a.UpfrontShutdownScript)
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as an AcceptChannel on the wire.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) MsgType() MessageType {
	return MsgAcceptChannel
}

// MaxPayloadLength returns the maximum allowed payload length for a
// AcceptChannel message.
//
// This is part of the lnwire.Message interface.
func (a *AcceptChannel) MaxPayloadLength(uint32) uint32 {
	// 32 + (8 * 4) + (4 * 1) + (2 * 2) + (33 * 6)
	var length uint32 = 270 // base length

	// Upfront shutdown script max length.
	length += 2 + deliveryAddressMaxSize

	return length
}
