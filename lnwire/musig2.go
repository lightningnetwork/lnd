package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

// Musig2Nonce represents a musig2 public nonce, which is the concatenation of
// two EC points serialized in compressed format.
type Musig2Nonce [musig2.PubNonceSize]byte

// Musig2NonceRecordProducer wraps a Musig2Nonce with the tlv type it should be
// encoded under. This can then be used to produce a TLV record.
type Musig2NonceRecordProducer struct {
	Musig2Nonce

	Type tlv.Type
}

// NewMusig2NonceRecordProducer constructs a new Musig2NonceRecordProducer with
// the given tlv type set.
func NewMusig2NonceRecordProducer(tlvType tlv.Type) *Musig2NonceRecordProducer {
	return &Musig2NonceRecordProducer{
		Type: tlvType,
	}
}

// Record returns a TLV record that can be used to encode/decode the musig2
// nonce from a given TLV stream.
func (m *Musig2NonceRecordProducer) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		m.Type, &m.Musig2Nonce, musig2.PubNonceSize, nonceTypeEncoder,
		nonceTypeDecoder,
	)
}

// nonceTypeEncoder is a custom TLV encoder for the Musig2Nonce type.
func nonceTypeEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*Musig2Nonce); ok {
		_, err := w.Write(v[:])
		return err
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.Musig2Nonce")
}

// nonceTypeDecoder is a custom TLV decoder for the Musig2Nonce record.
func nonceTypeDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*Musig2Nonce); ok {
		_, err := io.ReadFull(r, v[:])
		return err
	}

	return tlv.NewTypeForDecodingErr(
		val, "lnwire.Musig2Nonce", l, musig2.PubNonceSize,
	)
}
