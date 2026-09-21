package bolt12

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
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

// TestDecodeMinimalOfferString decodes a minimal offer string and verifies
// the issuer ID field is correctly parsed. This exercises the low-level
// Decode plus decodeOffer path. TestDecodeOfferString covers the
// DecodeOfferString wrapper.
func TestDecodeMinimalOfferString(t *testing.T) {
	t.Parallel()

	// Minimal offer: just offer_issuer_id (type 22).
	offerStr := "lno1zcss9mk8y3wkklfvevcrszlmu23kfrxh49p" +
		"x20665dqwmn4p72pksese"

	_, tlvBytes, err := Decode(offerStr)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	// Verify issuer ID is present and correctly typed.
	var (
		issuerKey *btcec.PublicKey
		set       bool
	)
	offer.OfferIssuerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]) {
			issuerKey = r.Val
			set = true
		},
	)
	require.True(t, set, "expected offer_issuer_id to be set")

	expectedHex := "02eec7245d6b7d2ccb30380bfbe2a3648cd7a94" +
		"2653f5aa340edcea1f283686619"
	require.Equal(t, expectedHex,
		hex.EncodeToString(issuerKey.SerializeCompressed()))

	// Re-encode and verify bytes match.
	reencoded, err := offer.Encode()
	require.NoError(t, err)
	require.Equal(t, tlvBytes, reencoded)
}

// TestDecodeOfferString decodes a spec test vector through the bech32
// wrapper, reader gates included.
func TestDecodeOfferString(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "with description (but no amount)")

	offer, err := DecodeOfferString(
		vec.Bolt12, farFutureNow(), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	var desc []byte
	offer.OfferDescription.WhenSome(
		func(r tlv.RecordT[tlv.TlvType10, tlv.Blob]) {
			desc = r.Val
		},
	)
	require.Equal(t, "Test vectors", string(desc))
}

// TestDecodeOfferStringInvalid asserts the wrapper rejects a string that
// fails at each layer: HRP discrimination and the reader MUST gates.
func TestDecodeOfferStringInvalid(t *testing.T) {
	t.Parallel()

	// An lnr string from signature-test.json exercises HRP
	// discrimination.
	lnrStr := "lnr1qqyqqqqqqqqqqqqqqcp4256ypqqkgzshgysy6ct5d" +
		"pjk6ct5d93kzmpq23ex2ct5d9ek293pqthvwfzadd7jej" +
		"es8q9lhc4rvjxd022zv5l44g6qah82ru5rdpnpjkppqvj" +
		"x204vgdzgsqpvcp4mldl3plscny0rt707gvpdh6ndydfac" +
		"z43euzqhrurageg3n7kafgsek6gz3e9w52parv8gs2hlxz" +
		"k95tzeswywffxlkeyhml0hh46kndmwf4m6xma3tkq2lu0" +
		"4qz3slje2rfthc89vss"

	missingIssuer := findTestVector(
		t, "Missing offer_issuer_id and no offer_path",
	)

	tests := []struct {
		name        string
		offer       string
		errContains string
	}{
		{
			name:        "wrong HRP",
			offer:       lnrStr,
			errContains: "expected HRP",
		},
		{
			name:        "reader gate failure",
			offer:       missingIssuer.Bolt12,
			errContains: ErrNoIssuerIdentity.Error(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeOfferString(
				tc.offer, farFutureNow(),
				bitcoinMainnetGenesisHash,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// TestOfferStringRoundTrip pins the encode→decode identity of the bech32
// wrapper pair on a spec vector.
func TestOfferStringRoundTrip(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "Minimal bolt12 offer")

	offer, err := DecodeOfferString(
		vec.Bolt12, farFutureNow(), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	encoded, err := EncodeOfferString(offer)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	offer2, err := DecodeOfferString(
		encoded, farFutureNow(), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	var id1, id2 *btcec.PublicKey
	offer.OfferIssuerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]) {
			id1 = r.Val
		},
	)
	offer2.OfferIssuerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]) {
			id2 = r.Val
		},
	)
	require.Equal(
		t, hex.EncodeToString(id1.SerializeCompressed()),
		hex.EncodeToString(id2.SerializeCompressed()),
	)
}
