package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// ChanUpdateMsgFlags is a bitfield that signals whether optional fields are
// present in the ChannelUpdate.
type ChanUpdateMsgFlags uint8

const (
	// ChanUpdateRequiredMaxHtlc is a bit that indicates whether the
	// required htlc_maximum_msat field is present in this ChannelUpdate.
	ChanUpdateRequiredMaxHtlc ChanUpdateMsgFlags = 1 << iota
)

// String returns the bitfield flags as a string.
func (c ChanUpdateMsgFlags) String() string {
	return fmt.Sprintf("%08b", c)
}

// HasMaxHtlc returns true if the htlc_maximum_msat option bit is set in the
// message flags.
func (c ChanUpdateMsgFlags) HasMaxHtlc() bool {
	return c&ChanUpdateRequiredMaxHtlc != 0
}

// ChanUpdateChanFlags is a bitfield that signals various options concerning a
// particular channel edge. Each bit is to be examined in order to determine
// how the ChannelUpdate message is to be interpreted.
type ChanUpdateChanFlags uint8

const (
	// ChanUpdateDirection indicates the direction of a channel update. If
	// this bit is set to 0 if Node1 (the node with the "smaller" Node ID)
	// is updating the channel, and to 1 otherwise.
	ChanUpdateDirection ChanUpdateChanFlags = 1 << iota

	// ChanUpdateDisabled is a bit that indicates if the channel edge
	// selected by the ChanUpdateDirection bit is to be treated as being
	// disabled.
	ChanUpdateDisabled
)

// IsDisabled determines whether the channel flags has the disabled bit set.
func (c ChanUpdateChanFlags) IsDisabled() bool {
	return c&ChanUpdateDisabled == ChanUpdateDisabled
}

// String returns the bitfield flags as a string.
func (c ChanUpdateChanFlags) String() string {
	return fmt.Sprintf("%08b", c)
}

// ChannelUpdate1 message is used after channel has been initially announced.
// Each side independently announces its fees and minimum expiry for HTLCs and
// other parameters. Also this message is used to redeclare initially set
// channel parameters.
type ChannelUpdate1 struct {
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

	// Timestamp allows ordering in the case of multiple announcements. We
	// should ignore the message if timestamp is not greater than
	// the last-received.
	Timestamp uint32

	// MessageFlags is a bitfield that describes whether optional fields
	// are present in this update. Currently, the least-significant bit
	// must be set to 1 if the optional field MaxHtlc is present.
	MessageFlags ChanUpdateMsgFlags

	// ChannelFlags is a bitfield that describes additional meta-data
	// concerning how the update is to be interpreted. Currently, the
	// least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise. If the second bit is set, then the
	// channel is set to be disabled.
	ChannelFlags ChanUpdateChanFlags

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

	// HtlcMaximumMsat is the maximum HTLC value which will be accepted.
	HtlcMaximumMsat MilliSatoshi

	// InboundFee is an optional TLV record that contains the fee
	// information for incoming HTLCs.
	InboundFee tlv.OptionalRecordT[tlv.TlvType55555, Fee]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraOpaqueData ExtraOpaqueData
}

// A compile time check to ensure ChannelUpdate implements the lnwire.Message
// interface.
var _ Message = (*ChannelUpdate1)(nil)

// A compile time check to ensure ChannelUpdate1 implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ChannelUpdate1)(nil)

// Decode deserializes a serialized ChannelUpdate stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate1) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(r,
		&a.Signature,
		a.ChainHash[:],
		&a.ShortChannelID,
		&a.Timestamp,
		&a.MessageFlags,
		&a.ChannelFlags,
		&a.TimeLockDelta,
		&a.HtlcMinimumMsat,
		&a.BaseFee,
		&a.FeeRate,
	)
	if err != nil {
		return err
	}

	// Now check whether the max HTLC field is present and read it if so.
	if a.MessageFlags.HasMaxHtlc() {
		if err := ReadElements(r, &a.HtlcMaximumMsat); err != nil {
			return err
		}
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var inboundFee = a.InboundFee.Zero()
	typeMap, err := tlvRecords.ExtractRecords(&inboundFee)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrParsingExtraTLVBytes, err)
	}

	val, ok := typeMap[a.InboundFee.TlvType()]
	if ok && val == nil {
		a.InboundFee = tlv.SomeRecordT(inboundFee)
	}

	if len(tlvRecords) != 0 {
		a.ExtraOpaqueData = tlvRecords
	}

	return nil
}

