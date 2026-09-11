package bolt12

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// validInvoice returns an Invoice populated with the minimum set of fields
// required to satisfy validateInvoiceWrite.
func validInvoice(t testing.TB) *Invoice {
	t.Helper()

	_, pub := bobKey()

	var payHash [32]byte
	for i := range payHash {
		payHash[i] = byte(i)
	}

	_, intro := aliceKey()
	_, blinding := bobKey()
	_, hopPub := aliceKey()

	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	return &Invoice{
		InvoiceCreatedAt: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType164, TUint64](
				TUint64(1234567890),
			),
		),
		InvoiceAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType170, TUint64](
				TUint64(100_000),
			),
		),
		InvoicePaymentHash: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType168, [32]byte](
				payHash,
			),
		),
		InvoiceNodeID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType176](pub),
		),
		InvoicePaths: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160, lnwire.BlindedPaths](
				lnwire.BlindedPaths{
					Paths: []lnwire.BlindedPath{{
						IntroductionNode: introNode,
						BlindingPoint:    blinding,
						Hops: []lnwire.BlindedHop{{
							BlindedNodeID: hopPub,
						}},
					}},
				},
			),
		),
		InvoiceBlindedPay: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162, BlindedPayInfos](
				BlindedPayInfos{Infos: []BlindedPayInfo{{}}},
			),
		),
	}
}

// TestUsableFallbackAddresses pins the BOLT 12 ignore semantics for
// invoice_fallbacks.
func TestUsableFallbackAddresses(t *testing.T) {
	t.Parallel()

	addrs := []FallbackAddress{
		// Valid: version 0, 2 bytes.
		{Version: 0, Address: []byte{0x01, 0x02}},
		// Invalid: version 17, 2 bytes. Version is not supported.
		{Version: 17, Address: []byte{0x01, 0x02}},
		// Invalid: version 0, 1 byte. Address is too short.
		{Version: 0, Address: []byte{0x01}},
		// Invalid: version 0, 41 bytes. Address is too long.
		{Version: 0, Address: make([]byte, 41)},
		// Valid: version 16, 40 bytes.
		{Version: 16, Address: make([]byte, 40)},
	}
	inv := &Invoice{
		InvoiceFallbacks: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType172, FallbackAddresses](
				FallbackAddresses{Addrs: addrs},
			),
		),
	}

	got := inv.usableFallbackAddresses()
	require.Len(t, got, 2)
	require.Equal(t, byte(0), got[0].Version)
	require.Equal(t, byte(16), got[1].Version)
	require.Len(t, got[1].Address, 40)
}

