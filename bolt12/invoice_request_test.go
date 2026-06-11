package bolt12

import (
	"testing"

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
