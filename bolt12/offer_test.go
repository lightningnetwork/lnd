package bolt12

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOfferRoundTrip pins encode→decode→re-encode for an Offer with a
// representative subset of optional fields. A byte-identical re-encode is the
// invariant that keeps offer_id stable across the codec boundary.
func TestOfferRoundTrip(t *testing.T) {
	t.Parallel()

	desc := tlv.Blob("coffee")
	issuer := tlv.Blob("alice")
	_, bobPub := bobKey()

	o := &Offer{
		OfferAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8](TUint64(1500)),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](desc),
		),
		OfferIssuer: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType18](issuer),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](bobPub),
		),
	}

	encoded, err := o.Encode()
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := decodeOffer(encoded)
	require.NoError(t, err)

	require.Equal(t, TUint64(1500), decoded.OfferAmount.UnwrapOrFailV(t))
	require.Equal(t, desc, decoded.OfferDescription.UnwrapOrFailV(t))
	require.Equal(t, issuer, decoded.OfferIssuer.UnwrapOrFailV(t))

	reencoded, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, encoded, reencoded)
}

// TestDecodeOversizedRecord pins the per-record cap by feeding the decoder a
// TLV declaring a length one byte over tlv.MaxRecordSize.
func TestDecodeOversizedRecord(t *testing.T) {
	t.Parallel()

	// Build a synthetic TLV with type=22 (offer_issuer_id, known by the
	// offer decoder) and declared length one byte over the cap. The value
	// bytes are present so the framing itself is consistent.
	const oversize = tlv.MaxRecordSize + 1
	var (
		buf [8]byte
		w   bytes.Buffer
	)
	require.NoError(t, tlv.WriteVarInt(&w, 22, &buf))
	require.NoError(t, tlv.WriteVarInt(&w, oversize, &buf))
	w.Write(make([]byte, oversize))

	_, err := decodeOffer(w.Bytes())
	require.ErrorIs(
		t, err, tlv.ErrRecordTooLarge,
		"expected an oversize-record rejection, got %v", err,
	)
}
