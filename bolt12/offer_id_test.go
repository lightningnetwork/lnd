package bolt12

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOfferIDMatchesOfferEncoding asserts that for an offer the id is the hash
// of its whole encoding. Every offer TLV already sits in the hashed ranges, so
// the range filter is a no-op there.
func TestOfferIDMatchesOfferEncoding(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "Minimal bolt12 offer")
	_, tlvBytes, err := Decode(vec.Bolt12)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	encoded, err := offer.Encode()
	require.NoError(t, err)

	id, err := OfferID(offer)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(encoded), id)
}

// TestOfferIDCoversUnknownOfferRangeTLVs asserts that an unknown TLV in the
// offer range changes the id. A caller that rebuilt the offer from its known
// fields instead would compute the same id for both requests and accept a
// request whose offer fields differ from the offer.
func TestOfferIDCoversUnknownOfferRangeTLVs(t *testing.T) {
	t.Parallel()

	_, pub := bobKey()
	pubBytes := pub.SerializeCompressed()

	// Build two invoice requests that differ only by an unknown odd TLV in
	// the offer range.
	request := func(withUnknown bool) *InvoiceRequest {
		var buf bytes.Buffer
		appendRawRecord(t, &buf, 0, []byte("meta"))
		appendRawRecord(t, &buf, 10, []byte("coffee"))
		if withUnknown {
			appendRawRecord(t, &buf, 13, []byte{0xde, 0xad})
		}
		appendRawRecord(t, &buf, 22, pubBytes)
		appendRawRecord(t, &buf, 88, pubBytes)

		ir, err := DecodeInvoiceRequest(buf.Bytes())
		require.NoError(t, err)

		return ir
	}

	plain, err := OfferID(request(false))
	require.NoError(t, err)

	withUnknown, err := OfferID(request(true))
	require.NoError(t, err)

	require.NotEqual(t, plain, withUnknown)
}

// TestOfferIDSkipsFieldsOutsideTheOfferRange asserts an invoice request
// mirroring an offer hashes to that offer's id, which is the lookup a receiver
// performs. The filter drops the payer's own fields, so a signature or an
// invreq field cannot move the id.
func TestOfferIDSkipsFieldsOutsideTheOfferRange(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "Minimal bolt12 offer")
	_, tlvBytes, err := Decode(vec.Bolt12)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	want, err := OfferID(offer)
	require.NoError(t, err)

	priv, pub := bobKey()
	ir, err := NewInvoiceRequestFromOffer(
		offer, pub, []byte("meta"), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType82, TUint64](TUint64(1000)),
	)
	_, err = SignInvoiceRequest(ir, priv)
	require.NoError(t, err)

	got, err := OfferID(ir)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
