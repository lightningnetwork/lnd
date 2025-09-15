package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	defaultCltvExpiryDelta           = uint16(80)
	defaultHtlcMinMsat               = MilliSatoshi(1)
	defaultFeeBaseMsat               = uint32(1000)
	defaultFeeProportionalMillionths = uint32(1)
)

// ChannelUpdate2 message is used after taproot channel has been initially
// announced. Each side independently announces its fees and minimum expiry for
// HTLCs and other parameters. This message is also used to redeclare initially
// set channel parameters.
type ChannelUpdate2 struct {
	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	// Along with the short channel ID, this uniquely identifies the
	// channel globally in a blockchain.
	ChainHash tlv.RecordT[tlv.TlvType0, chainhash.Hash]

	// ShortChannelID is the unique description of the funding transaction.
	ShortChannelID tlv.RecordT[tlv.TlvType2, ShortChannelID]

	// BlockHeight allows ordering in the case of multiple announcements. We
	// should ignore the message if block height is not greater than the
	// last-received. The block height must always be greater or equal to
	// the block height that the channel funding transaction was confirmed
	// in.
	BlockHeight tlv.RecordT[tlv.TlvType4, uint32]

	// DisabledFlags is an optional bitfield that describes various reasons
	// that the node is communicating that the channel should be considered
	// disabled.
	DisabledFlags tlv.RecordT[tlv.TlvType6, ChanUpdateDisableFlags]

	// SecondPeer is used to indicate which node the channel node has
	// created and signed this message. If this field is present, it was
	// node 2 otherwise it was node 1.
	SecondPeer tlv.OptionalRecordT[tlv.TlvType8, TrueBoolean]

	// CLTVExpiryDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	CLTVExpiryDelta tlv.RecordT[tlv.TlvType10, uint16]

	// HTLCMinimumMsat is the minimum HTLC value which will be accepted.
	HTLCMinimumMsat tlv.RecordT[tlv.TlvType12, MilliSatoshi]

	// HtlcMaximumMsat is the maximum HTLC value which will be accepted.
	HTLCMaximumMsat tlv.RecordT[tlv.TlvType14, MilliSatoshi]

	// FeeBaseMsat is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment.
	FeeBaseMsat tlv.RecordT[tlv.TlvType16, uint32]

	// FeeProportionalMillionths is the fee rate that will be charged per
	// millionth of a satoshi.
	FeeProportionalMillionths tlv.RecordT[tlv.TlvType18, uint32]

	// InboundFee is an optional TLV record that contains the fee
	// information for incoming HTLCs.
	// TODO(elle): assign normal tlv type?
	InboundFee tlv.OptionalRecordT[tlv.TlvType55555, Fee]

	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature tlv.RecordT[tlv.TlvType160, Sig]

	// Any extra fields in the signed range that we do not yet know about,
	// but we need to keep them for signature validation and to produce a
	// valid message.
	ExtraSignedFields
}

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (c *ChannelUpdate2) GossipVersion() GossipVersion {
	return GossipVersion2
}

// Encode serializes the target ChannelUpdate2 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) Encode(w *bytes.Buffer, _ uint32) error {
	return EncodePureTLVMessage(c, w)
}

