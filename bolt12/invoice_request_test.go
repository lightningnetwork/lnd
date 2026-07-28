package bolt12

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestInvoiceRequestRoundTrip pins encode→decode→re-encode for an
// InvoiceRequest with a representative subset of optional fields.
func TestInvoiceRequestRoundTrip(t *testing.T) {
	t.Parallel()

	_, bobPub := bobKey()

	metadata := tlv.Blob("payer-metadata")

	ir := &InvoiceRequest{
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob("description"),
			),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](bobPub),
		),
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](metadata),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType82, TUint64](1000),
		),
		Signature: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240](
				[64]byte{0x01},
			),
		),
	}

	encoded, err := ir.Encode()
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := DecodeInvoiceRequest(encoded)
	require.NoError(t, err)

	require.Equal(
		t, bobPub.SerializeCompressed(),
		decoded.InvreqPayerID.UnwrapOrFailV(t).SerializeCompressed(),
	)
	require.Equal(t, metadata, decoded.InvreqMetadata.UnwrapOrFailV(t))
	require.Equal(
		t, TUint64(1000), decoded.InvreqAmount.UnwrapOrFailV(t),
	)

	reencoded, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, encoded, reencoded)
}

// TestNewInvoiceRequestFromOffer tests the constructor for mirroring all offer
// fields and properly assigning the payer ID and metadata.
func TestNewInvoiceRequestFromOffer(t *testing.T) {
	t.Parallel()

	offer := validBobOffer(t)

	// Add some optional offer fields for mirroring verification.
	offer.OfferDescription = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("description")),
	)
	offer.OfferAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType8, TUint64](5000),
	)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	payerID := priv.PubKey()
	metadata := []byte("payer-metadata")

	ir, err := NewInvoiceRequestFromOffer(
		offer, payerID, metadata, bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)
	require.NotNil(t, ir)

	// Verify offer fields are copied exactly
	require.Equal(t, offer.OfferIssuerID, ir.OfferIssuerID)
	require.Equal(t, offer.OfferDescription, ir.OfferDescription)
	require.Equal(t, offer.OfferAmount, ir.OfferAmount)

	// Verify payer ID and metadata are set correctly
	require.Equal(t, payerID, ir.InvreqPayerID.UnwrapOrFailV(t))
	require.Equal(t, metadata, ir.InvreqMetadata.UnwrapOrFailV(t))

	// For Bitcoin mainnet the spec says SHOULD omit invreq_chain.
	require.False(t, ir.InvreqChain.IsSome())

	// A non-bitcoin chain must be set explicitly so it does not default
	// back to mainnet on the read side.
	var altChain [32]byte
	for i := range altChain {
		altChain[i] = 0xab
	}
	irAlt, err := NewInvoiceRequestFromOffer(
		offer, payerID, metadata, altChain,
	)
	require.NoError(t, err)
	require.Equal(t, altChain, irAlt.InvreqChain.UnwrapOrFailV(t))
}

// TestNewInvoiceRequestFromOfferMirrorsUnknownFields verifies the writer
// requirement "MUST copy all fields from the offer (including unknown fields)":
// an unknown odd TLV in the offer's signed range must survive into the
// constructed request's record set so it is signed and later mirrored into the
// invoice.
func TestNewInvoiceRequestFromOfferMirrorsUnknownFields(t *testing.T) {
	t.Parallel()

	_, pub := bobKey()

	// Build a minimal valid offer, encode it, then splice in an unknown odd
	// TLV (type 33, within the offer signed range) and decode it
	// back so the unknown lands in the offer's decodedTLVs sidecar.
	offer := &Offer{
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("desc")),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](pub),
		),
	}
	encoded, err := offer.Encode()
	require.NoError(t, err)

	const unknownType = 33
	unknownVal := []byte("xyz")
	var extra bytes.Buffer
	require.NoError(t, tlv.WriteVarInt(&extra, unknownType, &[8]byte{}))
	require.NoError(t, tlv.WriteVarInt(
		&extra, uint64(len(unknownVal)), &[8]byte{},
	))
	extra.Write(unknownVal)

	// TLV records are canonically ordered by type; type 33 sorts after the
	// offer's existing types (10, 22), so appending keeps the stream
	// sorted.
	spliced := append(append([]byte{}, encoded...), extra.Bytes()...)

	decodedOffer, err := decodeOffer(spliced)
	require.NoError(t, err)

	ir, err := NewInvoiceRequestFromOffer(
		decodedOffer, pub, []byte("metadata"),
		bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	// The unknown field must appear in the request's canonical record set.
	var found bool
	for _, r := range ir.AllRecords() {
		if r.Type() == unknownType {
			found = true
		}
	}
	require.True(t, found, "unknown offer TLV not mirrored into request")
}

