package bolt12

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOfferRoundTrip pins encode, decode and re-encode for an Offer with every
// field set, plus an unknown odd TLV in the offer range. Comparing the decoded
// struct against the original catches a field that is wired into the encode
// path but not into the decode path, which byte identity alone cannot see:
// such a field survives as an unknown TLV and re-encodes cleanly while its
// typed value silently disappears.
func TestOfferRoundTrip(t *testing.T) {
	t.Parallel()

	desc := tlv.Blob("coffee")
	issuer := tlv.Blob("alice")
	currency := tlv.Blob("USD")
	metadata := tlv.Blob("opaque")
	_, bobPub := bobKey()
	_, intro := aliceKey()
	_, blinding := bobKey()
	_, hopPub := aliceKey()

	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	o := &Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
				Chains: [][32]byte{bitcoinMainnetGenesisHash},
			}),
		),
		OfferMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](metadata),
		),
		OfferCurrency: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6](currency),
		),
		OfferAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8](TUint64(1500)),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](desc),
		),
		OfferFeatures: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](
				*lnwire.NewRawFeatureVector(lnwire.MPPOptional),
			),
		),
		OfferAbsoluteExpiry: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType14](TUint64(1 << 32)),
		),
		OfferPaths: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType16](lnwire.BlindedPaths{
				Paths: []lnwire.BlindedPath{{
					IntroductionNode: introNode,
					BlindingPoint:    blinding,
					Hops: []lnwire.BlindedHop{{
						BlindedNodeID: hopPub,
						EncryptedData: []byte{1, 2},
					}},
				}},
			}),
		),
		OfferIssuer: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType18](issuer),
		),
		OfferQuantityMax: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType20](TUint64(5)),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](bobPub),
		),

		// An unknown odd type in the offer range must survive the round
		// trip byte for byte, so that offer_id stays stable across
		// encoders that understand a wider set of extensions.
		decodedTLVs: tlv.TypeMap{13: []byte{0xde, 0xad}},
	}

	encoded, err := o.encode()
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := decodeOffer(encoded)
	require.NoError(t, err)

	// The sidecar is a decode artifact: it names every type seen on the
	// wire, whereas the fixture above only carries the unknown one. Adopt
	// the decoded view so the comparison below covers the typed fields.
	require.Equal(t, []byte{0xde, 0xad}, decoded.decodedTLVs[13])
	o.decodedTLVs = decoded.decodedTLVs
	require.Equal(t, o, decoded)

	reencoded, err := decoded.encode()
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

	_, tlvBytes, err := decodeBech32(offerStr)
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
	reencoded, err := offer.encode()
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

// TestOfferPaymentFlow simulates a payment for an offer, one step per party.
// The payee publishes an offer as an lno1 string, the payer answers it with a
// signed invoice request, and the payee replies with an invoice that the payer
// checks before it pays.
func TestOfferPaymentFlow(t *testing.T) {
	t.Parallel()

	payeeKey, payeePub := aliceKey()
	payerKey, payerPub := bobKey()
	now := time.Unix(1234567890, 0).Add(time.Minute)

	// Payee: create the offer and publish it as an lno1 string, for
	// example in a QR code. offer_issuer_id names the node that will sign
	// the invoice.
	offer := &Offer{
		OfferAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType8, TUint64](TUint64(1000)),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob("coffee"),
			),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](payeePub),
		),
	}
	lno, err := EncodeOfferString(offer)
	require.NoError(t, err)
	offerHash, err := OfferHash(offer)
	require.NoError(t, err)

	// Payer: read the offer and mirror its fields into a request under a
	// transient payer key.
	scanned, err := DecodeOfferString(lno, now, bitcoinMainnetGenesisHash)
	require.NoError(t, err)
	req, err := NewInvoiceRequestFromOffer(
		scanned, payerPub, []byte("unpredictable"),
		bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	// Payer: sign the request and send the raw TLV in an onion message to
	// the offer's node, with a reply path for the invoice.
	reqSig, err := SignInvoiceRequest(req, payerKey)
	require.NoError(t, err)
	req.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](reqSig),
	)
	reqWire, err := req.EncodeSigned()
	require.NoError(t, err)

	// Payee: read the request, gate it, and find the offer it answers. The
	// request mirrors the offer's fields, so it carries the same offer
	// hash.
	received, err := DecodeInvoiceRequest(reqWire)
	require.NoError(t, err)
	require.NoError(t, ValidateInvoiceRequestRead(
		received, bitcoinMainnetGenesisHash, Bolt12Features,
	))
	receivedHash, err := OfferHash(received)
	require.NoError(t, err)
	require.Equal(t, offerHash, receivedHash)

	// Payee: answer over the reply path with an invoice for the offer's
	// amount, signed with the key offer_issuer_id names.
	tmpl := validInvoice(t)
	inv := NewInvoiceFromRequest(received)
	inv.InvoiceCreatedAt = tmpl.InvoiceCreatedAt
	inv.InvoicePaymentHash = tmpl.InvoicePaymentHash
	inv.InvoicePaths = tmpl.InvoicePaths
	inv.InvoiceBlindedPay = tmpl.InvoiceBlindedPay
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType170](
			offer.OfferAmount.ValOpt().UnwrapOr(0),
		),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](payeePub),
	)
	invSig, err := SignInvoice(inv, payeeKey)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](invSig),
	)
	invWire, err := inv.EncodeSigned()
	require.NoError(t, err)

	// Payer: read the invoice and check it against its own request. The
	// node id to expect comes from the offer's offer_issuer_id.
	reply, err := DecodeInvoice(invWire)
	require.NoError(t, err)
	features := InvoiceKnownFeatures{
		Invoice: Bolt12Features,
		Blinded: Bolt12Features,
	}
	issuerID := scanned.OfferIssuerID.ValOpt().UnwrapOr(nil)
	require.NoError(t, ValidateInvoiceForPayment(
		reply, req, now, bitcoinMainnetGenesisHash, features, issuerID,
	))

	// Payer: pay over the invoice's blinded paths.
	require.NotEmpty(t, reply.UsablePaths(Bolt12Features))
}