// Decode deserializes a serialized ChannelUpdate2 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) Decode(r io.Reader, _ uint32) error {
	// First extract into extra opaque data.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var (
		chainHash  = tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		secondPeer = tlv.ZeroRecordT[tlv.TlvType8, TrueBoolean]()
		inboundFee = tlv.ZeroRecordT[tlv.TlvType55555, Fee]()
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&chainHash, &c.ShortChannelID, &c.BlockHeight, &c.DisabledFlags,
		&secondPeer, &c.CLTVExpiryDelta, &c.HTLCMinimumMsat,
		&c.HTLCMaximumMsat, &c.FeeBaseMsat,
		&c.FeeProportionalMillionths, &inboundFee,
		&c.Signature,
	)
	if err != nil {
		return err
	}
	c.Signature.Val.ForceSchnorr()

	// By default, the chain-hash is the bitcoin mainnet genesis block hash.
	c.ChainHash.Val = *chaincfg.MainNetParams.GenesisHash
	if _, ok := typeMap[c.ChainHash.TlvType()]; ok {
		c.ChainHash.Val = chainHash.Val
	}

	// The presence of the second_peer tlv type indicates "true".
	if _, ok := typeMap[c.SecondPeer.TlvType()]; ok {
		c.SecondPeer = tlv.SomeRecordT(secondPeer)
	}

	// If the CLTV expiry delta was not encoded, then set it to the default
	// value.
	if _, ok := typeMap[c.CLTVExpiryDelta.TlvType()]; !ok {
		c.CLTVExpiryDelta.Val = defaultCltvExpiryDelta
	}

	// If the HTLC Minimum msat was not encoded, then set it to the default
	// value.
	if _, ok := typeMap[c.HTLCMinimumMsat.TlvType()]; !ok {
		c.HTLCMinimumMsat.Val = defaultHtlcMinMsat
	}

	// If the base fee was not encoded, then set it to the default value.
	if _, ok := typeMap[c.FeeBaseMsat.TlvType()]; !ok {
		c.FeeBaseMsat.Val = defaultFeeBaseMsat
	}

	// If the proportional fee was not encoded, then set it to the default
	// value.
	if _, ok := typeMap[c.FeeProportionalMillionths.TlvType()]; !ok {
		c.FeeProportionalMillionths.Val = defaultFeeProportionalMillionths //nolint:ll
	}

	// If the inbound fee was encoded, set it.
	if _, ok := typeMap[c.InboundFee.TlvType()]; ok {
		c.InboundFee = tlv.SomeRecordT(inboundFee)
	}

	c.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// AllRecords returns all the TLV records for the message. This will include all
// the records we know about along with any that we don't know about but that
// fall in the signed TLV range.
//
// NOTE: this is part of the PureTLVMessage interface.
func (c *ChannelUpdate2) AllRecords() []tlv.Record {
	var recordProducers []tlv.RecordProducer

	// The chain-hash record is only included if it is _not_ equal to the
	// bitcoin mainnet genisis block hash.
	if !c.ChainHash.Val.IsEqual(chaincfg.MainNetParams.GenesisHash) {
		hash := tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		hash.Val = c.ChainHash.Val

		recordProducers = append(recordProducers, &hash)
	}

	recordProducers = append(recordProducers,
		&c.ShortChannelID, &c.BlockHeight, &c.Signature,
	)

	// Only include the disable flags if any bit is set.
	if !c.DisabledFlags.Val.IsEnabled() {
		recordProducers = append(recordProducers, &c.DisabledFlags)
	}

	// We only need to encode the second peer boolean if it is true
	c.SecondPeer.WhenSome(func(r tlv.RecordT[tlv.TlvType8, TrueBoolean]) {
		recordProducers = append(recordProducers, &r)
	})

	// We only encode the cltv expiry delta if it is not equal to the
	// default.
	if c.CLTVExpiryDelta.Val != defaultCltvExpiryDelta {
		recordProducers = append(recordProducers, &c.CLTVExpiryDelta)
	}

	if c.HTLCMinimumMsat.Val != defaultHtlcMinMsat {
		recordProducers = append(recordProducers, &c.HTLCMinimumMsat)
	}

	recordProducers = append(recordProducers, &c.HTLCMaximumMsat)

	if c.FeeBaseMsat.Val != defaultFeeBaseMsat {
		recordProducers = append(recordProducers, &c.FeeBaseMsat)
	}

	if c.FeeProportionalMillionths.Val != defaultFeeProportionalMillionths {
		recordProducers = append(
			recordProducers, &c.FeeProportionalMillionths,
		)
	}

	c.InboundFee.WhenSome(func(r tlv.RecordT[tlv.TlvType55555, Fee]) {
		recordProducers = append(recordProducers, &r)
	})

	recordProducers = append(recordProducers, RecordsAsProducers(
		tlv.MapToRecords(c.ExtraSignedFields),
	)...)

	return ProduceRecordsSorted(recordProducers...)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdate2) MsgType() MessageType {
	return MsgChannelUpdate2
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ChannelUpdate2) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure ChannelUpdate2 implements the
// lnwire.Message interface.
var _ Message = (*ChannelUpdate2)(nil)

