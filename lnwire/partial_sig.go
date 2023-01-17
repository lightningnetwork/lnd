package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// PartialSigLen...
	PartialSigLen = 32

	// PartialSigRecordType...
	PartialSigRecordType = 6
)

// PartialSig...
type PartialSig struct {
	// Sig...
	Sig btcec.ModNScalar
}

// NewPartialSig...
func NewPartialSig(sig btcec.ModNScalar) PartialSig {
	return PartialSig{
		Sig: sig,
	}
}

// Record...
func (p *PartialSig) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		PartialSigRecordType, p, PartialSigLen,
		partialSigTypeEncoder, partialSigTypeDecoder,
	)
}

// partialSigTypeEncoder...
func partialSigTypeEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*PartialSig); ok {
		sigBytes := v.Sig.Bytes()
		if _, err := w.Write(sigBytes[:]); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.PartialSig")
}

// Encode...
func (p *PartialSig) Encode(w io.Writer) error {
	return partialSigTypeEncoder(w, p, nil)
}

// partialSigWithNonceTypeDecoder decodes a 98-byte musig2 extended partial
// signature.
func partialSigTypeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*PartialSig); ok && l == PartialSigLen {
		var sBytes [32]byte
		if _, err := io.ReadFull(r, sBytes[:]); err != nil {
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

// Decode...
func (p *PartialSig) Decode(r io.Reader) error {
	return partialSigTypeDecoder(r, p, nil, PartialSigLen)
}

const (
	// PartialSigWithNonceLength is the length of a serialized
	// PartialSigWithNonce. The sig is encoded as the 32 byte S value
	// followed by the 66 nonce value.
	PartialSigWithNonceLen = 98

	// PartialSigWithNonceRecordType...
	PartialSigWithNonceRecordType = 2
)

// PartialSigWithNonce...
type PartialSigWithNonce struct {
	PartialSig

	// Nonce....
	Nonce Musig2Nonce
}

// NewPartialSigWithNonce...
func NewPartialSigWithNonce(nonce [musig2.PubNonceSize]byte,
	sig btcec.ModNScalar) *PartialSigWithNonce {

	return &PartialSigWithNonce{
		Nonce:      nonce,
		PartialSig: NewPartialSig(sig),
	}
}

// Record...
func (p *PartialSigWithNonce) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		PartialSigWithNonceRecordType, p, PartialSigWithNonceLen,
		partialSigWithNonceTypeEncoder, partialSigWithNonceTypeDecoder,
	)
}

// partialSigWithNonceTypeEncoder encodes 98-byte musig2 extended partial
// signature as: s {32} || nonce {66}.
func partialSigWithNonceTypeEncoder(w io.Writer, val interface{},
	buf *[8]byte) error {

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

// Encode...
func (p *PartialSigWithNonce) Encode(w io.Writer) error {
	return partialSigWithNonceTypeEncoder(w, p, nil)
}

// partialSigWithNonceTypeDecoder decodes a 98-byte musig2 extended partial
// signature.
func partialSigWithNonceTypeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*PartialSigWithNonce); ok && l == PartialSigWithNonceLen {
		var sBytes [32]byte
		if _, err := io.ReadFull(r, sBytes[:]); err != nil {
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

// Decode...
func (p *PartialSigWithNonce) Decode(r io.Reader) error {
	return partialSigWithNonceTypeDecoder(
		r, p, nil, PartialSigWithNonceLen,
	)
}
