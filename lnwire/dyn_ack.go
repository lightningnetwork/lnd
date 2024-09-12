package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DALocalMusig2Pubnonce is the TLV type number that identifies the
	// musig2 public nonce that we need to verify the commitment transaction
	// signature.
	DALocalMusig2Pubnonce tlv.Type = 0
)

// DynAck is the message used to accept the parameters of a dynamic commitment
// negotiation. Additional optional parameters will need to be present depending
// on the details of the dynamic commitment upgrade.
type DynAck struct {
	// ChanID is the ChannelID of the channel that is currently undergoing
	// a dynamic commitment negotiation
	ChanID ChannelID

	// Sig is a signature that acknowledges and approves the parameters
	// that were requested in the DynPropose
	Sig Sig

	// LocalNonce is an optional field that is transmitted when accepting
	// a dynamic commitment upgrade to Taproot Channels. This nonce will be
	// used to verify the first commitment transaction signature. This will
	// only be populated if the DynPropose we are responding to specifies
	// taproot channels in the ChannelType field.
	LocalNonce fn.Option[Musig2Nonce]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure DynAck implements the lnwire.Message
// interface.
var _ Message = (*DynAck)(nil)

// A compile time check to ensure DynAck implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*DynAck)(nil)

// Encode serializes the target DynAck into the passed io.Writer. Serialization
// will observe the rules defined by the passed protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, da.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, da.Sig); err != nil {
		return err
	}

	var tlvRecords []tlv.Record
	da.LocalNonce.WhenSome(func(nonce Musig2Nonce) {
		tlvRecords = append(
			tlvRecords, tlv.MakeStaticRecord(
				DALocalMusig2Pubnonce, &nonce,
				musig2.PubNonceSize, nonceTypeEncoder,
				nonceTypeDecoder,
			),
		)
	})
	tlv.SortRecords(tlvRecords)

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	var extraBytesWriter bytes.Buffer
	if err := tlvStream.Encode(&extraBytesWriter); err != nil {
		return err
	}

	da.ExtraData = ExtraOpaqueData(extraBytesWriter.Bytes())

	return WriteBytes(w, da.ExtraData)
}

// Decode deserializes the serialized DynAck stored in the passed io.Reader into
// the target DynAck using the deserialization rules defined by the passed
// protocol version.
//
// This is a part of the lnwire.Message interface.
func (da *DynAck) Decode(r io.Reader, _ uint32) error {
	// Parse out main message.
	if err := ReadElements(r, &da.ChanID, &da.Sig); err != nil {
		return err
	}

	// Parse out TLV records.
	var tlvRecords ExtraOpaqueData
	if err := ReadElement(r, &tlvRecords); err != nil {
		return err
	}

	// Prepare receiving buffers to be filled by TLV extraction.
	var localNonceScratch Musig2Nonce
	localNonce := tlv.MakeStaticRecord(
		DALocalMusig2Pubnonce, &localNonceScratch, musig2.PubNonceSize,
		nonceTypeEncoder, nonceTypeDecoder,
	)

	// Create set of Records to read TLV bytestream into.
	records := []tlv.Record{localNonce}
	tlv.SortRecords(records)

	// Read TLV stream into record set.
	extraBytesReader := bytes.NewReader(tlvRecords)
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}
	typeMap, err := tlvStream.DecodeWithParsedTypesP2P(extraBytesReader)
	if err != nil {
		return err
	}

	// Check the results of the TLV Stream decoding and appropriately set
	// message fields.
	if val, ok := typeMap[DALocalMusig2Pubnonce]; ok && val == nil {
		da.LocalNonce = fn.Some(localNonceScratch)
	}

	if len(tlvRecords) != 0 {
		da.ExtraData = tlvRecords
	}

	return nil
}

// MsgType returns the MessageType code which uniquely identifies this message
// as a DynAck on the wire.
//
// This is part of the lnwire.Message interface.
func (da *DynAck) MsgType() MessageType {
	return MsgDynAck
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (da *DynAck) SerializedSize() (uint32, error) {
	return MessageSerializedSize(da)
}
