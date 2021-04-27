package itest

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// AddToNodeLog adds a line to the log file and asserts there's no error.
func AddToNodeLog(t *testing.T,
	node *lntest.HarnessNode, logLine string) {

	err := node.AddToLog(logLine)
	require.NoError(t, err, "unable to add to log")
}
