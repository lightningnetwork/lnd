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

// TaprootNonceType indicates which nonce format to use for taproot channel
// messages like revoke_and_ack and channel_reestablish.
type TaprootNonceType uint8

const (
	// TaprootNonceTypeLegacy indicates that only the single LocalNonce
	// field should be populated. This is used for peers that support the
	// staging taproot channel feature bits (180/181).
	TaprootNonceTypeLegacy TaprootNonceType = iota

	// TaprootNonceTypeMap indicates that only the LocalNonces map-based
	// field should be populated. This is used for peers that support the
	// final taproot channel feature bits (80/81).
	TaprootNonceTypeMap
)

// DetermineTaprootNonceType returns the appropriate nonce type based on the
// peer's advertised feature bits. If the peer supports the final taproot
// channel feature bits (80/81), we use the map-based LocalNonces field.
// Otherwise, we fall back to the legacy single LocalNonce field.
func DetermineTaprootNonceType(features *FeatureVector) TaprootNonceType {
	if features == nil {
		return TaprootNonceTypeLegacy
	}

	if features.HasFeature(SimpleTaprootChannelsOptionalFinal) ||
		features.HasFeature(SimpleTaprootChannelsRequiredFinal) {

		return TaprootNonceTypeMap
	}

	return TaprootNonceTypeLegacy
}