// TestUsablePaths pins the BOLT 12 reader filter that excludes any blinded path
// whose payinfo.features carries an unknown required (even) bit, and confirms
// each surviving entry is paired with its own payinfo by index.
func TestUsablePaths(t *testing.T) {
	t.Parallel()

	_, blinding := bobKey()
	_, hopPub := aliceKey()
	_, intro := aliceKey()
	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	// hop builds a minimal single-hop blinded path; two of these populate
	// invoice_paths so the by-index pairing with payinfos can be observed.
	hop := lnwire.BlindedPath{
		IntroductionNode: introNode,
		BlindingPoint:    blinding,
		Hops:             []lnwire.BlindedHop{{BlindedNodeID: hopPub}},
	}
	pathsRecord := func(n int) tlv.OptionalRecordT[
		tlv.TlvType160, lnwire.BlindedPaths,
	] {

		paths := make([]lnwire.BlindedPath, n)
		for i := range paths {
			paths[i] = hop
		}

		return tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160, lnwire.BlindedPaths](
				lnwire.BlindedPaths{Paths: paths},
			),
		)
	}
	payRecord := func(infos ...BlindedPayInfo) tlv.OptionalRecordT[
		tlv.TlvType162, BlindedPayInfos,
	] {

		return tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162, BlindedPayInfos](
				BlindedPayInfos{Infos: infos},
			),
		)
	}

	// The first payinfo carries an unknown required feature bit
	// (MPPRequired); the second is featureless.
	required := *lnwire.NewRawFeatureVector(lnwire.MPPRequired)
	inv := &Invoice{
		InvoicePaths: pathsRecord(2),
		InvoiceBlindedPay: payRecord(
			BlindedPayInfo{FeeBaseMsat: 1, Features: required},
			BlindedPayInfo{FeeBaseMsat: 2},
		),
	}

	// No known bits: the MPPRequired bit is unknown, so path 0 is
	// filtered out and only path 1 (fee_base 2) survives.
	got := inv.UsablePaths(nil)
	require.Len(t, got, 1)
	require.Equal(t, uint32(2), got[0].PayInfo.FeeBaseMsat)

	// Once the bit is known, both paths become usable and stay paired with
	// their own payinfo in order.
	known := map[lnwire.FeatureBit]string{lnwire.MPPRequired: "mpp"}
	got = inv.UsablePaths(known)
	require.Len(t, got, 2)
	require.Equal(t, uint32(1), got[0].PayInfo.FeeBaseMsat)
	require.Equal(t, uint32(2), got[1].PayInfo.FeeBaseMsat)

	// A length mismatch between paths and payinfos yields no usable paths
	// (rejected upstream by validateInvoiceRead).
	inv.InvoiceBlindedPay = payRecord(BlindedPayInfo{})
	require.Empty(t, inv.UsablePaths(known))
}

