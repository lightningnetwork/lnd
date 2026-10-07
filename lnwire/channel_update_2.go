package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	defaultCltvExpiryDelta                  = uint16(80)
	defaultHtlcMinMsat                      = MilliSatoshi(1)
	defaultFeeBaseMsat                      = uint32(1000)
	defaultFeeProportionalMillionths        = uint32(1)
	defaultInboundFeeBaseMsat               = uint32(0)
	defaultInboundFeeProportionalMillionths = uint32(0)
)

// ChannelUpdate2 message is used after taproot channel has been initially
// announced. Each side independently announces its fees and minimum expiry for
// HTLCs and other parameters. This message is also used to redeclare initially
// set channel parameters.
//
// Every field with a spec default is optional. A field is present exactly
// when the sender included it, even when it holds the default value, because
// the signature covers the records as sent. The default applies only when a
// caller reads an absent field.
type ChannelUpdate2 struct {
	// ChainHash denotes the target chain that this channel was opened
	// within. This value should be the genesis hash of the target chain.
	// Along with the short channel ID, this uniquely identifies the
	// channel globally in a blockchain. When absent, it is the bitcoin
	// mainnet genesis block hash.
	ChainHash tlv.OptionalRecordT[tlv.TlvType0, chainhash.Hash]

	// ShortChannelID identifies the channel and the side of it that sent
	// this update. It is BOLT 1's `sciddir_or_pubkey` type constrained to
	// the `sciddir` form: 9 wire bytes of `<dirbyte><scid>`, where the
	// direction byte is `0` for `node_id_1` and `1` for `node_id_2`. It is
	// the only field that says which side sent the update.
	ShortChannelID tlv.RecordT[tlv.TlvType2, SciddirIntro]

	// BlockHeight allows ordering in the case of multiple announcements. We
	// should ignore the message if block height is not greater than the
	// last-received. The block height must always be greater or equal to
	// the block height that the channel funding transaction was confirmed
	// in.
	BlockHeight tlv.RecordT[tlv.TlvType4, uint32]

	// DisabledFlags is an optional bitfield that describes various reasons
	// that the node is communicating that the channel should be considered
	// disabled. When absent, the channel is enabled.
	DisabledFlags tlv.OptionalRecordT[tlv.TlvType6, ChanUpdateDisableFlags]

	// CLTVExpiryDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node. When absent, it is 80.
	CLTVExpiryDelta tlv.OptionalRecordT[tlv.TlvType10, uint16]

	// HTLCMinimumMsat is the minimum HTLC value which will be accepted.
	// When absent, it is 1.
	HTLCMinimumMsat tlv.OptionalRecordT[tlv.TlvType12, MilliSatoshi]

	// HtlcMaximumMsat is the maximum HTLC value which will be accepted.
	HTLCMaximumMsat tlv.OptionalRecordT[tlv.TlvType14, MilliSatoshi]

	// FeeBaseMsat is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment. When absent,
	// it is 1000.
	FeeBaseMsat tlv.OptionalRecordT[tlv.TlvType16, uint32]

	// FeeProportionalMillionths is the fee rate that will be charged per
	// millionth of a satoshi. When absent, it is 1.
	FeeProportionalMillionths tlv.OptionalRecordT[tlv.TlvType18, uint32]

	// InboundFeeBaseMsat is the base fee (in millisatoshis) added by this
	// node for HTLCs forwarded *in* via this channel, regardless of which
	// channel they are forwarded out on. Default 0. Positive-only: this
	// version of gossip does not support negative inbound fees.
	InboundFeeBaseMsat tlv.OptionalRecordT[tlv.TlvType20, uint32]

	// InboundFeeProportionalMillionths is the proportional inbound fee (in
	// millionths of a satoshi) added by this node per transferred satoshi
	// for HTLCs forwarded *in* via this channel. Default 0. Positive-only.
	InboundFeeProportionalMillionths tlv.OptionalRecordT[
		tlv.TlvType22, uint32,
	]

	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature tlv.RecordT[tlv.TlvType240, Sig]

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
		chainHash     = tlv.ZeroRecordT[tlv.TlvType0, [32]byte]()
		disabledFlags = tlv.ZeroRecordT[
			tlv.TlvType6, ChanUpdateDisableFlags,
		]()
		cltvDelta   = tlv.ZeroRecordT[tlv.TlvType10, uint16]()
		htlcMin     = tlv.ZeroRecordT[tlv.TlvType12, MilliSatoshi]()
		htlcMax     = tlv.ZeroRecordT[tlv.TlvType14, MilliSatoshi]()
		feeBase     = tlv.ZeroRecordT[tlv.TlvType16, uint32]()
		feeRate     = tlv.ZeroRecordT[tlv.TlvType18, uint32]()
		inboundBase = tlv.ZeroRecordT[tlv.TlvType20, uint32]()
		inboundRate = tlv.ZeroRecordT[tlv.TlvType22, uint32]()
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&chainHash, sciddirRecord(&c.ShortChannelID), &c.BlockHeight,
		&disabledFlags, &cltvDelta, truncatedUint64Record(&htlcMin),
		truncatedUint64Record(&htlcMax),
		truncatedUint32Record(&feeBase),
		truncatedUint32Record(&feeRate),
		truncatedUint32Record(&inboundBase),
		truncatedUint32Record(&inboundRate), &c.Signature,
	)
	if err != nil {
		return err
	}
	c.Signature.Val.ForceSchnorr()

	if err := assertRequiredPresent(
		typeMap,
		c.ShortChannelID.TlvType(),
		c.BlockHeight.TlvType(),
		c.Signature.TlvType(),
	); err != nil {
		return err
	}

	if _, ok := typeMap[c.ChainHash.TlvType()]; ok {
		hash := c.ChainHash.Zero()
		hash.Val = chainHash.Val
		c.ChainHash = tlv.SomeRecordT(hash)
	}
	SetOptFromMap(typeMap, &c.DisabledFlags, disabledFlags)
	SetOptFromMap(typeMap, &c.CLTVExpiryDelta, cltvDelta)
	SetOptFromMap(typeMap, &c.HTLCMinimumMsat, htlcMin)
	SetOptFromMap(typeMap, &c.HTLCMaximumMsat, htlcMax)
	SetOptFromMap(typeMap, &c.FeeBaseMsat, feeBase)
	SetOptFromMap(typeMap, &c.FeeProportionalMillionths, feeRate)
	SetOptFromMap(typeMap, &c.InboundFeeBaseMsat, inboundBase)
	SetOptFromMap(typeMap, &c.InboundFeeProportionalMillionths, inboundRate)

	c.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// AllRecords returns all the TLV records for the message. This will include all
