package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// ChannelReady is the message that both parties to a new channel creation
// send once they have observed the funding transaction being confirmed on the
// blockchain. ChannelReady contains the signatures necessary for the channel
// participants to advertise the existence of the channel to the rest of the
// network.
type ChannelReady struct {
	// ChanID is the outpoint of the channel's funding transaction. This
	// can be used to query for the channel in the database.
	ChanID ChannelID

	// NextPerCommitmentPoint is the secret that can be used to revoke the
	// next commitment transaction for the channel.
	NextPerCommitmentPoint *btcec.PublicKey

	// AliasScid is an alias ShortChannelID used to refer to the underlying
	// channel. It can be used instead of the confirmed on-chain
	// ShortChannelID for forwarding.
	AliasScid *ShortChannelID

	// NextLocalNonce is an optional field that stores a local musig2 nonce.
	// This will only be populated if the simple taproot channels type was
	// negotiated. This is the local nonce that will be used by the sender
	// to accept a new commitment state transition.
	NextLocalNonce OptMusig2NonceTLV

	// AnnouncementNodeNonce is an optional field that stores a public
	// nonce that will be used along with the node's ID key during signing
	// of the ChannelAnnouncement2 message.
	AnnouncementNodeNonce tlv.OptionalRecordT[tlv.TlvType0, Musig2Nonce]

	// AnnouncementBitcoinNonce is an optional field that stores a public
	// nonce that will be used along with the node's bitcoin key during
	// signing of the ChannelAnnouncement2 message.
	AnnouncementBitcoinNonce tlv.OptionalRecordT[tlv.TlvType2, Musig2Nonce]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewChannelReady creates a new ChannelReady message, populating it with the
// necessary IDs and revocation secret.
func NewChannelReady(cid ChannelID, npcp *btcec.PublicKey) *ChannelReady {
	return &ChannelReady{
		ChanID:                 cid,
		NextPerCommitmentPoint: npcp,
		ExtraData:              nil,
	}
}

// A compile time check to ensure ChannelReady implements the lnwire.Message
// interface.
var _ Message = (*ChannelReady)(nil)

// A compile time check to ensure ChannelReady implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ChannelReady)(nil)

// Decode deserializes the serialized ChannelReady message stored in the
// passed io.Reader into the target ChannelReady using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelReady) Decode(r io.Reader, _ uint32) error {
	// Read all the mandatory fields in the message.
	err := ReadElements(r,
		&c.ChanID,
		&c.NextPerCommitmentPoint,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	// Next we'll parse out the set of known records. For now, this is just
	// the AliasScidRecordType.
	var (
		aliasScid  ShortChannelID
		localNonce = c.NextLocalNonce.Zero()
		nodeNonce  = tlv.ZeroRecordT[tlv.TlvType0, Musig2Nonce]()
		btcNonce   = tlv.ZeroRecordT[tlv.TlvType2, Musig2Nonce]()
	)
	typeMap, err := tlvRecords.ExtractRecords(
		&btcNonce, &aliasScid, &nodeNonce, &localNonce,
	)
	if err != nil {
		return err
	}

	// We'll only set AliasScid if the corresponding TLV type was included
	// in the stream.
	if val, ok := typeMap[AliasScidRecordType]; ok && val == nil {
		c.AliasScid = &aliasScid
	}
	if val, ok := typeMap[c.NextLocalNonce.TlvType()]; ok && val == nil {
		c.NextLocalNonce = tlv.SomeRecordT(localNonce)
	}
	val, ok := typeMap[c.AnnouncementBitcoinNonce.TlvType()]
	if ok && val == nil {
		c.AnnouncementBitcoinNonce = tlv.SomeRecordT(btcNonce)
	}
	val, ok = typeMap[c.AnnouncementNodeNonce.TlvType()]
	if ok && val == nil {
		c.AnnouncementNodeNonce = tlv.SomeRecordT(nodeNonce)
	}

	if len(tlvRecords) != 0 {
		c.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target ChannelReady message into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelReady) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WritePublicKey(w, c.NextPerCommitmentPoint); err != nil {
		return err
	}

	// We'll only encode the AliasScid in a TLV segment if it exists.
	recordProducers := make([]tlv.RecordProducer, 0, 4)
	if c.AliasScid != nil {
		recordProducers = append(recordProducers, c.AliasScid)
	}
	c.NextLocalNonce.WhenSome(func(localNonce Musig2NonceTLV) {
		recordProducers = append(recordProducers, &localNonce)
	})
	c.AnnouncementBitcoinNonce.WhenSome(
		func(nonce tlv.RecordT[tlv.TlvType2, Musig2Nonce]) {
			recordProducers = append(recordProducers, &nonce)
		},
	)
	c.AnnouncementNodeNonce.WhenSome(
		func(nonce tlv.RecordT[tlv.TlvType0, Musig2Nonce]) {
			recordProducers = append(recordProducers, &nonce)
		},
	)

	err := EncodeMessageExtraData(&c.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// ChannelReady message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelReady) MsgType() MessageType {
	return MsgChannelReady
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ChannelReady) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
