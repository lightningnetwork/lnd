package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

type (
	// ShutdownNonceType is the type of the shutdown nonce TLV record.
	ShutdownNonceType = tlv.TlvType8

	// ShutdownNonceTLV is the TLV record that contains the shutdown nonce.
	ShutdownNonceTLV = tlv.OptionalRecordT[ShutdownNonceType, Musig2Nonce]
)

// SomeShutdownNonce returns a ShutdownNonceTLV with the given nonce.
func SomeShutdownNonce(nonce Musig2Nonce) ShutdownNonceTLV {
	return tlv.SomeRecordT(
		tlv.NewRecordT[ShutdownNonceType, Musig2Nonce](nonce),
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
	ShutdownNonce ShutdownNonceTLV

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

	musigNonce := s.ShutdownNonce.Zero()
	typeMap, err := tlvRecords.ExtractRecords(&musigNonce)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[s.ShutdownNonce.TlvType()]; ok && val == nil {
		s.ShutdownNonce = tlv.SomeRecordT(musigNonce)
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
	s.ShutdownNonce.WhenSome(
		func(nonce tlv.RecordT[ShutdownNonceType, Musig2Nonce]) {
			recordProducers = append(recordProducers, &nonce)
		},
	)
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
