package linters

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestTxScope(t *testing.T) {
	t.Parallel()

	analysistest.Run(
		t, analysistest.TestData(), newTxScopeAnalyzer(), "txscope",
	)
}
