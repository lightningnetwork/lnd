package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ShutdownNonceRecordType is the type of the shutdown nonce TLV record.
	ShutdownNonceRecordType = 8
)

// ShutdownNonce is the type of the nonce we send during the shutdown flow.
// Unlike the other nonces, this nonce is symmetric w.r.t the message being
// signed (there's only one message for shutdown: the co-op close txn).
type ShutdownNonce Musig2Nonce

// Record returns a TLV record that can be used to encode/decode the musig2
// nonce from a given TLV stream.
func (s *ShutdownNonce) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		ShutdownNonceRecordType, s, musig2.PubNonceSize,
		shutdownNonceTypeEncoder, shutdownNonceTypeDecoder,
	)
}

// shutdownNonceTypeEncoder is a custom TLV encoder for the Musig2Nonce type.
func shutdownNonceTypeEncoder(w io.Writer, val interface{},
	_ *[8]byte) error {

	if v, ok := val.(*ShutdownNonce); ok {
		_, err := w.Write(v[:])
		return err
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.Musig2Nonce")
}

// shutdownNonceTypeDecoder is a custom TLV decoder for the Musig2Nonce record.
func shutdownNonceTypeDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*ShutdownNonce); ok {
		_, err := io.ReadFull(r, v[:])
		return err
	}

	return tlv.NewTypeForDecodingErr(
		val, "lnwire.ShutdownNonce", l, musig2.PubNonceSize,
	)
}

// Shutdown is sent by either side in order to initiate the cooperative closure
// of a channel. This message is sparse as both sides implicitly have the
// information necessary to construct a transaction that will send the settled
// funds of both parties to the final delivery addresses negotiated during the
// funding workflow.
type Shutdown struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// Address is the script to which the channel funds will be paid.
	Address DeliveryAddress

	// ShutdownNonce is the nonce the sender will use to sign the first
	// co-op sign offer.
	ShutdownNonce *ShutdownNonce

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewShutdown creates a new Shutdown message.
func NewShutdown(cid ChannelID, addr DeliveryAddress) *Shutdown {
	return &Shutdown{
		ChannelID: cid,
		Address:   addr,
	}
}

// A compile-time check to ensure Shutdown implements the lnwire.Message
// interface.
var _ Message = (*Shutdown)(nil)

// Decode deserializes a serialized Shutdown stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r, &s.ChannelID, &s.Address)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	var musigNonce ShutdownNonce
	typeMap, err := tlvRecords.ExtractRecords(&musigNonce)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[ShutdownNonceRecordType]; ok && val == nil {
		s.ShutdownNonce = &musigNonce
	}

	if len(tlvRecords) != 0 {
		s.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target Shutdown into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := make([]tlv.RecordProducer, 0, 1)
	if s.ShutdownNonce != nil {
		recordProducers = append(recordProducers, s.ShutdownNonce)
	}
	err := EncodeMessageExtraData(&s.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteChannelID(w, s.ChannelID); err != nil {
		return err
	}

	if err := WriteDeliveryAddress(w, s.Address); err != nil {
		return err
	}

	return WriteBytes(w, s.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) MsgType() MessageType {
	return MsgShutdown
}
