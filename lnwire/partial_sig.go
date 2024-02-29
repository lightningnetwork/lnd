package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// PartialSigLen is the length of a musig2 partial signature.
	PartialSigLen = 32
)

type (
	// PartialSigType is the type of the tlv record for a musig2
	// partial signature. This is an _even_ type, which means it's required
	// if included.
	PartialSigType = tlv.TlvType6

	// PartialSigTLV is a tlv record for a musig2 partial signature.
	PartialSigTLV = tlv.RecordT[PartialSigType, PartialSig]

	// OptPartialSigTLV is a tlv record for a musig2 partial signature.
	// This is an optional record type.
	OptPartialSigTLV = tlv.OptionalRecordT[PartialSigType, PartialSig]
)

// PartialSig is the base partial sig type. This only encodes the 32-byte
// partial signature. This is used for the co-op close flow, as both sides have
// already exchanged nonces, so they can send just the partial signature.
type PartialSig struct {
	// Sig is the 32-byte musig2 partial signature.
	Sig btcec.ModNScalar
}

// NewPartialSig creates a new partial sig.
func NewPartialSig(sig btcec.ModNScalar) PartialSig {
	return PartialSig{
		Sig: sig,
	}
}

// Record returns the tlv record for the partial sig.
func (p *PartialSig) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		(PartialSigType)(nil).TypeVal(), p, PartialSigLen,
		partialSigTypeEncoder, partialSigTypeDecoder,
	)
}

// partialSigTypeEncoder encodes a 32-byte musig2 partial signature as a TLV
// value.
func partialSigTypeEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*PartialSig); ok {
		sigBytes := v.Sig.Bytes()

		return tlv.EBytes32(w, &sigBytes, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.PartialSig")
}

// Encode writes the encoded version of this message to the passed io.Writer.
func (p *PartialSig) Encode(w io.Writer) error {
	return partialSigTypeEncoder(w, p, nil)
}

// partialSigTypeDecoder decodes a 32-byte musig2 extended partial signature.
func partialSigTypeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*PartialSig); ok && l == PartialSigLen {
		var sBytes [32]byte
		err := tlv.DBytes32(r, &sBytes, buf, PartialSigLen)
		if err != nil {
			return err
		}

		var s btcec.ModNScalar
		s.SetBytes(&sBytes)

		*v = PartialSig{
			Sig: s,
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.PartialSig", l,
		PartialSigLen)
}

// Decode reads the encoded version of this message from the passed io.Reader.
func (p *PartialSig) Decode(r io.Reader) error {
	return partialSigTypeDecoder(r, p, nil, PartialSigLen)
}

// SomePartialSig is a helper function that returns an otional PartialSig.
func SomePartialSig(sig PartialSig) OptPartialSigTLV {
	return tlv.SomeRecordT(tlv.NewRecordT[PartialSigType, PartialSig](sig))
}

const (
	// PartialSigWithNonceLen is the length of a serialized
	// PartialSigWithNonce. The sig is encoded as the 32 byte S value
	// followed by the 66 nonce value.
	PartialSigWithNonceLen = 98
)

type (
	// PartialSigWithNonceType is the type of the tlv record for a musig2
	// partial signature with nonce. This is an _even_ type, which means
	// it's required if included.
	PartialSigWithNonceType = tlv.TlvType2

	// PartialSigWithNonceTLV is a tlv record for a musig2 partial
	// signature.
	PartialSigWithNonceTLV = tlv.RecordT[
		PartialSigWithNonceType, PartialSigWithNonce,
	]

	// OptPartialSigWithNonceTLV is a tlv record for a musig2 partial
	// signature.  This is an optional record type.
	OptPartialSigWithNonceTLV = tlv.OptionalRecordT[
		PartialSigWithNonceType, PartialSigWithNonce,
	]
)

// PartialSigWithNonce is a partial signature with the nonce that was used to
// generate the signature. This is used for funding as well as the commitment
// transaction update dance. By sending the nonce only with the signature, we
// enable the sender to generate their nonce just before they create their
// signature. Signers can use this trait to mix in additional contextual data
// such as the commitment txn itself into their nonce generation function.
//
// The final signature is 98 bytes: 32 bytes for the S value, and 66 bytes for
// the public nonce (two compressed points).
type PartialSigWithNonce struct {
	PartialSig

	// Nonce is the 66-byte musig2 nonce.
	Nonce Musig2Nonce
}

// NewPartialSigWithNonce creates a new partial sig with nonce.
func NewPartialSigWithNonce(nonce [musig2.PubNonceSize]byte,
	sig btcec.ModNScalar) *PartialSigWithNonce {

	return &PartialSigWithNonce{
		Nonce:      nonce,
		PartialSig: NewPartialSig(sig),
	}
}

// Record returns the tlv record for the partial sig with nonce.
func (p *PartialSigWithNonce) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		(PartialSigWithNonceType)(nil).TypeVal(), p,
		PartialSigWithNonceLen, partialSigWithNonceTypeEncoder,
		partialSigWithNonceTypeDecoder,
	)
}

// partialSigWithNonceTypeEncoder encodes 98-byte musig2 extended partial
// signature as: s {32} || nonce {66}.
func partialSigWithNonceTypeEncoder(w io.Writer, val interface{},
	_ *[8]byte) error {

	if v, ok := val.(*PartialSigWithNonce); ok {
		sigBytes := v.Sig.Bytes()
		if _, err := w.Write(sigBytes[:]); err != nil {
			return err
		}
		if _, err := w.Write(v.Nonce[:]); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.PartialSigWithNonce")
}

// Encode writes the encoded version of this message to the passed io.Writer.
func (p *PartialSigWithNonce) Encode(w io.Writer) error {
	return partialSigWithNonceTypeEncoder(w, p, nil)
}

// partialSigWithNonceTypeDecoder decodes a 98-byte musig2 extended partial
// signature.
func partialSigWithNonceTypeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*PartialSigWithNonce); ok &&
		l == PartialSigWithNonceLen {

		var sBytes [32]byte
		err := tlv.DBytes32(r, &sBytes, buf, PartialSigLen)
		if err != nil {
			return err
		}

		var s btcec.ModNScalar
		s.SetBytes(&sBytes)

		var nonce [66]byte
		if _, err := io.ReadFull(r, nonce[:]); err != nil {
			return err
		}

		*v = PartialSigWithNonce{
			PartialSig: NewPartialSig(s),
			Nonce:      nonce,
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.PartialSigWithNonce", l,
		PartialSigWithNonceLen)
}

// Decode reads the encoded version of this message from the passed io.Reader.
func (p *PartialSigWithNonce) Decode(r io.Reader) error {
	return partialSigWithNonceTypeDecoder(
		r, p, nil, PartialSigWithNonceLen,
	)
}

// MaybePartialSigWithNonce is a helper function that returns an optional
// PartialSigWithNonceTLV.
func MaybePartialSigWithNonce(sig *PartialSigWithNonce,
) OptPartialSigWithNonceTLV {

	if sig == nil {
		var none OptPartialSigWithNonceTLV
		return none
	}

	return tlv.SomeRecordT(
		tlv.NewRecordT[PartialSigWithNonceType, PartialSigWithNonce](
			*sig,
		),
	)
}
