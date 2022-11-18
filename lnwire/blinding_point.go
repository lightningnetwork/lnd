package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// BlindingPointRecordType is the type for ephemeral pubkeys used in
	// route blinding.
	BlindingPointRecordType tlv.Type = 0
)

// BlindingPoint is used to communicate ephemeral pubkeys used by route
// blinding.
//
// Note: this struct wraps a btcec.Pubkey key so that we can implement the
// RecordProducer interface on the struct and re-use the existing tlv library
// functions for public keys. The type is unexported to enforce proper handling
// of aliasing / nil types.
type blindingPoint btcec.PublicKey

// NewBlindingPoint converts a pubkey into its aliased blinding point type,
// returning nil if the pubkey provided is nil.
//
//nolint:revive
func NewBlindingPoint(pubkey *btcec.PublicKey) *blindingPoint {
	if pubkey == nil {
		return nil
	}

	blindingPoint := blindingPoint(*pubkey)

	return &blindingPoint
}

// Pubkey returns the underlying btcec.Pubkey type for a blinding point.
func (b *blindingPoint) Pubkey() *btcec.PublicKey {
	if b == nil {
		return nil
	}

	pubkey := btcec.PublicKey(*b)

	return &pubkey
}

// Record returns a TLV record for blinded pubkeys.
//
// Note: implements the RecordProducer interface.
func (b *blindingPoint) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		BlindingPointRecordType, b, 33,
		blindingPointEncoder, blindingPointDecoder,
	)
}

// blindingPointEncoder is a custom TLV encoder for the BlindingPoint record.
func blindingPointEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*blindingPoint); ok {
		// EPubkey requires a double pointer, so we de-alias and
		// reference the blinding point provided.
		pubkey := btcec.PublicKey(*v)
		pubkeyRef := &pubkey
		return tlv.EPubKey(w, &pubkeyRef, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.BlindingPoint")
}

// blindingPointDecoder is a custom TLV decoder for the BlindingPoint record.
func blindingPointDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*blindingPoint); ok {
		var pubkey *btcec.PublicKey
		if err := tlv.DPubKey(r, &pubkey, buf, l); err != nil {
			return err
		}
		*v = blindingPoint(*pubkey)

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.BlindingPoint")
}