// TestInvoiceRoundTripPreservesAllTypes pins encode, decode and re-encode for
// an Invoice with every field set, plus an unknown odd TLV. Byte identity
// keeps the Merkle root stable, and comparing the decoded struct against the
// fixture catches a field wired into the encode path but not into the decode
// path, which byte identity alone cannot see.
func TestInvoiceRoundTripPreservesAllTypes(t *testing.T) {
	t.Parallel()

	priv, pub := bobKey()
	_, intro := aliceKey()
	_, blinding := bobKey()
	_, hopPub := aliceKey()

	introNode, err := lnwire.NewPubkeyIntro(intro)
	require.NoError(t, err)

	paths := lnwire.BlindedPaths{
		Paths: []lnwire.BlindedPath{
			{
				IntroductionNode: introNode,
				BlindingPoint:    blinding,
				Hops: []lnwire.BlindedHop{
					{
						BlindedNodeID: hopPub,
						EncryptedData: []byte{1, 2},
					},
				},
			},
		},
	}

	// name_len, name, domain_len, domain.
	bip353 := append(
		[]byte{3, 'b', 'o', 'b', 6}, []byte("ex.com")...,
	)

	features := *lnwire.NewRawFeatureVector(lnwire.MPPOptional)

	// The required fields come from the shared fixture, the rest are set
	// here so the round trip covers every field.
	inv := validInvoice(t)

	// The shared fixture leaves the hop payload and the payinfo feature
	// vector at their zero values, which decode as empty rather than nil,
	// so set both explicitly here.
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](paths),
	)
	payInfo := BlindedPayInfo{
		FeeBaseMsat:               1000,
		FeeProportionalMillionths: 10,
		CltvExpiryDelta:           80,
		HtlcMinimumMsat:           1,
		HtlcMaximumMsat:           100_000,
		Features:                  *lnwire.NewRawFeatureVector(),
	}
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType162](BlindedPayInfos{
			Infos: []BlindedPayInfo{payInfo},
		}),
	)
	inv.InvreqMetadata = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType0](tlv.Blob("payer-meta")),
	)
	inv.OfferChains = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
			Chains: [][32]byte{bitcoinMainnetGenesisHash},
		}),
	)
	inv.OfferMetadata = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType4](tlv.Blob("opaque")),
	)
	inv.OfferCurrency = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType6](tlv.Blob("USD")),
	)
	inv.OfferAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType8](TUint64(1500)),
	)
	inv.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("coffee")),
	)
	inv.OfferFeatures = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType12](features),
	)
	inv.OfferAbsoluteExpiry = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType14](TUint64(1 << 32)),
	)
	inv.OfferPaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType16](paths),
	)
	inv.OfferIssuer = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType18](tlv.Blob("alice")),
	)
	inv.OfferQuantityMax = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType20](TUint64(5)),
	)
	inv.OfferIssuerID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType22](pub),
	)
	inv.InvreqChain = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType80](
			bitcoinMainnetGenesisHash,
		),
	)
	inv.InvreqAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType82](TUint64(100_000)),
	)
	inv.InvreqFeatures = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType84](features),
	)
	inv.InvreqQuantity = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType86](TUint64(2)),
	)
	inv.InvreqPayerID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType88](pub),
	)
	inv.InvreqPayerNote = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType89](tlv.Blob("tip")),
	)
	inv.InvreqPaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType90](paths),
	)
	inv.InvreqBip353Name = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType91](bip353),
	)
	inv.InvoiceRelativeExp = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType166](TUint32(3600)),
	)
	inv.InvoiceFallbacks = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType172](FallbackAddresses{
			Addrs: []FallbackAddress{{
				Version: 1,
				Address: []byte{3, 4, 5},
			}},
		}),
	)
	inv.InvoiceFeatures = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType174](features),
	)

	// An unknown odd type in the signed range must survive the round trip
	// byte for byte, because the Merkle root covers it.
	inv.decodedTLVs = tlv.TypeMap{93: []byte{0xde, 0xad}}

	sig, err := SignInvoice(inv, priv)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)

	encoded, err := inv.Encode()
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := DecodeInvoice(encoded)
	require.NoError(t, err)

	err = validateInvoiceRead(
		decoded, bitcoinMainnetGenesisHash,
		InvoiceKnownFeatures{
			Invoice: Bolt12Features,
			Blinded: Bolt12Features,
		},
	)
	require.NoError(t, err)

	// The sidecar names every type seen on the wire, whereas the fixture
	// carries only the unknown one. Adopt the decoded view so the
	// comparison below covers the typed fields.
	require.Equal(t, []byte{0xde, 0xad}, decoded.decodedTLVs[93])
	inv.decodedTLVs = decoded.decodedTLVs
	require.Equal(t, inv, decoded)

	// Re-encode the decoded copy and confirm canonicality.
	reencoded, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, encoded, reencoded)
}

// TestDecodeInvoiceRejectsTruncated locks in that DecodeInvoice surfaces an
// error when fed a truncated TLV stream rather than returning a partial
// Invoice. A silent partial-decode would let validation see fields that weren't
// actually on the wire.
func TestDecodeInvoiceRejectsTruncated(t *testing.T) {
	t.Parallel()

	inv := validInvoice(t)
	encoded, err := inv.Encode()
	require.NoError(t, err)

	// Chop off the last byte. The truncation lands in the middle of the
	// final blinded_pay record's variable-length payload.
	truncated := encoded[:len(encoded)-1]

	_, err = DecodeInvoice(truncated)
	require.Error(t, err)
}

// TestNewInvoiceFromRequest verifies the constructor mirrors all non-signature
// invoice_request fields into the invoice, applies the invreq_amount ->
// invoice_amount writer rule, and does not copy the request's signature.
func TestNewInvoiceFromRequest(t *testing.T) {
	t.Parallel()

	_, bobPub := bobKey()
	_, alicePub := aliceKey()

	req := &InvoiceRequest{
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob("description"),
			),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](alicePub),
		),
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				tlv.Blob("payer-metadata"),
			),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](bobPub),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](2500),
		),
		Signature: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240]([64]byte{0x01}),
		),
	}

	inv := NewInvoiceFromRequest(req)
	require.NotNil(t, inv)

	// Non-signature request fields are mirrored exactly.
	require.Equal(t, req.OfferDescription, inv.OfferDescription)
	require.Equal(t, req.OfferIssuerID, inv.OfferIssuerID)
	require.Equal(t, req.InvreqMetadata, inv.InvreqMetadata)
	require.Equal(t, req.InvreqPayerID, inv.InvreqPayerID)
	require.Equal(t, req.InvreqAmount, inv.InvreqAmount)

	// invreq_amount is mirrored into invoice_amount per the writer rule.
	require.Equal(t, TUint64(2500), inv.InvoiceAmount.UnwrapOrFailV(t))

	// The request's signature is not copied. The invoice signs its own.
	require.True(t, inv.Signature.IsNone())
}

