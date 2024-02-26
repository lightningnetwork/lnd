package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// OnionPacketSize is the size of the serialized Sphinx onion packet included
// in each UpdateAddHTLC message. The breakdown of the onion packet is as
// follows: 1-byte version, 33-byte ephemeral public key (for ECDH), 1300-bytes
// of per-hop data, and a 32-byte HMAC over the entire packet.
const OnionPacketSize = 1366

// UpdateAddHTLC is the message sent by Alice to Bob when she wishes to add an
// HTLC to his remote commitment transaction. In addition to information
// detailing the value, the ID, expiry, and the onion blob is also included
// which allows Bob to derive the next hop in the route. The HTLC added by this
// message is to be added to the remote node's "pending" HTLC's.  A subsequent
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

	// Endorsed indicates whether an experimental endorsement signal is
	// included with this HTLC. Note that this value is obtained from
	// ExtraData on decoding, and packed into ExtraData on encoding for
	// easy access.
	Endorsed EndorsementSignal

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
	if err := ReadElements(r,
		&c.ChanID,
		&c.ID,
		&c.Amount,
		c.PaymentHash[:],
		&c.Expiry,
		c.OnionBlob[:],
		&c.ExtraData,
	); err != nil {
		return err
	}

	// If an endorsement signal is present, pull it out now for easy use.
	_, err := c.ExtraData.ExtractRecords(&c.Endorsed)
	if err != nil {
		return err
	}

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

	// If we have any fields that we need packed into extra records, add
	// them now. Note that any other extra records must be included here
	// if we choose to add them. If they are packed into ExtraData at a
	// higher level, they'll be overwritten.
	if err := c.ExtraData.PackRecords(&c.Endorsed); err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
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

// EndorsedHTLCExperimental is the TLV number for experimental HTLC
// endorsement signaling.
// See: https://github.com/lightning/blips/pull/27
const EndorsedHTLCExperimental tlv.Type = 65555

// EndorsementSignal is an alias that allows implementing the RecordProducer
// interface on a boolean.
type EndorsementSignal bool

// NewEndorsedRecord creates a TLV record for a single-byte endorsement record.
func (e *EndorsementSignal) Record() tlv.Record {
	// Note: byte is an alias for uint8 in go, so we can re-use its
	// encoding/decoding functions then convert back to our boolean value.
	return tlv.MakeStaticRecord(
		EndorsedHTLCExperimental, e, 1,
		func(w io.Writer, val interface{}, buf *[8]byte) error {
			if v, ok := val.(*EndorsementSignal); ok {
				// Convert boolean value to a single byte.
				var endorsedByte byte
				if *v {
					endorsedByte = 1
				}

				return tlv.EUint8(w, &endorsedByte, buf)
			}

			return tlv.NewTypeForEncodingErr(val, "*bool")
		},
		func(r io.Reader, val interface{}, buf *[8]byte,
			l uint64) error {

			if v, ok := val.(*EndorsementSignal); ok && l == 1 {
				var endorsedByte byte

				err := tlv.DUint8(r, &endorsedByte, buf, l)
				if err != nil {
					return err
				}

				// Interpret any non-zero value as a positive
				// endorsement.
				if endorsedByte == 0x0 {
					*v = false
				} else {
					*v = true
				}

				return nil
			}

			return tlv.NewTypeForDecodingErr(val, "*bool", l, 1)
		},
	)
}
