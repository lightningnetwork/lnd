package invoicesrpc

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
)

// TestCreateRPCInvoiceWithoutPaymentSecret checks that a stored invoice
// without a payment secret, as created by lnd versions before v0.9.0, can
// still be converted to its RPC representation.
func TestCreateRPCInvoiceWithoutPaymentSecret(t *testing.T) {
	t.Parallel()

	payReq := "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5" +
		"rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7en" +
		"xv4jszjlrkes0mgx4ghum45ha5gkzlac8xmrr4skgwey" +
		"p6xqxucu4wz4j8uvpg0jznsesezax6hdt0gtyn3tuqpf" +
		"y2curryn83zygkydmpxcqdfu7k0"

	invoice := &invoices.Invoice{
		PaymentRequest: []byte(payReq),
		State:          invoices.ContractOpen,
	}

	rpcInvoice, err := CreateRPCInvoice(invoice, &chaincfg.MainNetParams)
	require.NoError(t, err)
	require.Equal(t, payReq, rpcInvoice.PaymentRequest)
}