// TestNewInvoiceFromRequestMirrorsUnknownFields verifies the writer requirement
// "MUST copy all non-signature fields from the invoice request (including
// unknown fields)": an unknown odd TLV in the request's signed range must
// survive into the constructed invoice's canonical record set so it is signed.
func TestNewInvoiceFromRequestMirrorsUnknownFields(t *testing.T) {
	t.Parallel()

	_, bobPub := bobKey()

	// Build a minimal valid spontaneous request and encode it.
	req := &InvoiceRequest{
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("desc")),
		),
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](tlv.Blob("meta")),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](bobPub),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
		),
	}
	encoded, err := req.Encode()
	require.NoError(t, err)

	// Fill in an unknown odd TLV (type 93, within the invreq signed range
	// and above the request's existing types) so the spliced stream stays
	// canonically sorted and the unknown lands in the decoded request's
	// decodedTLVs sidecar.
	const unknownType = 93
	unknownVal := []byte("xyz")
	var extra bytes.Buffer
	require.NoError(t, tlv.WriteVarInt(&extra, unknownType, &[8]byte{}))
	require.NoError(t, tlv.WriteVarInt(
		&extra, uint64(len(unknownVal)), &[8]byte{},
	))
	extra.Write(unknownVal)

	spliced := append(append([]byte{}, encoded...), extra.Bytes()...)

	decodedReq, err := DecodeInvoiceRequest(spliced)
	require.NoError(t, err)

	inv := NewInvoiceFromRequest(decodedReq)

	// The unknown field must appear in the invoice's canonical record set
	// with its value preserved, not just its type.
	var (
		found  bool
		gotVal bytes.Buffer
	)
	for _, r := range inv.AllRecords() {
		if r.Type() != unknownType {
			continue
		}
		found = true
		require.NoError(t, r.Encode(&gotVal))
	}
	require.True(t, found, "unknown request TLV not mirrored into invoice")
	require.Equal(
		t, unknownVal, gotVal.Bytes(),
		"unknown request TLV value not preserved",
	)
}

// TestInvoiceEncodeValidationGate verifies that Encode runs
// validateInvoiceWrite and rejects invalid invoices.
func TestInvoiceEncodeValidationGate(t *testing.T) {
	t.Parallel()

	inv := validInvoice(t)
	inv.InvoiceCreatedAt = tlv.OptionalRecordT[
		tlv.TlvType164, TUint64,
	]{}

	_, err := inv.Encode()
	require.ErrorIs(t, err, ErrMissingCreatedAt)
}

// TestInvoiceStringRoundTrip pins the encode→decode identity of the lni
// wrapper pair: the recovered invoice must re-encode to the original TLV
// stream byte-for-byte. The invoice is signed first because the decode
// wrapper runs the reader gates, which reject unsigned invoices.
func TestInvoiceStringRoundTrip(t *testing.T) {
	t.Parallel()

	priv, _ := bobKey()
	inv := validInvoice(t)

	sig, err := SignInvoice(inv, priv)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	encoded, err := EncodeInvoiceString(inv)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	// validInvoice sets invoice_created_at to 1234567890, so a clock one
	// second later sits inside the default 7200s expiry window.
	decoded, err := DecodeInvoiceString(
		encoded, time.Unix(1234567890+1, 0), bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	originalBytes, err := inv.Encode()
	require.NoError(t, err)
	decodedBytes, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, originalBytes, decodedBytes)
}

