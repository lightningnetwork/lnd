package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OnionPacketSize is the size of the serialized Sphinx onion packet
	// included in each UpdateAddHTLC message. The breakdown of the onion
	// packet is as follows: 1-byte version, 33-byte ephemeral public key
	// (for ECDH), 1300-bytes of per-hop data, and a 32-byte HMAC over the
	// entire packet.
	OnionPacketSize = 1366

	// ExperimentalEndorsementType is the TLV type used for a custom
	// record that sets an experimental endorsement value.
	ExperimentalEndorsementType tlv.Type = 106823

	// ExperimentalUnendorsed is the value that the experimental endorsement
	// field contains when a htlc is not endorsed.
	ExperimentalUnendorsed = 0

	// ExperimentalEndorsed is the value that the experimental endorsement
	// field contains when a htlc is endorsed. We're using a single byte
	// to represent our endorsement value, but limit the value to using
	// the first three bits (max value = 00000111). Interpreted as a uint8
	// (an alias for byte in go), we can just define this constant as 7.
	ExperimentalEndorsed = 7
)

type (
	// BlindingPointTlvType is the type for ephemeral pubkeys used in
	// route blinding.
	BlindingPointTlvType = tlv.TlvType0

	// BlindingPointRecord holds an optional blinding point on update add
	// htlc.
	//nolint:ll
	BlindingPointRecord = tlv.OptionalRecordT[BlindingPointTlvType, *btcec.PublicKey]
)

// UpdateAddHTLC is the message sent by Alice to Bob when she wishes to add an
// HTLC to his remote commitment transaction. In addition to information
// detailing the value, the ID, expiry, and the onion blob is also included
// which allows Bob to derive the next hop in the route. The HTLC added by this
// message is to be added to the remote node's "pending" HTLCs.  A subsequent
// CommitSig message will move the pending HTLC to the newly created commitment
// transaction, marking them as "staged".
type UpdateAddHTLC struct {
	// ChanID is the particular active channel that this UpdateAddHTLC is
	// bound to.
	ChanID ChannelID

	// ID is the identification server for this HTLC. This value is
	// explicitly included as it allows nodes to survive single-sided
	// restarts. The ID value for this sides starts at zero, and increases
	// with each offered HTLC.
	ID uint64

	// Amount is the amount of millisatoshis this HTLC is worth.
	Amount MilliSatoshi

	// PaymentHash is the payment hash to be included in the HTLC this
	// request creates. The pre-image to this HTLC must be revealed by the
	// upstream peer in order to fully settle the HTLC.
	PaymentHash [32]byte

	// Expiry is the number of blocks after which this HTLC should expire.
	// It is the receiver's duty to ensure that the outgoing HTLC has a
	// sufficient expiry value to allow her to redeem the incoming HTLC.
	Expiry uint32

	// OnionBlob is the raw serialized mix header used to route an HTLC in
	// a privacy-preserving manner. The mix header is defined currently to
	// be parsed as a 4-tuple: (groupElement, routingInfo, headerMAC,
	// body).  First the receiving node should use the groupElement, and
	// its current onion key to derive a shared secret with the source.
	// Once the shared secret has been derived, the headerMAC should be
	// checked FIRST. Note that the MAC only covers the routingInfo field.
	// If the MAC matches, and the shared secret is fresh, then the node
	// should strip off a layer of encryption, exposing the next hop to be
	// used in the subsequent UpdateAddHTLC message.
	OnionBlob [OnionPacketSize]byte

	// BlindingPoint is the ephemeral pubkey used to optionally blind the
	// next hop for this htlc.
	BlindingPoint BlindingPointRecord

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field of the UpdateAddHTLC
	// message.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewUpdateAddHTLC returns a new empty UpdateAddHTLC message.
func NewUpdateAddHTLC() *UpdateAddHTLC {
	return &UpdateAddHTLC{}
}

// A compile time check to ensure UpdateAddHTLC implements the lnwire.Message
// interface.
var _ Message = (*UpdateAddHTLC)(nil)

// Decode deserializes a serialized UpdateAddHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) Decode(r io.Reader, pver uint32) error {
	// msgExtraData is a temporary variable used to read the message extra
	// data field from the reader.
	var msgExtraData ExtraOpaqueData

	if err := ReadElements(r,
		&c.ChanID,
		&c.ID,
		&c.Amount,
		c.PaymentHash[:],
		&c.Expiry,
		c.OnionBlob[:],
		&msgExtraData,
	); err != nil {
		return err
	}

	// Extract TLV records from the extra data field.
	blindingRecord := c.BlindingPoint.Zero()

	customRecords, parsed, extraData, err := ParseAndExtractCustomRecords(
		msgExtraData, &blindingRecord,
	)
	if err != nil {
		return err
	}

	// Assign the parsed records back to the message.
	if parsed.Contains(blindingRecord.TlvType()) {
		c.BlindingPoint = tlv.SomeRecordT(blindingRecord)
	}

	c.CustomRecords = customRecords
	c.ExtraData = extraData

	return nil
}

// Encode serializes the target UpdateAddHTLC into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteUint64(w, c.ID); err != nil {
		return err
	}

	if err := WriteMilliSatoshi(w, c.Amount); err != nil {
		return err
	}

	if err := WriteBytes(w, c.PaymentHash[:]); err != nil {
		return err
	}

	if err := WriteUint32(w, c.Expiry); err != nil {
		return err
	}

	if err := WriteBytes(w, c.OnionBlob[:]); err != nil {
		return err
	}

	// Only include blinding point in extra data if present.
	var records []tlv.RecordProducer
	c.BlindingPoint.WhenSome(
		func(b tlv.RecordT[BlindingPointTlvType, *btcec.PublicKey]) {
			records = append(records, &b)
		},
	)

	extraData, err := MergeAndEncode(records, c.ExtraData, c.CustomRecords)
	if err != nil {
		return err
	}

	return WriteBytes(w, extraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *UpdateAddHTLC) MsgType() MessageType {
	return MsgUpdateAddHTLC
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *UpdateAddHTLC) TargetChanID() ChannelID {
	return c.ChanID
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *UpdateAddHTLC) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure UpdateAddHTLC implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*UpdateAddHTLC)(nil)