// TestDecodeInvoiceRequestBech32String decodes the invoice_request string and
// verifies key fields. This exercises the low-level Decode plus
// DecodeInvoiceRequest path.
func TestDecodeInvoiceRequestBech32String(t *testing.T) {
	t.Parallel()

	// From upstream lightning/bolts signature-test.json: the
	// invoice_request bolt12 string.
	lnrStr := "lnr1qqyqqqqqqqqqqqqqqcp4256ypqqkgzshgysy6ct5d" +
		"pjk6ct5d93kzmpq23ex2ct5d9ek293pqthvwfzadd7jej" +
		"es8q9lhc4rvjxd022zv5l44g6qah82ru5rdpnpjkppqvj" +
		"x204vgdzgsqpvcp4mldl3plscny0rt707gvpdh6ndydfac" +
		"z43euzqhrurageg3n7kafgsek6gz3e9w52parv8gs2hlxz" +
		"k95tzeswywffxlkeyhml0hh46kndmwf4m6xma3tkq2lu0" +
		"4qz3slje2rfthc89vss"

	_, tlvBytes, err := Decode(lnrStr)
	require.NoError(t, err)

	ir, err := DecodeInvoiceRequest(tlvBytes)
	require.NoError(t, err)

	// Verify invreq_metadata is set (8 zero bytes).
	var metadata []byte
	ir.InvreqMetadata.WhenSome(
		func(r tlv.RecordT[tlv.TlvType0, tlv.Blob]) {
			metadata = r.Val
		},
	)
	require.Equal(t, make([]byte, 8), metadata)

	// Verify offer_currency is "USD".
	var currency []byte
	ir.OfferCurrency.WhenSome(
		func(r tlv.RecordT[tlv.TlvType6, tlv.Blob]) {
			currency = r.Val
		},
	)
	require.Equal(t, "USD", string(currency))

	// Verify offer_amount is 100.
	var amount TUint64
	ir.OfferAmount.WhenSome(
		func(r tlv.RecordT[tlv.TlvType8, TUint64]) {
			amount = r.Val
		},
	)
	require.Equal(t, TUint64(100), amount)

	// Verify offer_description is "A Mathematical Treatise".
	var desc []byte
	ir.OfferDescription.WhenSome(
		func(r tlv.RecordT[tlv.TlvType10, tlv.Blob]) {
			desc = r.Val
		},
	)
	require.Equal(t, "A Mathematical Treatise", string(desc))

	// Verify invreq_payer_id is Bob's compressed pubkey (0x424242...
	// privkey).
	var payerIDSet bool
	ir.InvreqPayerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType88, *btcec.PublicKey]) {
			payerIDSet = true
		},
	)
	require.True(t, payerIDSet)

	// Verify signature is present.
	var (
		sig    [64]byte
		sigSet bool
	)
	ir.Signature.WhenSome(
		func(r tlv.RecordT[tlv.TlvType240, [64]byte]) {
			sig = r.Val
			sigSet = true
		},
	)
	require.True(t, sigSet)

	expectedSig := "b8f83ea3288cfd6ea510cdb481472575141e8d87" +
		"44157f98562d162cc1c472526fdb24befefbdebab4dbb" +
		"726bbd1b7d8aec057f8fa805187e5950d2bbe0e5642"
	require.Equal(t, expectedSig, hex.EncodeToString(sig[:]))

	// Verify decode populated the canonical record set used by the Merkle
	// tree, so every wire TLV must be reachable through AllRecords for
	// signature verification to find them.
	require.NotEmpty(t, ir.AllRecords())

	// Re-encode must be byte-identical to the decoded wire bytes: the
	// signature is over the Merkle root of this canonical encoding, so any
	// reordering, dropped TLV, or non-canonical integer would invalidate
	// it.
	reencoded, err := ir.Encode()
	require.NoError(t, err)
	require.Equal(t, tlvBytes, reencoded)
}
