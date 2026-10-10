package linters

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestDeferRecover verifies direct-defer acceptance and misuse diagnostics.
func TestDeferRecover(t *testing.T) {
	t.Parallel()

	analysistest.Run(
		t, analysistest.TestData(), newDeferRecoverAnalyzer(),
		"deferrecover",
	)
}
