package lnwire

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// BlindingPointRecordType is the type which refers to an ephemeral
	// public key used in route blinding.
	BlindingPointRecordType tlv.Type = 1
)

// NOTE(9/20/22): The swap from tlv.Record to tlv.RecordProducer inside
// ExtractRecords() requires us to define this type and associated (en/de)code
// methods, as it precludes us from being able to leverage the 'record'
// package's "primitive" TLV types. Instead it pushes us to define our own
// custom type so that we may satisfy the RecordProducer interface{}.
// It might be nice to be able to take advantage of the primitive types that
// the 'record' package offers. No type casting. The change to the
// RecordProducer{} interface is said to "reduce line noise", but I am not yet
// sure what this means. Are there any more details on why this was swapped?
// https://github.com/lightningnetwork/lnd/pull/5669/commits/57b7a668c00ad3ebc049fd3517517118389238e0#diff-0352ea3877666d95afd22632bf69ef124ae1d072a206cf257a7cef618b6562b1R54
type BlindingPoint btcec.PublicKey

// Record returns a TLV record that can be used to encode/decode the
// ephemeral (route) blinding point type from a given TLV stream.
func (b *BlindingPoint) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		BlindingPointRecordType, b, 33, blindingPointEncoder, blindingPointDecoder,
	)
}

// blindingPointEncoder is a custom TLV encoder for the BlindingPoint record.
func blindingPointEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*BlindingPoint); ok {
		// Convert from *BlindingPoint to **btcec.PublicKey
		// so that we can use tlv.EPubKey()?
		key := new(btcec.PublicKey)
		*key = btcec.PublicKey(*v)
		if err := tlv.EPubKey(w, &key, buf); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.BlindingPoint")
}

// blindingPointDecoder is a custom TLV decoder for the BlindingPoint record.
func blindingPointDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*BlindingPoint); ok {

		// 1. Read bytes into internally defined variable.
		// Define **btcec.PublicKey so that we can use tlv.DPubKey()?
		var blindingPoint *btcec.PublicKey

		if err := tlv.DPubKey(r, &blindingPoint, buf, l); err != nil {
			return err
		}

		// 2. Convert internal variable to desired custom type.
		*v = BlindingPoint(*blindingPoint)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.BlindingPoint", l, 33)
}
