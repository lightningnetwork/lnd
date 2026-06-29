package bolt12

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// validInvoice returns an Invoice populated with the minimum set of fields
// required to satisfy ValidateInvoiceWrite.
func validInvoice(t *testing.T) *Invoice {
	t.Helper()

	_, pub := bobKey()
	var nodeID [33]byte
	copy(nodeID[:], pub.SerializeCompressed())

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
			tlv.NewPrimitiveRecord[tlv.TlvType176, [33]byte](
				nodeID,
			),
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

	got := inv.UsableFallbackAddresses()
	require.Len(t, got, 2)
	require.Equal(t, byte(0), got[0].Version)
	require.Equal(t, byte(16), got[1].Version)
	require.Len(t, got[1].Address, 40)
}

// TestInvoiceRoundTripPreservesAllTypes encodes a fully populated invoice then
// decodes it back, asserting every field is preserved byte-for-byte. The codec
// promises bijection on the message level, and any drift (dropped record,
// re-ordered output) breaks downstream signature verification because the
// Merkle root depends on the exact raw TLV stream.
func TestInvoiceRoundTripPreservesAllTypes(t *testing.T) {
	t.Parallel()

	inv := validInvoice(t)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240, [64]byte]([64]byte{}),
	)

	encoded, err := inv.Encode()
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := DecodeInvoice(encoded)
	require.NoError(t, err)

	// Re-encode the decoded copy and confirm canonicality.
	// decode(encode(decode(encode(x)))) must equal decode(encode(x)).
	encoded2, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, encoded, encoded2)
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