// TestOfferlessPaymentFlow simulates a payment for an invoice request that
// answers no offer, one step per party. The payer publishes a signed request as
// an lnr1 string, the payee reads it and answers with an invoice, and the payer
// checks that invoice before it pays.
func TestOfferlessPaymentFlow(t *testing.T) {
	t.Parallel()

	payerKey, payerPub := bobKey()
	payeeKey, payeePub := aliceKey()

	// Payer: build the request. It carries no offer_issuer_id and no
	// offer_paths. The payer's own key and the amount it will pay take
	// the place of an offer.
	req := &InvoiceRequest{
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				tlv.Blob("unpredictable"),
			),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob("refund"),
			),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](TUint64(1000)),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](payerPub),
		),
	}

	// Payer: sign with the invreq_payer_id key and publish the request as
	// an lnr1 string, for example in a QR code.
	reqSig, err := SignInvoiceRequest(req, payerKey)
	require.NoError(t, err)
	req.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](reqSig),
	)
	lnr, err := EncodeInvoiceRequestString(req)
	require.NoError(t, err)

	// Payee: read the scanned string. The reader gates run here, so a
	// request without a valid signature stops at this step.
	scanned, err := DecodeInvoiceRequestString(
		lnr, bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	// Payee: answer with an invoice for the requested amount, signed with
	// its node key. It sends the encoded invoice in an onion message to
	// invreq_paths, or to invreq_payer_id when there are none.
	tmpl := validInvoice(t)
	inv := NewInvoiceFromRequest(scanned)
	inv.InvoiceCreatedAt = tmpl.InvoiceCreatedAt
	inv.InvoicePaymentHash = tmpl.InvoicePaymentHash
	inv.InvoicePaths = tmpl.InvoicePaths
	inv.InvoiceBlindedPay = tmpl.InvoiceBlindedPay
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType170](
			scanned.InvreqAmount.ValOpt().UnwrapOr(0),
		),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](payeePub),
	)
	invSig, err := SignInvoice(inv, payeeKey)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](invSig),
	)
	wire, err := inv.EncodeSigned()
	require.NoError(t, err)

	// Payer: read the invoice and check it against the request it
	// published. It confirmed no payee key out of band, so it passes nil.
	received, err := DecodeInvoice(wire)
	require.NoError(t, err)
	now := time.Unix(1234567890, 0).Add(time.Minute)
	features := InvoiceKnownFeatures{
		Invoice: Bolt12Features,
		Blinded: Bolt12Features,
	}
	require.NoError(t, ValidateInvoiceForPayment(
		received, req, now, bitcoinMainnetGenesisHash, features, nil,
	))

	// Payer: pay over the invoice's blinded paths.
	require.NotEmpty(t, received.UsablePaths(Bolt12Features))

	// A payer that did confirm a key out of band still rejects an invoice
	// that another node signed.
	require.ErrorIs(t, ValidateInvoiceForPayment(
		received, req, now, bitcoinMainnetGenesisHash, features,
		payerPub,
	), ErrUnexpectedInvoiceNodeID)
}
