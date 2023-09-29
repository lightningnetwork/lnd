package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ChanUpdate2ChainHashType is the tlv number associated with the chain
	// hash TLV record in the channel_update_2 message.
	ChanUpdate2ChainHashType = tlv.Type(0)

	// ChanUpdate2SCIDType is the tlv number associated with the SCID TLV
	// record in the channel_update_2 message.
	ChanUpdate2SCIDType = tlv.Type(2)

	// ChanUpdate2BlockHeightType is the tlv number associated with the
	// block height record in the channel_update_2 message.
	ChanUpdate2BlockHeightType = tlv.Type(4)

	// ChanUpdate2DisableFlagsType is the tlv number associated with the
	// disable flags record in the channel_update_2 message.
	ChanUpdate2DisableFlagsType = tlv.Type(6)

	// ChanUpdate2DirectionType is the tlv number associated with the
	// disable boolean TLV record in the channel_update_2 message.
	ChanUpdate2DirectionType = tlv.Type(8)

	// ChanUpdate2CLTVExpiryDeltaType is the tlv number associated with the
	// CLTV expiry delta TLV record in the channel_update_2 message.
	ChanUpdate2CLTVExpiryDeltaType = tlv.Type(10)

	// ChanUpdate2HTLCMinMsatType is the tlv number associated with the htlc
	// minimum msat record in the channel_update_2 message.
	ChanUpdate2HTLCMinMsatType = tlv.Type(12)

	// ChanUpdate2HTLCMaxMsatType is the tlv number associated with the htlc
	// maximum msat record in the channel_update_2 message.
	ChanUpdate2HTLCMaxMsatType = tlv.Type(14)

	// ChanUpdate2FeeBaseMsatType is the tlv number associated with the fee
	// base msat record in the channel_update_2 message.
	ChanUpdate2FeeBaseMsatType = tlv.Type(16)

	// ChanUpdate2FeeProportionalMillionthsType is the tlv number associated
	// with the fee proportional millionths record in the channel_update_2
	// message.
	ChanUpdate2FeeProportionalMillionthsType = tlv.Type(18)

	defaultCltvExpiryDelta           = uint16(80)
	defaultHtlcMinMsat               = MilliSatoshi(1)
	defaultFeeBaseMsat               = uint32(1000)
	defaultFeeProportionalMillionths = uint32(1)
)

// ChannelUpdate2 message is used after taproot channel has been initially
// announced. Each side independently announces its fees and minimum expiry for
// HTLCs and other parameters. Also this message is used to redeclare initially
// set channel parameters.
type ChannelUpdate2 struct {
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

	// BlockHeight allows ordering in the case of multiple announcements. We
	// should ignore the message if block height is not greater than the
	// last-received. The block height must always be greater or equal to
	// the block height that the channel funding transaction was confirmed
	// in.
	BlockHeight uint32

	// DisabledFlags is an optional bitfield that describes various reasons
	// that the node is communicating that the channel should be considered
	// disabled.
	DisabledFlags ChanUpdateDisableFlags

	// Direction is false if this update was produced by node 1 of the
	// channel announcement and true if it is from node 2.
	Direction bool

	// CLTVExpiryDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	CLTVExpiryDelta uint16

	// HTLCMinimumMsat is the minimum HTLC value which will be accepted.
	HTLCMinimumMsat MilliSatoshi

	// HtlcMaximumMsat is the maximum HTLC value which will be accepted.
	HTLCMaximumMsat MilliSatoshi

	// FeeBaseMsat is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment.
	FeeBaseMsat uint32

	// FeeProportionalMillionths is the fee rate that will be charged per
	// millionth of a satoshi.
	FeeProportionalMillionths uint32

	// ExtraOpaqueData is the set of data that was appended to this message
	// to fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraOpaqueData ExtraOpaqueData
}

