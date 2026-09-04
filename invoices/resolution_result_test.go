package invoices

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInvoiceProcessingFailResolutionResults tests the metadata of invoice
// processing failure results.
func TestInvoiceProcessingFailResolutionResults(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		result   FailResolutionResult
		expected string
	}{
		{
			name:     "invoice interceptor",
			result:   ResultInvoiceInterceptorError,
			expected: "invoice interceptor failed",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, testCase.expected,
				testCase.result.FailureString(),
			)
			require.Equal(
				t, testCase.expected, testCase.result.String(),
			)
			require.False(t, testCase.result.IsSetFailure())
		})
	}
}
