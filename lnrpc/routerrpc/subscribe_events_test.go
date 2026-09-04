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

	testCases := []struct {
		name     string
		failure  invoices.FailResolutionResult
		expected FailureDetail
	}{
		{
			name:     "invoice interceptor",
			failure:  invoices.ResultInvoiceInterceptorError,
			expected: FailureDetail_INVOICE_INTERCEPTOR_ERROR,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			failureDetail, err := rpcFailureResolution(
				testCase.failure,
			)
			require.NoError(t, err)
			require.Equal(t, testCase.expected, failureDetail)
		})
	}
}
