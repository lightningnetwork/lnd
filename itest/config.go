//go:build integration

package itest

import (
	"os/exec"

	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testDebuglevelShow tests that "lnd --debuglevel=show" command works and
// prints the list of supported subsystems.
func testDebuglevelShow(ht *lntest.HarnessTest) {
	// We can't use ht.NewNode, because it adds more arguments to the
	// command line (e.g. flags configuring bitcoin backend), but we want to
	// make sure that "lnd --debuglevel=show" works without any other flags.
	lndBinary := getLndBinary(ht.T)
	cmd := exec.Command(lndBinary, "--debuglevel=show")
	stdoutStderrBytes, err := cmd.CombinedOutput()
	require.NoError(ht, err, "failed to run 'lnd --debuglevel=show'")

	// Make sure that the output contains the list of supported subsystems
	// and that the list is not empty. We search PEER subsystem.
	stdoutStderr := string(stdoutStderrBytes)
	require.Contains(ht, stdoutStderr, "Supported subsystems")
	require.Contains(ht, stdoutStderr, "PEER")
}
