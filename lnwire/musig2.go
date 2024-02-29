package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

// NonceRecordTypeT is the TLV type used to encode a local musig2 nonce.
type NonceRecordTypeT = tlv.TlvType4

// nonceRecordType is the TLV (integer) type used to encode a local musig2
// nonce.
var nonceRecordType tlv.Type = (NonceRecordTypeT)(nil).TypeVal()

type (
	// Musig2Nonce represents a musig2 public nonce, which is the
	// concatenation of two EC points serialized in compressed format.
	Musig2Nonce [musig2.PubNonceSize]byte

	// Musig2NonceTLV is a TLV type that can be used to encode/decode a
	// musig2 nonce. This is an optional TLV.
	Musig2NonceTLV = tlv.RecordT[NonceRecordTypeT, Musig2Nonce]

	// OptMusig2NonceTLV is a TLV type that can be used to encode/decode a
	// musig2 nonce.
	OptMusig2NonceTLV = tlv.OptionalRecordT[NonceRecordTypeT, Musig2Nonce]
)

// Record returns a TLV record that can be used to encode/decode the musig2
// nonce from a given TLV stream.
func (m *Musig2Nonce) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		nonceRecordType, m, musig2.PubNonceSize,
		nonceTypeEncoder, nonceTypeDecoder,
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

// SomeMusig2Nonce is a helper function that creates a musig2 nonce TLV.
func SomeMusig2Nonce(nonce Musig2Nonce) OptMusig2NonceTLV {
	return tlv.SomeRecordT(
		tlv.NewRecordT[NonceRecordTypeT, Musig2Nonce](nonce),
	)
}
