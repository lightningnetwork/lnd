package bolt12

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestDecodeInvoiceStringUnvalidated asserts the display entry point skips the
// reader gates but still pins the prefix, so an offer string cannot be read
// back as an invoice.
func TestDecodeInvoiceStringUnvalidated(t *testing.T) {
	t.Parallel()

	// An invoice that the reader would reject, here for a chain the node
	// does not accept, still decodes for display.
	inv := validInvoice(t)

	var altChain [32]byte
	for i := range altChain {
		altChain[i] = 0xaa
	}
	inv.InvreqChain = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType80](altChain),
	)

	priv, _ := bobKey()
	sig, err := SignInvoice(inv, priv)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)

	encoded, err := EncodeInvoiceString(inv)
	require.NoError(t, err)

	_, err = DecodeInvoiceString(
		encoded, farFutureNow(), bitcoinMainnetGenesisHash,
	)
	require.ErrorIs(t, err, ErrUnsupportedChain)

	decoded, err := DecodeInvoiceStringUnvalidated(encoded)
	require.NoError(t, err)
	require.Equal(t, altChain, decoded.InvreqChain.UnwrapOrFailV(t))

	// The prefix still has to be an invoice one.
	offerStr := findTestVector(t, "Minimal bolt12 offer").Bolt12
	_, err = DecodeInvoiceStringUnvalidated(offerStr)
	require.ErrorContains(t, err, "expected HRP")
}