// Encode serializes the target ChannelUpdate into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate1) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteSig(w, a.Signature); err != nil {
		return err
	}

	if err := WriteBytes(w, a.ChainHash[:]); err != nil {
		return err
	}

	if err := WriteShortChannelID(w, a.ShortChannelID); err != nil {
		return err
	}

	if err := WriteUint32(w, a.Timestamp); err != nil {
		return err
	}

	if err := WriteChanUpdateMsgFlags(w, a.MessageFlags); err != nil {
		return err
	}

	if err := WriteChanUpdateChanFlags(w, a.ChannelFlags); err != nil {
		return err
	}

	if err := WriteUint16(w, a.TimeLockDelta); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, a.HtlcMinimumMsat); err != nil {
		return err
	}

	if err := WriteUint32(w, a.BaseFee); err != nil {
		return err
	}

	if err := WriteUint32(w, a.FeeRate); err != nil {
		return err
	}

	// Now append optional fields if they are set. Currently, the only
	// optional field is max HTLC.
	if a.MessageFlags.HasMaxHtlc() {
		err := WriteMilliSatoshi(w, a.HtlcMaximumMsat)
		if err != nil {
			return err
		}
	}

	recordProducers := make([]tlv.RecordProducer, 0, 1)
	a.InboundFee.WhenSome(func(fee tlv.RecordT[tlv.TlvType55555, Fee]) {
		recordProducers = append(recordProducers, &fee)
	})

	err := EncodeMessageExtraData(&a.ExtraOpaqueData, recordProducers...)
	if err != nil {
		return err
	}

	// Finally, append any extra opaque data.
	return WriteBytes(w, a.ExtraOpaqueData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdate1) MsgType() MessageType {
	return MsgChannelUpdate
}

// DataToSign is used to retrieve part of the announcement message which should
// be signed.
func (a *ChannelUpdate1) DataToSign() ([]byte, error) {
	// We should not include the signatures itself.
	b := make([]byte, 0, MaxMsgBody)
	buf := bytes.NewBuffer(b)
	if err := WriteBytes(buf, a.ChainHash[:]); err != nil {
		return nil, err
	}

	if err := WriteShortChannelID(buf, a.ShortChannelID); err != nil {
		return nil, err
	}

	if err := WriteUint32(buf, a.Timestamp); err != nil {
		return nil, err
	}

	if err := WriteChanUpdateMsgFlags(buf, a.MessageFlags); err != nil {
		return nil, err
	}

	if err := WriteChanUpdateChanFlags(buf, a.ChannelFlags); err != nil {
		return nil, err
	}

	if err := WriteUint16(buf, a.TimeLockDelta); err != nil {
		return nil, err
	}

	if err := WriteMilliSatoshi(buf, a.HtlcMinimumMsat); err != nil {
		return nil, err
	}

	if err := WriteUint32(buf, a.BaseFee); err != nil {
		return nil, err
	}

	if err := WriteUint32(buf, a.FeeRate); err != nil {
		return nil, err
	}

	// Now append optional fields if they are set. Currently, the only
	// optional field is max HTLC.
	if a.MessageFlags.HasMaxHtlc() {
		err := WriteMilliSatoshi(buf, a.HtlcMaximumMsat)
		if err != nil {
			return nil, err
		}
	}

	// Finally, append any extra opaque data.
	if err := WriteBytes(buf, a.ExtraOpaqueData); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// SCID returns the ShortChannelID of the channel that the update applies to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) SCID() ShortChannelID {
	return a.ShortChannelID
}

// IsNode1 is true if the update was produced by node 1 of the channel peers.
// Node 1 is the node with the lexicographically smaller public key.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) IsNode1() bool {
	return a.ChannelFlags&ChanUpdateDirection == 0
}

// IsDisabled is true if the update is announcing that the channel should be
// considered disabled.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) IsDisabled() bool {
	return a.ChannelFlags&ChanUpdateDisabled == ChanUpdateDisabled
}

// GetChainHash returns the hash of the chain that the message is referring to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) GetChainHash() chainhash.Hash {
	return a.ChainHash
}

// ForwardingPolicy returns the set of forwarding constraints of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) ForwardingPolicy() *ForwardingPolicy {
	return &ForwardingPolicy{
		TimeLockDelta: a.TimeLockDelta,
		BaseFee:       MilliSatoshi(a.BaseFee),
		FeeRate:       MilliSatoshi(a.FeeRate),
		MinHTLC:       a.HtlcMinimumMsat,
		HasMaxHTLC:    a.MessageFlags.HasMaxHtlc(),
		MaxHTLC:       a.HtlcMaximumMsat,
	}
}

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (a *ChannelUpdate1) GossipVersion() GossipVersion {
	return GossipVersion1
}

// CmpAge can be used to determine if the update is older or newer than the
// passed update. It returns 1 if this update is newer, -1 if it is older, and
// 0 if they are the same age.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) CmpAge(update ChannelUpdate) (CompareResult, error) {
	other, ok := update.(*ChannelUpdate1)
	if !ok {
		return 0, fmt.Errorf("expected *ChannelUpdate1, got: %T",
			update)
	}

	switch {
	case a.Timestamp > other.Timestamp:
		return GreaterThan, nil
	case a.Timestamp < other.Timestamp:
		return LessThan, nil
	default:
		return EqualTo, nil
	}
}

// SetDisabledFlag can be used to adjust the disabled flag of an update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) SetDisabledFlag(disabled bool) {
	if disabled {
		a.ChannelFlags |= ChanUpdateDisabled
	} else {
		a.ChannelFlags &= ^ChanUpdateDisabled
	}
}

// SetSCID can be used to overwrite the SCID of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (a *ChannelUpdate1) SetSCID(scid ShortChannelID) {
	a.ShortChannelID = scid
}

// A compile time assertion to ensure ChannelUpdate1 implements the
// ChannelUpdate interface.
var _ ChannelUpdate = (*ChannelUpdate1)(nil)

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (a *ChannelUpdate1) SerializedSize() (uint32, error) {
	return MessageSerializedSize(a)
}