// TestEncodeInvoiceStringInvalid asserts the wrapper refuses to emit an
// invoice that fails writer validation.
func TestEncodeInvoiceStringInvalid(t *testing.T) {
	t.Parallel()

	// A dummy signature passes the presence gate. Writer validation runs
	// before signature verification, so the writer-validation branch is
	// exercised.
	inv := validInvoice(t)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte]([64]byte{}),
	)
	inv.InvoicePaymentHash = tlv.OptionalRecordT[
		tlv.TlvType168, [32]byte,
	]{}

	encoded, err := EncodeInvoiceString(inv)
	require.ErrorIs(t, err, ErrMissingPaymentHash)
	require.Empty(t, encoded)
}

// TestEncodeInvoiceStringUnsigned asserts the wire-string layer refuses to
// emit an unsigned invoice: the signature becomes mandatory at the bech32
// boundary even though pre-sign Encode is permitted.
func TestEncodeInvoiceStringUnsigned(t *testing.T) {
	t.Parallel()

	inv := validInvoice(t)
	require.False(t, inv.Signature.IsSome())

	encoded, err := EncodeInvoiceString(inv)
	require.ErrorIs(t, err, ErrMissingSignature)
	require.Empty(t, encoded)
}

// TestEncodeInvoiceStringInvalidSignature asserts the wire-string layer
// refuses to emit an invoice whose signature does not verify against
// invoice_node_id.
func TestEncodeInvoiceStringInvalidSignature(t *testing.T) {
	t.Parallel()

	priv, _ := bobKey()
	inv := validInvoice(t)

	sig, err := SignInvoice(inv, priv)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	// The post-sign mutation leaves the signature stale: it covers a
	// Merkle root this invoice no longer produces.
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType170, TUint64](TUint64(200_000)),
	)

	encoded, err := EncodeInvoiceString(inv)
	require.ErrorIs(t, err, ErrInvalidSignature)
	require.Empty(t, encoded)
}

// TestDecodeInvoiceStringExpiry asserts the wrapper folds the expiry gate
// in: an invoice past invoice_created_at + relative expiry is rejected even
// though the structural reader checks pass.
func TestDecodeInvoiceStringExpiry(t *testing.T) {
	t.Parallel()

	priv, _ := bobKey()
	inv := validInvoice(t)

	sig, err := SignInvoice(inv, priv)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte](sig),
	)

	encoded, err := EncodeInvoiceString(inv)
	require.NoError(t, err)

	// validInvoice's invoice_created_at is 1234567890 with the default
	// 7200s expiry, so 8000s later is past the window.
	expired := time.Unix(1234567890+8000, 0)
	decoded, err := DecodeInvoiceString(
		encoded, expired, bitcoinMainnetGenesisHash,
	)
	require.ErrorIs(t, err, ErrInvoiceExpired)
	require.Nil(t, decoded)
}

// TestDecodeInvoiceStringInvalid asserts the lni wrapper rejects a string
// that fails HRP discrimination or bech32 decoding.
func TestDecodeInvoiceStringInvalid(t *testing.T) {
	t.Parallel()

	offerStr := findTestVector(t, "Minimal bolt12 offer").Bolt12

	tests := []struct {
		name        string
		invoice     string
		errContains string
	}{
		{
			name:        "wrong HRP",
			invoice:     offerStr,
			errContains: "expected HRP",
		},
		{
			name:        "malformed bech32",
			invoice:     "not a bech32 string",
			errContains: "bech32",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv, err := DecodeInvoiceString(
				tc.invoice, farFutureNow(),
				bitcoinMainnetGenesisHash,
			)
			require.Error(t, err)
			require.Nil(t, inv)
			require.Contains(t, err.Error(), tc.errContains)
		})
	}
}
