package invoices

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInvoiceProcessingFailResolutionResults tests the metadata of invoice
// processing failure results.
func TestInvoiceProcessingFailResolutionResults(t *testing.T) {
	t.Parallel()

	result := ResultInvoiceInterceptorError
	const expected = "invoice interceptor failed"
	require.Equal(t, expected, result.FailureString())
	require.Equal(t, expected, result.String())
	require.False(t, result.IsSetFailure())
}