// Decode deserializes a serialized ChannelUpdate2 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) Decode(r io.Reader, _ uint32) error {
	err := ReadElement(r, &c.Signature)
	if err != nil {
		return err
	}
	c.Signature.ForceSchnorr()

	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	scidRecordProducer := NewShortChannelIDRecordProducer(
		ChanUpdate2SCIDType,
	)

	directionRecordProducer := NewBooleanRecordProducer(
		ChanUpdate2DirectionType,
	)

	var (
		chainHash        [32]byte
		htlcMin, htlcMax uint64
		disableFlags     uint8
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(ChanUpdate2ChainHashType, &chainHash),
		scidRecordProducer.Record(),
		tlv.MakePrimitiveRecord(
			ChanUpdate2BlockHeightType, &c.BlockHeight,
		),
		tlv.MakePrimitiveRecord(
			ChanUpdate2DisableFlagsType, &disableFlags,
		),
		directionRecordProducer.Record(),
		tlv.MakePrimitiveRecord(
			ChanUpdate2CLTVExpiryDeltaType, &c.CLTVExpiryDelta,
		),
		tlv.MakePrimitiveRecord(
			ChanUpdate2HTLCMinMsatType, &htlcMin,
		),
		tlv.MakePrimitiveRecord(
			ChanUpdate2HTLCMaxMsatType, &htlcMax,
		),
		tlv.MakePrimitiveRecord(
			ChanUpdate2FeeBaseMsatType, &c.FeeBaseMsat,
		),
		tlv.MakePrimitiveRecord(
			ChanUpdate2FeeProportionalMillionthsType,
			&c.FeeProportionalMillionths,
		),
	}

	typeMap, err := tlvRecords.ExtractRecords(records...)
	if err != nil {
		return err
	}

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash = *chaincfg.MainNetParams.GenesisHash
	if _, ok := typeMap[ChanUpdate2ChainHashType]; ok {
		c.ChainHash = chainHash
	}

	if _, ok := typeMap[ChanUpdate2DisableFlagsType]; ok {
		c.DisabledFlags = ChanUpdateDisableFlags(disableFlags)
	}

	if _, ok := typeMap[ChanUpdate2SCIDType]; ok {
		c.ShortChannelID = scidRecordProducer.ShortChannelID
	}

	if _, ok := typeMap[ChanUpdate2DirectionType]; ok {
		c.Direction = directionRecordProducer.Bool
	}

	// If the CLTV expiry delta was not encoded, then set it to the default
	// value.
	if _, ok := typeMap[ChanUpdate2CLTVExpiryDeltaType]; !ok {
		c.CLTVExpiryDelta = defaultCltvExpiryDelta
	}

	c.HTLCMinimumMsat = defaultHtlcMinMsat
	if _, ok := typeMap[ChanUpdate2HTLCMinMsatType]; ok {
		c.HTLCMinimumMsat = MilliSatoshi(htlcMin)
	}

	if _, ok := typeMap[ChanUpdate2HTLCMaxMsatType]; ok {
		c.HTLCMaximumMsat = MilliSatoshi(htlcMax)
	}

	// If the base fee was not encoded, then set it to the default value.
	if _, ok := typeMap[ChanUpdate2FeeBaseMsatType]; !ok {
		c.FeeBaseMsat = defaultFeeBaseMsat
	}

	// If the proportional fee was not encoded, then set it to the default
	// value.
	if _, ok := typeMap[ChanUpdate2FeeProportionalMillionthsType]; !ok {
		c.FeeProportionalMillionths = defaultFeeProportionalMillionths
	}

	if len(tlvRecords) != 0 {
		c.ExtraOpaqueData = tlvRecords
	}

	return nil
}

// Encode serializes the target ChannelUpdate2 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) Encode(w *bytes.Buffer, _ uint32) error {
	_, err := w.Write(c.Signature.RawBytes())
	if err != nil {
		return err
	}

	_, err = c.DataToSign()
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraOpaqueData)
}

// DigestTag returns the tag to be used when signing the digest.
func (c *ChannelUpdate2) DigestTag() []byte {
	return MsgTag("channel_announcement_2", "announcement_signature")
}

// DigestToSign computes the digest of the message to be signed.
func (c *ChannelUpdate2) DigestToSign() ([]byte, error) {
	data, err := c.DataToSign()
	if err != nil {
		return nil, err
	}

	hash := MsgHash(
		"channel_announcement_2", "announcement_signature", data,
	)

	return hash[:], nil
}

// DataToSign is used to retrieve part of the announcement message which should
// be signed. For the ChannelUpdate2 message, this includes the serialised TLV
// records.
func (c *ChannelUpdate2) DataToSign() ([]byte, error) {
	// The chain-hash record is only included if it is _not_ equal to the
	// bitcoin mainnet genisis block hash.
	var records []tlv.Record
	if !c.ChainHash.IsEqual(chaincfg.MainNetParams.GenesisHash) {
		chainHash := [32]byte(c.ChainHash)
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2ChainHashType, &chainHash,
		))
	}

	scidRecordProducer := &ShortChannelIDRecordProducer{
		ShortChannelID: c.ShortChannelID,
		Type:           ChanUpdate2SCIDType,
	}

	records = append(records,
		scidRecordProducer.Record(),
		tlv.MakePrimitiveRecord(
			ChanUpdate2BlockHeightType, &c.BlockHeight,
		),
	)

	// Only include the disable flags if any bit is set.
	if !c.DisabledFlags.IsEnabled() {
		disableFlags := uint8(c.DisabledFlags)
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2DisableFlagsType, &disableFlags,
		))
	}

	// We only need to encode the direction if the direction is set to 1.
	if c.Direction {
		directionRecordProducer := &BooleanRecordProducer{
			Bool: true,
			Type: ChanUpdate2DirectionType,
		}
		records = append(records, directionRecordProducer.Record())
	}

	// We only encode the cltv expiry delta if it is not equal to the
	// default.
	if c.CLTVExpiryDelta != defaultCltvExpiryDelta {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2CLTVExpiryDeltaType, &c.CLTVExpiryDelta,
		))
	}

	if c.HTLCMinimumMsat != defaultHtlcMinMsat {
		var htlcMin = uint64(c.HTLCMinimumMsat)
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2HTLCMinMsatType, &htlcMin,
		))
	}

	var htlcMax = uint64(c.HTLCMaximumMsat)
	records = append(records, tlv.MakePrimitiveRecord(
		ChanUpdate2HTLCMaxMsatType, &htlcMax,
	))

	if c.FeeBaseMsat != defaultFeeBaseMsat {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2FeeBaseMsatType, &c.FeeBaseMsat,
		))
	}

	if c.FeeProportionalMillionths != defaultFeeProportionalMillionths {
		records = append(records, tlv.MakePrimitiveRecord(
			ChanUpdate2FeeProportionalMillionthsType,
			&c.FeeProportionalMillionths,
		))
	}

	err := EncodeMessageExtraDataFromRecords(&c.ExtraOpaqueData, records...)
	if err != nil {
		return nil, err
	}

	return c.ExtraOpaqueData, nil
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) MsgType() MessageType {
	return MsgChannelUpdate2
}