// the records we know about along with any that we don't know about but that
// fall in the signed TLV range.
//
// NOTE: this is part of the PureTLVMessage interface.
func (c *ChannelUpdate2) AllRecords() []tlv.Record {
	recordProducers := []tlv.RecordProducer{
		sciddirRecord(&c.ShortChannelID), &c.BlockHeight, &c.Signature,
	}

	// Each optional record is emitted exactly when it is present, so a
	// decoded message re-encodes to the bytes that the sender signed.
	c.ChainHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType0, chainhash.Hash]) {
			hash := tlv.NewPrimitiveRecord[tlv.TlvType0, [32]byte](
				r.Val,
			)
			recordProducers = append(recordProducers, &hash)
		},
	)
	AddOpt(&recordProducers, c.DisabledFlags)
	AddOpt(&recordProducers, c.CLTVExpiryDelta)
	addOptTU64(&recordProducers, c.HTLCMinimumMsat)
	addOptTU64(&recordProducers, c.HTLCMaximumMsat)
	addOptTU32(&recordProducers, c.FeeBaseMsat)
	addOptTU32(&recordProducers, c.FeeProportionalMillionths)
	addOptTU32(&recordProducers, c.InboundFeeBaseMsat)
	addOptTU32(&recordProducers, c.InboundFeeProportionalMillionths)

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

