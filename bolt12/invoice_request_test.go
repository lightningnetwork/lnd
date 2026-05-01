package bolt12

import (
	"bytes"
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

// TestDecodeInvoiceRequestString decodes the signature-test.json
// invoice_request string through the bech32 wrapper, reader gates included,
// and verifies key fields.
func TestDecodeInvoiceRequestString(t *testing.T) {
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

	ir, err := DecodeInvoiceRequestString(lnrStr, bitcoinMainnetGenesisHash)
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
}

// TestInvoiceRequestStringRoundTrip pins the encode→decode identity of the
// lnr wrapper pair: the recovered request must re-encode to the original TLV
// stream byte-for-byte.
func TestInvoiceRequestStringRoundTrip(t *testing.T) {
	t.Parallel()

	ir := validInvoiceRequest(t)

	encoded, err := EncodeInvoiceRequestString(ir)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := DecodeInvoiceRequestString(
		encoded, bitcoinMainnetGenesisHash,
	)
	require.NoError(t, err)

	originalBytes, err := ir.Encode()
	require.NoError(t, err)
	decodedBytes, err := decoded.Encode()
	require.NoError(t, err)
	require.Equal(t, originalBytes, decodedBytes)
}

// TestEncodeInvoiceRequestStringInvalid asserts the wrapper refuses to emit
// a request that fails writer validation.
func TestEncodeInvoiceRequestStringInvalid(t *testing.T) {
	t.Parallel()

	ir := validInvoiceRequest(t)
	ir.InvreqPayerID = tlv.OptionalRecordT[
		tlv.TlvType88, *btcec.PublicKey,
	]{}

	encoded, err := EncodeInvoiceRequestString(ir)
	require.ErrorIs(t, err, ErrMissingPayerID)
	require.Empty(t, encoded)
}

// TestEncodeInvoiceRequestStringUnsigned asserts the wire-string layer
// refuses to emit an unsigned invoice request: the signature becomes
// mandatory at the bech32 boundary even though pre-sign Encode is permitted.
func TestEncodeInvoiceRequestStringUnsigned(t *testing.T) {
	t.Parallel()

	ir := validInvoiceRequest(t)
	ir.Signature = tlv.OptionalRecordT[tlv.TlvType240, [64]byte]{}

	encoded, err := EncodeInvoiceRequestString(ir)
	require.ErrorIs(t, err, ErrMissingSignature)
	require.Empty(t, encoded)
}

// TestEncodeInvoiceRequestStringInvalidSignature asserts the wire-string
// layer refuses to emit a request whose signature does not verify against
// invreq_payer_id.
func TestEncodeInvoiceRequestStringInvalidSignature(t *testing.T) {
	t.Parallel()

	ir := validInvoiceRequest(t)

	// The post-sign mutation leaves the signature stale: it covers a
	// Merkle root this request no longer produces.
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType82, TUint64](TUint64(2000)),
	)

	encoded, err := EncodeInvoiceRequestString(ir)
	require.ErrorIs(t, err, ErrInvalidSignature)
	require.Empty(t, encoded)
}
