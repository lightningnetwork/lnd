package invoicesrpc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestCreateRPCInvoiceBolt12 verifies that a BOLT 12 invoice converts to an
// RPC invoice. Its lni1 payment request is not a BOLT 11 string, so the
// conversion must take the payment hash from the preimage.
func TestCreateRPCInvoiceBolt12(t *testing.T) {
	t.Parallel()

	const payReq = "lni1qqgsqvgnwgcg35z6ee2h3yczraddm72xrfua9uve2rlrm9" +
		"deu7xyfzr"

	// The conversion would fail if it decoded the string as BOLT 11.
	_, err := zpay32.Decode(payReq, &chaincfg.MainNetParams)
	require.Error(t, err)

	var preimage lntypes.Preimage
	preimage[0] = 0x12

	invoice := &invoices.Invoice{
		PaymentRequest: []byte(payReq),
		IsBolt12:       true,
		Terms: invoices.ContractTerm{
			PaymentPreimage: &preimage,
			Value:           lnwire.MilliSatoshi(10000),
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.MPPOptional,
				), lnwire.Features,
			),
		},
	}

	rpcInvoice, err := CreateRPCInvoice(invoice, &chaincfg.MainNetParams)
	require.NoError(t, err)

	hash := preimage.Hash()
	require.Equal(t, hash[:], rpcInvoice.RHash)
	require.Equal(t, payReq, rpcInvoice.PaymentRequest)
	require.EqualValues(t, 10000, rpcInvoice.ValueMsat)
}