// A compile time check to ensure ChannelUpdate2 implements the
// lnwire.Message interface.
var _ Message = (*ChannelUpdate2)(nil)

// SCID returns the ShortChannelID of the channel that the update applies to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SCID() ShortChannelID {
	return c.ShortChannelID
}

// IsNode1 is true if the update was produced by node 1 of the channel peers.
// Node 1 is the node with the lexicographically smaller public key.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsNode1() bool {
	return !c.Direction
}

// IsDisabled is true if the update is announcing that the channel should be
// considered disabled.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsDisabled() bool {
	return !c.DisabledFlags.IsEnabled()
}

// GetChainHash returns the hash of the chain that the message is referring to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) GetChainHash() chainhash.Hash {
	return c.ChainHash
}

// ForwardingPolicy returns the set of forwarding constraints of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) ForwardingPolicy() *ForwardingPolicy {
	return &ForwardingPolicy{
		TimeLockDelta: c.CLTVExpiryDelta,
		BaseFee:       MilliSatoshi(c.FeeBaseMsat),
		FeeRate:       MilliSatoshi(c.FeeProportionalMillionths),
		MinHTLC:       c.HTLCMinimumMsat,
		HasMaxHTLC:    true,
		MaxHTLC:       c.HTLCMaximumMsat,
	}
}

// CmpAge can be used to determine if the update is older or newer than the
// passed update. It returns 1 if this update is newer, -1 if it is older, and
// 0 if they are the same age.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) CmpAge(update ChannelUpdate) (int, error) {
	other, ok := update.(*ChannelUpdate2)
	if !ok {
		return 0, fmt.Errorf("expected *ChannelUpdate2, got: %T",
			update)
	}

	switch {
	case c.BlockHeight > other.BlockHeight:
		return 1, nil
	case c.BlockHeight < other.BlockHeight:
		return -1, nil
	default:
		return 0, nil
	}
}

// SetDisabled can be used to adjust the disabled flag of an update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetDisabled(disabled bool) {
	if disabled {
		c.DisabledFlags |= ChanUpdateDisableIncoming
		c.DisabledFlags |= ChanUpdateDisableOutgoing
	} else {
		c.DisabledFlags &^= ChanUpdateDisableIncoming
		c.DisabledFlags &^= ChanUpdateDisableOutgoing
	}
}

// SetSig can be used to adjust the signature of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetSig(signature input.Signature) error {
	sig, err := NewSigFromSignature(signature)
	if err != nil {
		return err
	}

	c.Signature = sig

	return nil
}

// SetSCID can be used to overwrite the SCID of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetSCID(scid ShortChannelID) {
	c.ShortChannelID = scid
}

// A compile time check to ensure ChannelUpdate2 implements the
// lnwire.ChannelUpdate interface.
var _ ChannelUpdate = (*ChannelUpdate2)(nil)

// ChanUpdateDisableFlags is a bit vector that can be used to indicate various
// reasons for the channel being marked as disabled.
type ChanUpdateDisableFlags uint8

const (
	// ChanUpdateDisableIncoming is a bit indicates that a channel is
	// disabled in the inbound direction meaning that the node broadcasting
	// the update is communicating that they cannot receive funds.
	ChanUpdateDisableIncoming ChanUpdateDisableFlags = 1 << iota

	// ChanUpdateDisableOutgoing is a bit indicates that a channel is
	// disabled in the outbound direction meaning that the node broadcasting
	// the update is communicating that they cannot send or route funds.
	ChanUpdateDisableOutgoing = 2
)

// IncomingDisabled returns true if the ChanUpdateDisableIncoming bit is set.
func (c ChanUpdateDisableFlags) IncomingDisabled() bool {
	return c&ChanUpdateDisableIncoming == ChanUpdateDisableIncoming
}

// OutgoingDisabled returns true if the ChanUpdateDisableOutgoing bit is set.
func (c ChanUpdateDisableFlags) OutgoingDisabled() bool {
	return c&ChanUpdateDisableOutgoing == ChanUpdateDisableOutgoing
}

// IsEnabled returns true if none of the disable bits are set.
func (c ChanUpdateDisableFlags) IsEnabled() bool {
	return c == 0
}

// String returns the bitfield flags as a string.
func (c ChanUpdateDisableFlags) String() string {
	return fmt.Sprintf("%08b", c)
}