// A compile time check to ensure ChannelUpdate2 implements the
// lnwire.PureTLVMessage interface.
var _ PureTLVMessage = (*ChannelUpdate2)(nil)

// SCID returns the ShortChannelID of the channel that the update applies to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SCID() ShortChannelID {
	return c.ShortChannelID.Val
}

// IsNode1 is true if the update was produced by node 1 of the channel peers.
// Node 1 is the node with the lexicographically smaller public key.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsNode1() bool {
	return c.SecondPeer.IsNone()
}

// IsDisabled is true if the update is announcing that the channel should be
// considered disabled.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsDisabled() bool {
	return !c.DisabledFlags.Val.IsEnabled()
}

// GetChainHash returns the hash of the chain that the message is referring to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) GetChainHash() chainhash.Hash {
	return c.ChainHash.Val
}

// ForwardingPolicy returns the set of forwarding constraints of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) ForwardingPolicy() *ForwardingPolicy {
	return &ForwardingPolicy{
		TimeLockDelta: c.CLTVExpiryDelta.Val,
		BaseFee:       MilliSatoshi(c.FeeBaseMsat.Val),
		FeeRate:       MilliSatoshi(c.FeeProportionalMillionths.Val),
		MinHTLC:       c.HTLCMinimumMsat.Val,
		HasMaxHTLC:    true,
		MaxHTLC:       c.HTLCMaximumMsat.Val,
	}
}

// CmpAge can be used to determine if the update is older or newer than the
// passed update. It returns 1 if this update is newer, -1 if it is older, and
// 0 if they are the same age.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) CmpAge(update ChannelUpdate) (CompareResult, error) {
	other, ok := update.(*ChannelUpdate2)
	if !ok {
		return 0, fmt.Errorf("expected *ChannelUpdate2, got: %T",
			update)
	}

	switch {
	case c.BlockHeight.Val > other.BlockHeight.Val:
		return GreaterThan, nil
	case c.BlockHeight.Val < other.BlockHeight.Val:
		return LessThan, nil
	default:
		return EqualTo, nil
	}
}

// SetDisabledFlag can be used to adjust the disabled flag of an update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetDisabledFlag(disabled bool) {
	if disabled {
		c.DisabledFlags.Val |= ChanUpdateDisableIncoming
		c.DisabledFlags.Val |= ChanUpdateDisableOutgoing
	} else {
		c.DisabledFlags.Val &^= ChanUpdateDisableIncoming
		c.DisabledFlags.Val &^= ChanUpdateDisableOutgoing
	}
}

// SetSCID can be used to overwrite the SCID of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetSCID(scid ShortChannelID) {
	c.ShortChannelID.Val = scid
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

// Record returns the tlv record for the disable flags.
func (c *ChanUpdateDisableFlags) Record() tlv.Record {
	return tlv.MakeStaticRecord(0, c, 1, encodeDisableFlags,
		decodeDisableFlags)
}

func encodeDisableFlags(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*ChanUpdateDisableFlags); ok {
		flagsInt := uint8(*v)

		return tlv.EUint8(w, &flagsInt, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.ChanUpdateDisableFlags")
}

func decodeDisableFlags(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*ChanUpdateDisableFlags); ok {
		var flagsInt uint8
		err := tlv.DUint8(r, &flagsInt, buf, l)
		if err != nil {
			return err
		}

		*v = ChanUpdateDisableFlags(flagsInt)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.ChanUpdateDisableFlags",
		l, l)
}

// TrueBoolean is a record that indicates true or false using the presence of
// the record. If the record is absent, it indicates false. If it is present,
// it indicates true.
type TrueBoolean struct{}

// Record returns the tlv record for the boolean entry.
func (b *TrueBoolean) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, b, 0, booleanEncoder, booleanDecoder,
	)
}

func booleanEncoder(_ io.Writer, val interface{}, _ *[8]byte) error {
	if _, ok := val.(*TrueBoolean); ok {
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "TrueBoolean")
}

func booleanDecoder(_ io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if _, ok := val.(*TrueBoolean); ok && (l == 0 || l == 1) {
		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "TrueBoolean")
}
