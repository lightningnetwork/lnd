package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
)


// TaprootPartialSigs houses the 3 possible taproot partial signatures (without nonces)
// that can be sent in a ClosingSig message. These use just PartialSig since the
// receiver already knows our nonce from the previous ClosingComplete.
type TaprootPartialSigs struct {
	// CloserNoClosee is a partial signature that excludes the
	// output of the closee. Uses TLV type 5.
	CloserNoClosee tlv.OptionalRecordT[tlv.TlvType5, PartialSig]

	// NoCloserClosee is a partial signature that excludes the
	// output of the closer. Uses TLV type 6.
	NoCloserClosee tlv.OptionalRecordT[tlv.TlvType6, PartialSig]

	// CloserAndClosee is a partial signature that includes
	// both outputs. Uses TLV type 7.
	CloserAndClosee tlv.OptionalRecordT[tlv.TlvType7, PartialSig]
}

// ClosingSig is sent in response to a ClosingComplete message. It carries the
// signatures of the closee to the closer.
type ClosingSig struct {
	// ChannelID serves to identify which channel is to be closed.
	ChannelID ChannelID

	// CloserScript is the script to which the channel funds will be paid
	// for the closer (the person sending the ClosingComplete) message.
	CloserScript DeliveryAddress

	// CloseeScript is the script to which the channel funds will be paid
	// (the person receiving the ClosingComplete message).
	CloseeScript DeliveryAddress

	// FeeSatoshis is the total fee in satoshis that the party to the
	// channel proposed for the close transaction.
	FeeSatoshis btcutil.Amount

	// LockTime is the locktime number to be used in the input spending the
	// funding transaction.
	LockTime uint32

	// ClosingSigs houses the 3 possible signatures that can be sent.
	// For non-taproot channels, these are regular signatures.
	ClosingSigs

	// TaprootPartialSigs houses the 3 possible taproot partial signatures
	// that can be sent. For ClosingSig, we only send the partial signature
	// without the nonce since the remote already knows our nonce from the
	// previous ClosingComplete message.
	//
	// NOTE: This field is only populated for taproot channels. When present,
	// the above ClosingSigs MUST be empty.
	TaprootPartialSigs

	// NextCloseeNonce is an optional nonce for RBF iterations. This is the
	// nonce that the closer should use for this party's closee signature
	// in the next RBF round.
	//
	// NOTE: This field is only populated for taproot channels during RBF.
	NextCloseeNonce tlv.OptionalRecordT[tlv.TlvType22, Musig2Nonce]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// decodeClosingSigSigs decodes the closing sig TLV records from the passed
// ExtraOpaqueData.
func decodeClosingSigSigs(c *ClosingSigs, tp *TaprootPartialSigs, 
	nextNonce *tlv.OptionalRecordT[tlv.TlvType22, Musig2Nonce], 
	tlvRecords ExtraOpaqueData) error {
	// Regular signatures
	sig1 := c.CloserNoClosee.Zero()
	sig2 := c.NoCloserClosee.Zero()
	sig3 := c.CloserAndClosee.Zero()
	
	// Taproot partial signatures (without nonces)
	tSig1 := tp.CloserNoClosee.Zero()
	tSig2 := tp.NoCloserClosee.Zero()
	tSig3 := tp.CloserAndClosee.Zero()
	
	// Next closee nonce for RBF
	nonce := nextNonce.Zero()

	typeMap, err := tlvRecords.ExtractRecords(
		&sig1, &sig2, &sig3, &tSig1, &tSig2, &tSig3, &nonce,
	)
	if err != nil {
		return err
	}

	// Regular signatures
	if val, ok := typeMap[c.CloserNoClosee.TlvType()]; ok && val == nil {
		c.CloserNoClosee = tlv.SomeRecordT(sig1)
	}
	if val, ok := typeMap[c.NoCloserClosee.TlvType()]; ok && val == nil {
		c.NoCloserClosee = tlv.SomeRecordT(sig2)
	}
	if val, ok := typeMap[c.CloserAndClosee.TlvType()]; ok && val == nil {
		c.CloserAndClosee = tlv.SomeRecordT(sig3)
	}
	
	// Taproot partial signatures
	if val, ok := typeMap[tp.CloserNoClosee.TlvType()]; ok && val == nil {
		tp.CloserNoClosee = tlv.SomeRecordT(tSig1)
	}
	if val, ok := typeMap[tp.NoCloserClosee.TlvType()]; ok && val == nil {
		tp.NoCloserClosee = tlv.SomeRecordT(tSig2)
	}
	if val, ok := typeMap[tp.CloserAndClosee.TlvType()]; ok && val == nil {
		tp.CloserAndClosee = tlv.SomeRecordT(tSig3)
	}
	
	// Next closee nonce
	if val, ok := typeMap[nextNonce.TlvType()]; ok && val == nil {
		*nextNonce = tlv.SomeRecordT(nonce)
	}

	return nil
}

// Decode deserializes a serialized ClosingSig message stored in the passed
// io.Reader.
func (c *ClosingSig) Decode(r io.Reader, _ uint32) error {
	// First, read out all the fields that are hard coded into the message.
	err := ReadElements(
		r, &c.ChannelID, &c.CloserScript, &c.CloseeScript,
		&c.FeeSatoshis, &c.LockTime,
	)
	if err != nil {
		return err
	}

	// With the hard coded messages read, we'll now read out the TLV fields
	// of the message.
	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	if err := decodeClosingSigSigs(&c.ClosingSigs, &c.TaprootPartialSigs, &c.NextCloseeNonce, tlvRecords); err != nil {
		return err
	}

	if len(tlvRecords) != 0 {
		c.ExtraData = tlvRecords
	}

	return nil
}

// closingSigSigRecords returns the set of records that encode the closing sigs,
// including both regular and taproot signatures.
func closingSigSigRecords(c *ClosingSigs, tp *TaprootPartialSigs, 
	nextNonce tlv.OptionalRecordT[tlv.TlvType22, Musig2Nonce]) []tlv.RecordProducer {
	recordProducers := make([]tlv.RecordProducer, 0, 7)
	
	// Regular signatures
	c.CloserNoClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType1, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})
	c.NoCloserClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType2, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})
	c.CloserAndClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType3, Sig]) {
		recordProducers = append(recordProducers, &sig)
	})
	
	// Taproot partial signatures (without nonces)
	tp.CloserNoClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType5, PartialSig]) {
		recordProducers = append(recordProducers, &sig)
	})
	tp.NoCloserClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType6, PartialSig]) {
		recordProducers = append(recordProducers, &sig)
	})
	tp.CloserAndClosee.WhenSome(func(sig tlv.RecordT[tlv.TlvType7, PartialSig]) {
		recordProducers = append(recordProducers, &sig)
	})
	
	// Next closee nonce for RBF
	nextNonce.WhenSome(func(nonce tlv.RecordT[tlv.TlvType22, Musig2Nonce]) {
		recordProducers = append(recordProducers, &nonce)
	})

	return recordProducers
}

// Encode serializes the target ClosingSig into the passed io.Writer.
func (c *ClosingSig) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, c.ChannelID); err != nil {
		return err
	}

	if err := WriteDeliveryAddress(w, c.CloserScript); err != nil {
		return err
	}
	if err := WriteDeliveryAddress(w, c.CloseeScript); err != nil {
		return err
	}

	if err := WriteSatoshi(w, c.FeeSatoshis); err != nil {
		return err
	}

	if err := WriteUint32(w, c.LockTime); err != nil {
		return err
	}

	recordProducers := closingSigSigRecords(&c.ClosingSigs, &c.TaprootPartialSigs, c.NextCloseeNonce)

	err := EncodeMessageExtraData(&c.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the uint32 code which uniquely identifies this message as a
// ClosingSig message on the wire.
//
// This is part of the lnwire.Message interface.
func (c *ClosingSig) MsgType() MessageType {
	return MsgClosingSig
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *ClosingSig) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}

// A compile time check to ensure ClosingSig implements the lnwire.Message
// interface.
var _ Message = (*ClosingSig)(nil)

// A compile time check to ensure ClosingSig implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*ClosingSig)(nil)
