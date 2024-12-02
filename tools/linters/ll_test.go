package linters

import (
	"os"
	"regexp"
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
		logRegex      string
		expectedIssue []string
	}{
		{
			name: "Single long line",
			content: `
				fmt.Println("This is a very long line that exceeds the maximum length and should be flagged by the linter.")`,
			logRegex: defaultLogRegex,
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
			logRegex: defaultLogRegex,
			expectedIssue: []string{
				"the line is 140 characters long, which " +
					"exceeds the maximum of 80 characters.",
				"the line is 148 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
		{
			name:     "Short lines",
			logRegex: defaultLogRegex,
			content: `
				fmt.Println("Short line")`,
		},
		{
			name:     "Directive ignored",
			logRegex: defaultLogRegex,
			content:  `//go:generate something very very very very very very very very very long and complex here wowowow`,
		},
		{
			name:     "Long single line import",
			logRegex: defaultLogRegex,
			content:  `import "github.com/lightningnetwork/lnd/lnrpc/walletrpc/more/more/more/more/more/more/ok/that/is/enough"`,
		},
		{
			name:     "Multi-line import",
			logRegex: defaultLogRegex,
			content: `
			import (
				"os"
				"fmt"
				"github.com/lightningnetwork/lnd/lnrpc/walletrpc/more/ok/that/is/enough"
			)`,
		},
		{
			name:     "Long single line log",
			logRegex: defaultLogRegex,
			content: `
			log.Infof("This is a very long log line but since it is a log line, it should be skipped by the linter."),
			rpcLog.Info("Another long log line with a slightly different name and should still be skipped")`,
		},
		{
			name:     "Long single line log followed by a non-log line",
			logRegex: defaultLogRegex,
			content: `
				log.Infof("This is a very long log line but since it is a log line, it should be skipped by the linter.")
				fmt.Println("This is a very long line that exceeds the maximum length and should be flagged by the linter.")`,
			expectedIssue: []string{
				"the line is 140 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
		{
			name:     "Multi-line log",
			logRegex: defaultLogRegex,
			content: `
				log.Infof("This is a very long log line but 
						since it is a log line, it 
						should be skipped by the linter.")`,
		},
		{
			name:     "Multi-line log followed by a non-log line",
			logRegex: defaultLogRegex,
			content: `
				log.Infof("This is a very long log line but 
						since it is a log line, it 
						should be skipped by the linter.")
				fmt.Println("This is a very long line that 
						exceeds the maximum length and 
						should be flagged by the linter.")`,
			expectedIssue: []string{
				"the line is 82 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
		{
			name:     "Only skip 'S' logs",
			logRegex: `^\s*.*(L|l)og\.(Info|Debug|Trace|Warn|Error|Critical)S\(`,
			content: `
				log.Infof("A long log line but it is not an S log and so should be caught")
				log.InfoS("This is a very long log line but 
						since it is an 'S' log line, it 
						should be skipped by the linter.")
				log.TraceS("Another S log that should be skipped by the linter")`,
			expectedIssue: []string{
				"the line is 107 characters long, which " +
					"exceeds the maximum of 80 characters.",
			},
		},
	}

	tabSpaces := strings.Repeat(" ", defaultTabWidthInSpaces)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logRegex := regexp.MustCompile(tc.logRegex)

			// Write content to a temporary file.
			tmpFile := t.TempDir() + "/test.go"
			err := os.WriteFile(tmpFile, []byte(tc.content), 0644)
			require.NoError(t, err)

			// Run the linter on the file.
			issues, err := getLLLIssuesForFile(
				tmpFile, defaultMaxLineLen, tabSpaces, logRegex,
			)
			require.NoError(t, err)

			require.Len(t, issues, len(tc.expectedIssue))

			for i, issue := range issues {
				require.Equal(
					t, tc.expectedIssue[i], issue.text,
				)
			}
		})
	}
}
