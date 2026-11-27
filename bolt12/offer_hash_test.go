package bolt12

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOfferHashMatchesOfferEncoding asserts that for an offer the offer hash is
// the hash of its whole encoding. Every offer TLV already sits in the hashed
// ranges, so the range filter is a no-op there.
func TestOfferHashMatchesOfferEncoding(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "Minimal bolt12 offer")
	_, tlvBytes, err := decodeBech32(vec.Bolt12)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	encoded, err := offer.encode()
	require.NoError(t, err)

	id, err := OfferHash(offer)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(encoded), id)
}

// TestOfferHashCoversUnknownOfferRangeTLVs asserts that an unknown TLV in the
// offer range changes the id. A caller that rebuilt the offer from its known
// fields instead would compute the same id for both requests and accept a
// request whose offer fields differ from the offer.
func TestOfferHashCoversUnknownOfferRangeTLVs(t *testing.T) {
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

	plain, err := OfferHash(request(false))
	require.NoError(t, err)

	withUnknown, err := OfferHash(request(true))
	require.NoError(t, err)

	require.NotEqual(t, plain, withUnknown)
}

// TestOfferHashSkipsFieldsOutsideTheOfferRange asserts an invoice request
// mirroring an offer hashes to that offer's id, which is the lookup a receiver
// performs. The filter drops the payer's own fields, so a signature or an
// invreq field cannot move the id.
func TestOfferHashSkipsFieldsOutsideTheOfferRange(t *testing.T) {
	t.Parallel()

	vec := findTestVector(t, "Minimal bolt12 offer")
	_, tlvBytes, err := decodeBech32(vec.Bolt12)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	want, err := OfferHash(offer)
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

	got, err := OfferHash(ir)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestOfferHashMatchesCoreLightning pins the offer hash against the offer Core
// Lightning publishes in its own RPC schema, together with the offer_id it
// reports for it.
func TestOfferHashMatchesCoreLightning(t *testing.T) {
	t.Parallel()

	// Offer and expected offer_id taken from Core Lightning's
	// doc/schemas/offer.json.
	const (
		clnOffer = "lno1qgsqvgnwgcg35z6ee2h3yczraddm72xrfua9uve2rlr" +
			"m9deu7xyfzrcgqgn3qzs2ge5hx6pqwdskcefpzcssxwz9sqk" +
			"jtd8qwnx06lxckvu6g8w8t0ue0zsrfqqygj636s4sw7v6"

		clnOfferHash = "c5cde0292d56941940f8b10a4c9bdd1f8846d6041a61b" +
			"3f7e0f87e105aa88121"
	)

	_, tlvBytes, err := decodeBech32(clnOffer)
	require.NoError(t, err)

	offer, err := decodeOffer(tlvBytes)
	require.NoError(t, err)

	id, err := OfferHash(offer)
	require.NoError(t, err)

	require.Equal(t, clnOfferHash, hex.EncodeToString(id[:]))
}

// testPaths returns one blinded path with one hop, for the offer_paths and
// invreq_paths records of a test message.
func testPaths() lnwire.BlindedPaths {
	_, intro := aliceKey()
	_, blinding := bobKey()

	return lnwire.BlindedPaths{
		Paths: []lnwire.BlindedPath{{
			IntroductionNode: lnwire.PubkeyIntro{Pubkey: intro},
			BlindingPoint:    blinding,
			Hops: []lnwire.BlindedHop{{
				BlindedNodeID: intro,
				EncryptedData: []byte{1, 2, 3},
			}},
		}},
	}
}

// TestOfferHashWithoutPaths verifies that the hash ignores offer_paths. This
// lets a node bind the paths of an offer to a value that exists before them.
// It also verifies that a request over such a path gives the same value.
func TestOfferHashWithoutPaths(t *testing.T) {
	t.Parallel()

	_, issuer := bobKey()
	_, payer := aliceKey()

	offer := &Offer{}
	offer.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10, []byte]([]byte("coffee")),
	)
	offer.OfferIssuerID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType22](issuer),
	)

	before, err := OfferHashWithoutPaths(offer)
	require.NoError(t, err)

	offer.OfferPaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType16](testPaths()),
	)

	after, err := OfferHashWithoutPaths(offer)
	require.NoError(t, err)
	require.Equal(t, before, after)

	full, err := OfferHash(offer)
	require.NoError(t, err)
	require.NotEqual(t, full, after)

	ir, err := NewInvoiceRequestFromOffer(
		offer, payer, []byte("meta"), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	fromRequest, err := OfferHashWithoutPaths(ir)
	require.NoError(t, err)
	require.Equal(t, after, fromRequest)
}

// TestInvoiceRequestHash verifies that an invoice hashes to the hash of the
// request it answers, that invreq_paths do not count, and that a field in the
// request range does.
func TestInvoiceRequestHash(t *testing.T) {
	t.Parallel()

	_, issuer := bobKey()
	_, payer := aliceKey()

	offer := &Offer{}
	offer.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10, []byte]([]byte("coffee")),
	)
	offer.OfferIssuerID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType22](issuer),
	)

	newRequest := func(metadata string) *InvoiceRequest {
		ir, err := NewInvoiceRequestFromOffer(
			offer, payer, []byte(metadata),
			bitcoinMainnetGenesisHash,
		)
		require.NoError(t, err)

		return ir
	}

	ir := newRequest("meta")
	reqHash, err := InvoiceRequestHash(ir)
	require.NoError(t, err)

	invHash, err := InvoiceRequestHash(NewInvoiceFromRequest(ir))
	require.NoError(t, err)
	require.Equal(t, reqHash, invHash)

	ir.InvreqPaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType90](testPaths()),
	)
	withPaths, err := InvoiceRequestHash(ir)
	require.NoError(t, err)
	require.Equal(t, reqHash, withPaths)

	other, err := InvoiceRequestHash(newRequest("other"))
	require.NoError(t, err)
	require.NotEqual(t, reqHash, other)
}
