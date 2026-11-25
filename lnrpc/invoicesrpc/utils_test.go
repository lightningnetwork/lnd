package invoicesrpc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestCreateRPCInvoiceLegacyDescription uses a signed payment request
// created by the previous encoder with a multiline description.
func TestCreateRPCInvoiceLegacyDescription(t *testing.T) {
	t.Parallel()

	const paymentRequest = "lnbc1pvjluezpp5" +
		"quqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq" +
		"qqqqqqqqqqqdqhd3jkwctr0y9xjmnkda5kxegdrgpr3ns2ve86upu3xj" +
		"z36e2cgn4nplrddldup6sexfncx740hnqepaf5s8hl6vj7w5lzhc5y0g" +
		"ehzmdv0tk487cekp6gxv52s4sg3cp8n0duu"
	const memo = "legacy\ninvoice"

	net := &chaincfg.MainNetParams
	_, err := zpay32.Decode(paymentRequest, net)
	require.ErrorIs(t, err, zpay32.ErrInvalidUTF8Description)

	stored := &invoices.Invoice{
		Memo:           []byte(memo),
		PaymentRequest: []byte(paymentRequest),
		Terms: invoices.ContractTerm{
			Features: lnwire.NewFeatureVector(nil, lnwire.Features),
		},
	}

	rpcInvoice, err := CreateRPCInvoice(stored, net)
	require.NoError(t, err)
	require.Equal(t, paymentRequest, rpcInvoice.PaymentRequest)
	require.Equal(t, memo, rpcInvoice.Memo)
	hash := [32]byte{7}
	require.Equal(t, hash[:], rpcInvoice.RHash)
}
