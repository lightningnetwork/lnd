package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// PartialSigLength is the length of a serialized PartialSig. The sig is
	// encoded as the 32 byte S value followed by the 66 nonce value.
	PartialSigLen = 98

	// PartialSigRecordType...
	PartialSigRecordType = 2
)

// PartialSig...
type PartialSig struct {
	// Nonce....
	Nonce Musig2Nonce

	// Sig...
	Sig btcec.ModNScalar
}

// NewPartialSig...
func NewPartialSig(nonce [musig2.PubNonceSize]byte,
	sig btcec.ModNScalar) *PartialSig {

	return &PartialSig{
		Nonce: nonce,
		Sig:   sig,
	}
}

// Record...
func (p *PartialSig) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		PartialSigRecordType, p, PartialSigLen, partialSigTypeEncoder,
		partialSigTypeDecoder,
	)
}

// partialSigTypeEncoder encodes 98-byte musig2 extended partial signature as:
// s {32} || nonce {66}.
func partialSigTypeEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*PartialSig); ok {
		sigBytes := v.Sig.Bytes()
		if _, err := w.Write(sigBytes[:]); err != nil {
			return err
		}
		if _, err := w.Write(v.Nonce[:]); err != nil {
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

// partialSigTypeDecoder decodes a 98-byte musig2 extended partial signature.
func partialSigTypeDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*PartialSig); ok && l == PartialSigLen {
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

		*v = PartialSig{
			Sig:   s,
			Nonce: nonce,
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
