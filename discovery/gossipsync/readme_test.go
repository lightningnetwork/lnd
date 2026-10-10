package gossipsync

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadmeTransitionTable asserts that the transition table in README.md
// matches SyncerTransitions, so the documentation can't drift from the code.
func TestReadmeTransitionTable(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)

	rendered := SyncerTransitions.RenderMarkdown()
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}

		require.Contains(t, string(readme), line+"\n",
			"README.md is missing a row of SyncerTransitions; "+
				"regenerate it with RenderMarkdown")
	}
}
