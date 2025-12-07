package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// AnnounceSignatures2 is a direct message between two endpoints of a
// channel and serves as an opt-in mechanism to allow the announcement of
// a taproot channel to the rest of the network. It contains the necessary
// signatures by the sender to construct the channel_announcement_2 message.
type AnnounceSignatures2 struct {
	// ChannelID is the unique description of the funding transaction.
	// Channel id is better for users and debugging and short channel id is
	// used for quick test on existence of the particular utxo inside the
	// blockchain, because it contains information about block.
	ChannelID tlv.RecordT[tlv.TlvType0, ChannelID]

	// ShortChannelID is the unique description of the funding transaction.
	// It is constructed with the most significant 3 bytes as the block
	// height, the next 3 bytes indicating the transaction index within the
	// block, and the least significant two bytes indicating the output
	// index which pays to the channel.
	ShortChannelID tlv.RecordT[tlv.TlvType2, ShortChannelID]

	// PartialSignature is the combination of the partial Schnorr signature
	// created for the node's bitcoin key with the partial signature created
	// for the node's node ID key.
	PartialSignature tlv.RecordT[tlv.TlvType4, PartialSig]

	// Any extra fields in the signed range that we do not yet know about,
	// but we need to keep them for signature validation and to produce a
	// valid message.
	ExtraSignedFields
}

// NewAnnSigs2 is a constructor for AnnounceSignatures2.
func NewAnnSigs2(chanID ChannelID, scid ShortChannelID,
	partialSig PartialSig) *AnnounceSignatures2 {

	return &AnnounceSignatures2{
		ChannelID: tlv.NewRecordT[tlv.TlvType0, ChannelID](chanID),
		ShortChannelID: tlv.NewRecordT[tlv.TlvType2, ShortChannelID](
			scid,
		),
		PartialSignature: tlv.NewRecordT[tlv.TlvType4, PartialSig](
			partialSig,
		),
		ExtraSignedFields: make(ExtraSignedFields),
	}
}

// A compile time check to ensure AnnounceSignatures2 implements the
// lnwire.Message interface.
var _ Message = (*AnnounceSignatures2)(nil)

// A compile time check to ensure AnnounceSignatures2 implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*AnnounceSignatures2)(nil)

// A compile time check to ensure ChannelAnnouncement2 implements the
// lnwire.PureTLVMessage interface.
var _ PureTLVMessage = (*AnnounceSignatures2)(nil)

// Decode deserializes a serialized AnnounceSignatures2 stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures2) Decode(r io.Reader, _ uint32) error {
	stream, err := tlv.NewStream(ProduceRecordsSorted(
		&a.ChannelID, &a.ShortChannelID, &a.PartialSignature,
	)...)
	if err != nil {
		return err
	}

	typeMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return err
	}

	a.ExtraSignedFields = ExtraSignedFieldsFromTypeMap(typeMap)

	return nil
}

// Encode serializes the target AnnounceSignatures2 into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures2) Encode(w *bytes.Buffer, _ uint32) error {
	return EncodePureTLVMessage(a, w)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *AnnounceSignatures2) MsgType() MessageType {
	return MsgAnnounceSignatures2
}

// GossipVersion returns the gossip version that this message is part of.
//
// NOTE: this is part of the GossipMessage interface.
func (a *AnnounceSignatures2) GossipVersion() GossipVersion {
	return GossipVersion2
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (a *AnnounceSignatures2) SerializedSize() (uint32, error) {
	return MessageSerializedSize(a)
}

// AllRecords returns all the TLV records for the message. This will include all
// the records we know about along with any that we don't know about but that
// fall in the signed TLV range.
//
// NOTE: this is part of the PureTLVMessage interface.
func (a *AnnounceSignatures2) AllRecords() []tlv.Record {
	recordProducers := []tlv.RecordProducer{
		&a.ChannelID, &a.ShortChannelID,
		&a.PartialSignature,
	}

	recordProducers = append(recordProducers, RecordsAsProducers(
		tlv.MapToRecords(a.ExtraSignedFields),
	)...)

	return ProduceRecordsSorted(recordProducers...)
}

// SCID returns the ShortChannelID of the channel.
//
// NOTE: this is part of the AnnounceSignatures interface.
func (a *AnnounceSignatures2) SCID() ShortChannelID {
	return a.ShortChannelID.Val
}

// ChanID returns the ChannelID identifying the channel.
//
// NOTE: this is part of the AnnounceSignatures interface.
func (a *AnnounceSignatures2) ChanID() ChannelID {
	return a.ChannelID.Val
}
