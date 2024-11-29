package linters

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGetLLLIssuesForFile tests the line-too-long linter.
//
//nolint:ll
func TestGetLLLIssuesForFile(t *testing.T) {
	// Test data
	testCases := []struct {
		name          string
		content       string
		expectedIssue []string
	}{
		{
			name: "Single long line",
			content: `
				fmt.Println("This is a very long line that exceeds the maximum length and should be flagged by the linter.")`,
			expectedIssue: []string{
				"the line is 140 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
		{
			name: "Multiple long lines",
			content: `
				fmt.Println("This is a very long line that exceeds the maximum length and should be flagged by the linter.")
				fmt.Println("This is a another very long line that exceeds the maximum length and should be flagged by the linter.")`,
			expectedIssue: []string{
				"the line is 140 characters long, which " +
					"exceeds the maximum of 80 characters.",
				"the line is 148 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
		{
			name: "Short lines",
			content: `
				fmt.Println("Short line")`,
		},
		{
			name:    "Directive ignored",
			content: `//go:generate something very very very very very very very very very long and complex here wowowow`,
		},
		{
			name:    "Long single line import",
			content: `import "github.com/lightningnetwork/lnd/lnrpc/walletrpc/more/more/more/more/more/more/ok/that/is/enough"`,
		},
		{
			name: "Multi-line import",
			content: `
			import (
				"os"
				"fmt"
				"github.com/lightningnetwork/lnd/lnrpc/walletrpc/more/ok/that/is/enough"
			)`,
		},
		{
			name: "Long single line log",
			content: `
		log.Infof("This is a very long log line but since it is a log line, it should be skipped by the linter.")`,
			expectedIssue: []string{
				"the line is 121 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
	}

	tabSpaces := strings.Repeat(" ", 8)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Write content to a temporary file.
			tmpFile := t.TempDir() + "/test.go"
			err := os.WriteFile(tmpFile, []byte(tc.content), 0644)
			require.NoError(t, err)

			// Run the linter on the file.
			issues, err := getLLLIssuesForFile(
				tmpFile, 80, tabSpaces,
			)
			require.NoError(t, err)

			require.Len(t, issues, len(tc.expectedIssue))

			for i, issue := range issues {
				require.Equal(t, tc.expectedIssue[i], issue.Text)
			}
		})
	}
}
