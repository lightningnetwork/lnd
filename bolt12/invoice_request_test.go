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
