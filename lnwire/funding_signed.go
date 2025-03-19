package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// FundingSigned is sent from Bob (the responder) to Alice (the initiator)
// after receiving the funding outpoint and her signature for Bob's version of
// the commitment transaction.
type FundingSigned struct {
	// ChannelPoint is the particular active channel that this
	// FundingSigned is bound to.
	ChanID ChannelID

	// CommitSig is Bob's signature for Alice's version of the commitment
	// transaction.
	CommitSig Sig

	// PartialSig is used to transmit a musig2 extended partial signature
	// that also carries along the public nonce of the signer.
	//
	// NOTE: This field is only populated if a musig2 taproot channel is
	// being signed for. In this case, the above Sig type MUST be blank.
	PartialSig OptPartialSigWithNonceTLV

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// A compile time check to ensure FundingSigned implements the lnwire.Message
// interface.
var _ Message = (*FundingSigned)(nil)

// A compile time check to ensure FundingSigned implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*FundingSigned)(nil)

// Encode serializes the target FundingSigned into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (f *FundingSigned) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := make([]tlv.RecordProducer, 0, 1)
	f.PartialSig.WhenSome(func(sig PartialSigWithNonceTLV) {
		recordProducers = append(recordProducers, &sig)
	})
	err := EncodeMessageExtraData(&f.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteChannelID(w, f.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, f.CommitSig); err != nil {
		return err
	}

	return WriteBytes(w, f.ExtraData)
}

// Decode deserializes the serialized FundingSigned stored in the passed
// io.Reader into the target FundingSigned using the deserialization rules
// defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (f *FundingSigned) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r, &f.ChanID, &f.CommitSig)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	partialSig := f.PartialSig.Zero()
	typeMap, err := tlvRecords.ExtractRecords(&partialSig)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[f.PartialSig.TlvType()]; ok && val == nil {
		f.PartialSig = tlv.SomeRecordT(partialSig)
	}

	if len(tlvRecords) != 0 {
		f.ExtraData = tlvRecords
	}

	return nil
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// FundingSigned on the wire.
//
// This is part of the lnwire.Message interface.
func (f *FundingSigned) MsgType() MessageType {
	return MsgFundingSigned
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (f *FundingSigned) SerializedSize() (uint32, error) {
	return MessageSerializedSize(f)
}
