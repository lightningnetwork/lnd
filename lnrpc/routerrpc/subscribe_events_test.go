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

	tests := []struct {
		name     string
		result   invoices.FailResolutionResult
		expected FailureDetail
	}{
		{
			name:     "invoice expiry too soon",
			result:   invoices.ResultExpiryTooSoon,
			expected: FailureDetail_INVOICE_EXPIRY_TOO_SOON,
		},
		{
			name:     "htlc set expiry too soon",
			result:   invoices.ResultHtlcSetExpiryTooSoon,
			expected: FailureDetail_INVOICE_EXPIRY_TOO_SOON,
		},
		{
			name:     "invoice interceptor error",
			result:   invoices.ResultInvoiceInterceptorError,
			expected: FailureDetail_INVOICE_INTERCEPTOR_ERROR,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			failureDetail, err := rpcFailureResolution(test.result)
			require.NoError(t, err)
			require.Equal(t, test.expected, failureDetail)
		})
	}
}
