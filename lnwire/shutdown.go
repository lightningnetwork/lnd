package lnwire

import (
	"bytes"
	"fmt"
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

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field of the Shutdown
	// message.
	CustomRecords CustomRecords

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

// Decode deserializes a serialized Shutdown from the passed io.Reader,
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

		// Remove the entry from the TLV map. Anything left in the map
		// will be included in the custom records field.
		delete(typeMap, s.ShutdownNonce.TlvType())
	}

	// Set the custom records field to the remaining TLV records, but only
	// those that actually are in the custom TLV type range.
	customRecords, filtered, err := FilteredCustomRecords(typeMap)
	if err != nil {
		return err
	}
	s.CustomRecords = customRecords

	// Set extra data to nil if we didn't parse anything out of it so that
	// we can use assert.Equal in tests.
	if len(filtered) == 0 {
		s.ExtraData = make([]byte, 0)
	} else {
		// Encode the remaining records into the extra data field. These
		// records are not in the custom records TLV type range and do
		// not have associated fields in the UpdateAddHTLC struct.
		s.ExtraData, err = NewExtraOpaqueDataFromTlvTypeMap(filtered)
		if err != nil {
			return err
		}
	}

	return nil
}

// Encode serializes the target Shutdown into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, s.ChannelID); err != nil {
		return err
	}

	if err := WriteDeliveryAddress(w, s.Address); err != nil {
		return err
	}

	// Construct a slice of all the records that we should include in the
	// message extra data field. We will start by including any records from
	// the extra data field.
	msgExtraDataRecords, err := s.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	s.ShutdownNonce.WhenSome(
		func(nonce tlv.RecordT[ShutdownNonceType, Musig2Nonce]) {
			msgExtraDataRecords = append(
				msgExtraDataRecords, &nonce,
			)
		},
	)

	// Include custom records in the extra data wire field if they are
	// present. Ensure that the custom records are validated before
	// encoding them.
	if err := s.CustomRecords.Validate(); err != nil {
		return fmt.Errorf("custom records validation error: %w", err)
	}

	// Extend the message extra data records slice with TLV records from
	// the custom records field.
	recordProducers, err := s.CustomRecords.ExtendRecordProducers(
		msgExtraDataRecords,
	)
	if err != nil {
		return err
	}

	// We will now construct the message extra data field that will be
	// encoded into the byte writer.
	var msgExtraData ExtraOpaqueData
	if err := msgExtraData.PackRecords(recordProducers...); err != nil {
		return err
	}

	return WriteBytes(w, msgExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (s *Shutdown) MsgType() MessageType {
	return MsgShutdown
}
