package lnwire

import (
	"bytes"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// ChannelUpdate message is used after channel has been initially announced.
// Each side independently announces its fees and minimum expiry for HTLCs and
// other parameters. Also this message is used to redeclare initially setted
// channel parameters.
type ChannelUpdate struct {
	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature *btcec.Signature

	// ShortChannelID is the unique description of the funding transaction.
	ShortChannelID ShortChannelID

	// Timestamp allows ordering in the case of multiple announcements.
	// We should ignore the message if timestamp is not greater than
	// the last-received.
	Timestamp uint32

	// Flags least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise.
	Flags uint16

	// TimeLockDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	TimeLockDelta uint16

	// HtlcMinimumMsat is the minimum HTLC value which will be accepted.
	HtlcMinimumMsat uint64

	// BaseFee is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment.
	BaseFee uint32

	// FeeRate is the fee rate that will be charged per millionth of a
	// satoshi.
	FeeRate uint32
}

// A compile time check to ensure ChannelUpdate implements the lnwire.Message
// interface.
var _ Message = (*ChannelUpdate)(nil)

// Decode deserializes a serialized ChannelUpdate stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&a.Signature,
		&a.ShortChannelID,
		&a.Timestamp,
		&a.Flags,
		&a.TimeLockDelta,
		&a.HtlcMinimumMsat,
		&a.BaseFee,
		&a.FeeRate,
	)
}

// Encode serializes the target ChannelUpdate into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.Signature,
		a.ShortChannelID,
		a.Timestamp,
		a.Flags,
		a.TimeLockDelta,
		a.HtlcMinimumMsat,
		a.BaseFee,
		a.FeeRate,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) MsgType() MessageType {
	return MsgChannelUpdate
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// Signature - 64 bytes
	length += 64

	// ShortChannelID - 8 bytes
	length += 8

	// Timestamp - 4 bytes
	length += 4

	// Flags - 2 bytes
	length += 2

	// Expiry - 2 bytes
	length += 2

	// HtlcMinimumMstat - 8 bytes
	length += 8

	// FeeBaseMstat - 4 bytes
	length += 4

	// FeeProportionalMillionths - 4 bytes
	length += 4

	return length
}

// DataToSign is used to retrieve part of the announcement message which should
// be signed.
func (a *ChannelUpdate) DataToSign() ([]byte, error) {

	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		a.ShortChannelID,
		a.Timestamp,
		a.Flags,
		a.TimeLockDelta,
		a.HtlcMinimumMsat,
		a.BaseFee,
		a.FeeRate,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
