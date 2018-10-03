package lnwire

import (
	"bytes"
	"io"
	"io/ioutil"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// ChanUpdateFlag is a bitfield that signals various options concerning a
// particular channel edge. Each bit is to be examined in order to determine
// how the ChannelUpdate message is to be interpreted.
type ChanUpdateFlag uint16

const (
	// ChanUpdateDirection indicates the direction of a channel update. If
	// this bit is set to 0 if Node1 (the node with the "smaller" Node ID)
	// is updating the channel, and to 1 otherwise.
	ChanUpdateDirection ChanUpdateFlag = 1 << iota

	// ChanUpdateDisabled is a bit that indicates if the channel edge
	// selected by the ChanUpdateDirection bit is to be treated as being
	// disabled.
	ChanUpdateDisabled
)

// ChannelUpdate message is used after channel has been initially announced.
// Each side independently announces its fees and minimum expiry for HTLCs and
// other parameters. Also this message is used to redeclare initially set
// channel parameters.
type ChannelUpdate struct {
	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature Sig

	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	// Along with the short channel ID, this uniquely identifies the
	// channel globally in a blockchain.
	ChainHash chainhash.Hash

	// ShortChannelID is the unique description of the funding transaction.
	ShortChannelID ShortChannelID

	// Timestamp allows ordering in the case of multiple announcements.  We
	// should ignore the message if timestamp is not greater than
	// the last-received.
	Timestamp uint32

	// Flags is a bitfield that describes additional meta-data concerning
	// how the update is to be interpreted. Currently, the
	// least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise. If the second bit is set, then the
	// channel is set to be disabled.
	Flags ChanUpdateFlag

	// TimeLockDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	TimeLockDelta uint16

	// HtlcMinimumMsat is the minimum HTLC value which will be accepted.
	HtlcMinimumMsat MilliSatoshi

	// BaseFee is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment.
	BaseFee uint32

	// FeeRate is the fee rate that will be charged per millionth of a
	// satoshi.
	FeeRate uint32

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte
}

// A compile time check to ensure ChannelUpdate implements the lnwire.Message
// interface.
var _ Message = (*ChannelUpdate)(nil)

// Decode deserializes a serialized ChannelUpdate stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) Decode(r io.Reader, pver uint32) error {
	err := readElements(r,
		&a.Signature,
		a.ChainHash[:],
		&a.ShortChannelID,
		&a.Timestamp,
		&a.Flags,
		&a.TimeLockDelta,
		&a.HtlcMinimumMsat,
		&a.BaseFee,
		&a.FeeRate,
	)
	if err != nil {
		return err
	}

	// Now that we've read out all the fields that we explicitly know of,
	// we'll collect the remainder into the ExtraOpaqueData field. If there
	// aren't any bytes, then we'll snip off the slice to avoid carrying
	// around excess capacity.
	a.ExtraOpaqueData, err = ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	if len(a.ExtraOpaqueData) == 0 {
		a.ExtraOpaqueData = nil
	}

	return nil
}

// Encode serializes the target ChannelUpdate into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		a.Signature,
		a.ChainHash[:],
		a.ShortChannelID,
		a.Timestamp,
		a.Flags,
		a.TimeLockDelta,
		a.HtlcMinimumMsat,
		a.BaseFee,
		a.FeeRate,
		a.ExtraOpaqueData,
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
	return 65533
}

// DataToSign is used to retrieve part of the announcement message which should
// be signed.
func (a *ChannelUpdate) DataToSign() ([]byte, error) {

	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		a.ChainHash[:],
		a.ShortChannelID,
		a.Timestamp,
		a.Flags,
		a.TimeLockDelta,
		a.HtlcMinimumMsat,
		a.BaseFee,
		a.FeeRate,
		a.ExtraOpaqueData,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