// SCID returns the ShortChannelID of the channel that the update applies to,
// projecting away the direction byte that the wire encoding carries.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SCID() ShortChannelID {
	return c.ShortChannelID.Val.ShortChannelID()
}

// IsNode1 is true if the update was produced by node 1 of the channel peers.
// Node 1 is the node with the lexicographically smaller public key, and is
// indicated by a direction byte of 0 in the encoded ShortChannelID.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsNode1() bool {
	return c.ShortChannelID.Val.Direction == 0
}

// IsDisabled is true if the update is announcing that the channel should be
// considered disabled.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) IsDisabled() bool {
	return !c.DisableFlags().IsEnabled()
}

// GetChainHash returns the hash of the chain that the message is referring to.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) GetChainHash() chainhash.Hash {
	return c.ChainHash.ValOpt().UnwrapOr(
		*chaincfg.MainNetParams.GenesisHash,
	)
}

// ForwardingPolicy returns the set of forwarding constraints of the update.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) ForwardingPolicy() *ForwardingPolicy {
	maxHTLC := c.HTLCMaximumMsat.ValOpt()

	return &ForwardingPolicy{
		TimeLockDelta: c.CLTVExpiryDelta.ValOpt().UnwrapOr(
			defaultCltvExpiryDelta,
		),
		BaseFee: MilliSatoshi(c.FeeBaseMsat.ValOpt().UnwrapOr(
			defaultFeeBaseMsat,
		)),
		FeeRate: MilliSatoshi(
			c.FeeProportionalMillionths.ValOpt().UnwrapOr(
				defaultFeeProportionalMillionths,
			),
		),
		MinHTLC: c.HTLCMinimumMsat.ValOpt().UnwrapOr(
			defaultHtlcMinMsat,
		),
		HasMaxHTLC: maxHTLC.IsSome(),
		MaxHTLC:    maxHTLC.UnwrapOr(0),
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
	flags := c.DisableFlags()
	if disabled {
		flags |= ChanUpdateDisableIncoming
		flags |= ChanUpdateDisableOutgoing
	} else {
		flags &^= ChanUpdateDisableIncoming
		flags &^= ChanUpdateDisableOutgoing
	}

	// The update is re-signed after this change, so the flags follow the
	// writer rule and are omitted when they take the default value.
	c.DisabledFlags = tlv.OptionalRecordT[
		tlv.TlvType6, ChanUpdateDisableFlags,
	]{}
	if !flags.IsEnabled() {
		c.DisabledFlags = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType6](flags),
		)
	}
}

// DisableFlags returns the disable flags of the update, or the enabled default
// when the update does not carry them.
func (c *ChannelUpdate2) DisableFlags() ChanUpdateDisableFlags {
	return c.DisabledFlags.ValOpt().UnwrapOr(0)
}

// InboundFee returns the inbound base fee and the inbound fee rate of the
// update, with the default of 0 for each fee that the update does not carry.
func (c *ChannelUpdate2) InboundFee() (uint32, uint32) {
	base := c.InboundFeeBaseMsat.ValOpt().UnwrapOr(
		defaultInboundFeeBaseMsat,
	)
	rate := c.InboundFeeProportionalMillionths.ValOpt().UnwrapOr(
		defaultInboundFeeProportionalMillionths,
	)

	return base, rate
}

// SetSCID can be used to overwrite the SCID of the update, leaving the
// existing direction byte in place.
//
// NOTE: this is part of the ChannelUpdate interface.
func (c *ChannelUpdate2) SetSCID(scid ShortChannelID) {
	binary.BigEndian.PutUint64(
		c.ShortChannelID.Val.SCID[:], scid.ToUint64(),
	)
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

// String returns a human-readable representation of the disable flags.
func (c ChanUpdateDisableFlags) String() string {
	if c.IsEnabled() {
		return "Enabled"
	}

	incoming := c.IncomingDisabled()
	outgoing := c.OutgoingDisabled()

	switch {
	case incoming && outgoing:
		return "Disabled(incoming&outgoing)"
	case incoming:
		return "Disabled(incoming)"
	default:
		return "Disabled(outgoing)"
	}
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
