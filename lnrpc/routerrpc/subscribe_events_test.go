package routerrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
)

// TestRPCInvoiceProcessingFailureResolution tests that invoice processing
// failures map to their corresponding RPC failure detail.
func TestRPCInvoiceProcessingFailureResolution(t *testing.T) {
	t.Parallel()

	failureDetail, err := rpcFailureResolution(
		invoices.ResultInvoiceInterceptorError,
	)
	require.NoError(t, err)
	require.Equal(t, FailureDetail_INVOICE_INTERCEPTOR_ERROR, failureDetail)
}
