package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// FundingFlag represents the possible bit mask values for the ChannelFlags
// field within the OpenChannel struct.
type FundingFlag uint8

const (
	// FFAnnounceChannel is a FundingFlag that when set, indicates the
	// initiator of a funding flow wishes to announce the channel to the
	// greater network.
	FFAnnounceChannel FundingFlag = 1 << iota
)

// OpenChannel is the message Alice sends to Bob if we should like to create a
// channel with Bob where she's the sole provider of funds to the channel.
// Single funder channels simplify the initial funding workflow, are supported
// by nodes backed by SPV Bitcoin clients, and have a simpler security models
// than dual funded channels.
type OpenChannel struct {
	// ChainHash is the target chain that the initiator wishes to open a
	// channel within.
	ChainHash chainhash.Hash

	// PendingChannelID serves to uniquely identify the future channel
	// created by the initiated single funder workflow.
	PendingChannelID [32]byte

	// FundingAmount is the amount of satoshis that the initiator of the
	// channel wishes to use as the total capacity of the channel. The
	// initial balance of the funding will be this value minus the push
	// amount (if set).
	FundingAmount btcutil.Amount

	// PushAmount is the value that the initiating party wishes to "push"
	// to the responding as part of the first commitment state. If the
	// responder accepts, then this will be their initial balance.
	PushAmount MilliSatoshi

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

	// FeePerKiloWeight is the initial fee rate that the initiator suggests
	// for both commitment transaction. This value is expressed in sat per
	// kilo-weight.
	//
	// TODO(halseth): make SatPerKWeight when fee estimation is in own
	// package. Currently this will cause an import cycle.
	FeePerKiloWeight uint32

	// CsvDelay is the number of blocks to use for the relative time lock
	// in the pay-to-self output of both commitment transactions.
	CsvDelay uint16

	// MaxAcceptedHTLCs is the total number of incoming HTLC's that the
	// sender of this channel will accept.
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
	// be paid when mutually closing the channel. This field is optional, and
	// and has a length prefix, so a zero will be written if it is not set
	// and its length followed by the script will be written if it is set.
	UpfrontShutdownScript DeliveryAddress
}

// A compile time check to ensure OpenChannel implements the lnwire.Message
// interface.
var _ Message = (*OpenChannel)(nil)

// Encode serializes the target OpenChannel into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		o.ChainHash[:],
		o.PendingChannelID[:],
		o.FundingAmount,
		o.PushAmount,
		o.DustLimit,
		o.MaxValueInFlight,
		o.ChannelReserve,
		o.HtlcMinimum,
		o.FeePerKiloWeight,
		o.CsvDelay,
		o.MaxAcceptedHTLCs,
		o.FundingKey,
		o.RevocationPoint,
		o.PaymentPoint,
		o.DelayedPaymentPoint,
		o.HtlcPoint,
		o.FirstCommitmentPoint,
		o.ChannelFlags,
		o.UpfrontShutdownScript,
	)
}

// Decode deserializes the serialized OpenChannel stored in the passed
// io.Reader into the target OpenChannel using the deserialization rules
// defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel) Decode(r io.Reader, pver uint32) error {
	if err := ReadElements(r,
		o.ChainHash[:],
		o.PendingChannelID[:],
		&o.FundingAmount,
		&o.PushAmount,
		&o.DustLimit,
		&o.MaxValueInFlight,
		&o.ChannelReserve,
		&o.HtlcMinimum,
		&o.FeePerKiloWeight,
		&o.CsvDelay,
		&o.MaxAcceptedHTLCs,
		&o.FundingKey,
		&o.RevocationPoint,
		&o.PaymentPoint,
		&o.DelayedPaymentPoint,
		&o.HtlcPoint,
		&o.FirstCommitmentPoint,
		&o.ChannelFlags,
	); err != nil {
		return err
	}

	// Check for the optional upfront shutdown script field. If it is not there,
	// silence the EOF error.
	err := ReadElement(r, &o.UpfrontShutdownScript)
	if err != nil && err != io.EOF {
		return err
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as an OpenChannel on the wire.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel) MsgType() MessageType {
	return MsgOpenChannel
}

// MaxPayloadLength returns the maximum allowed payload length for a
// OpenChannel message.
//
// This is part of the lnwire.Message interface.
func (o *OpenChannel) MaxPayloadLength(uint32) uint32 {
	// (32 * 2) + (8 * 6) + (4 * 1) + (2 * 2) + (33 * 6) + 1
	var length uint32 = 319 // base length

	// Upfront shutdown script max length.
	length += 2 + deliveryAddressMaxSize

	return length
}
