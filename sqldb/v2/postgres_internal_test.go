package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureRequiredSSLMode verifies that the Postgres DSN is upgraded to a
// TLS-enforcing sslmode when requested.
func TestEnsureRequiredSSLMode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		dsn        string
		requireSSL bool
		expected   string
	}{
		{
			name:       "ssl disabled",
			dsn:        "postgres://user:pass@localhost/db?sslmode=disable",
			requireSSL: true,
			expected:   "postgres://user:pass@localhost/db?sslmode=require",
		},
		{
			name:       "ssl not requested",
			dsn:        "postgres://user:pass@localhost/db?sslmode=disable",
			requireSSL: false,
			expected:   "postgres://user:pass@localhost/db?sslmode=disable",
		},
		{
			name:       "strict mode preserved",
			dsn:        "postgres://user:pass@localhost/db?sslmode=verify-full",
			requireSSL: true,
			expected:   "postgres://user:pass@localhost/db?sslmode=verify-full",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result, err := ensureRequiredSSLMode(
				testCase.dsn, testCase.requireSSL,
			)
			require.NoError(t, err)
			require.Equal(t, testCase.expected, result)
		})
	}
}
