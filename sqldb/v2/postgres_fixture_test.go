//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)) && !(netbsd || openbsd)

package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSanitizeDockerName verifies that invalid Docker name characters are
// normalized before the fixture creates a container.
func TestSanitizeDockerName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "slashes and spaces",
			input:    "TestParent/some case",
			expected: "TestParent_some_case",
		},
		{
			name:     "leading punctuation",
			input:    "---fixture",
			expected: "fixture",
		},
		{
			name:     "trailing punctuation",
			input:    "fixture---",
			expected: "fixture",
		},
		{
			name:     "empty fallback",
			input:    "   ",
			expected: "postgresql-container",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := sanitizeDockerName(testCase.input)
			require.Equal(t, testCase.expected, result)
		})
	}
}
