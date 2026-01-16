package itest

import (
	"time"

	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// walletSyncTestCases defines a set of tests for the wallet_synced field
// in GetInfoResponse.
var walletSyncTestCases = []*lntest.TestCase{
	{
		Name:     "wallet synced",
		TestFunc: runTestWalletSynced,
	},
}

// runTestWalletSynced tests that the wallet_synced field in GetInfoResponse
// correctly reflects the wallet's sync state. It verifies that wallet_synced
// is false while the wallet is catching up to new blocks, and becomes true
// once fully synced.
func runTestWalletSynced(ht *lntest.HarnessTest) {
	// Create a test node.
	alice := ht.NewNodeWithCoins("Alice", nil)

	// Verify wallet starts synced.
	resp := alice.RPC.GetInfo()
	require.True(ht, resp.WalletSynced)
	ht.Logf("Alice wallet_synced=%v", resp.WalletSynced)

	// Stop Alice to create a clear sync gap while we mine blocks.
	require.NoError(ht, alice.Stop(), "failed to stop Alice")

	// Mine blocks while Alice is offline.
	const numBlocks = 40
	ht.Miner().MineBlocks(numBlocks)
	_, minerHeight := ht.Miner().GetBestBlock()

	// Restart Alice without waiting for full chain sync.
	require.NoError(
		ht, alice.Start(ht.Context()), "failed to restart Alice",
	)

	// While Alice is behind the miner height, wallet_synced must be false.
	deadline := time.Now().Add(lntest.DefaultTimeout)
	for {
		resp := alice.RPC.GetInfo()
		if int32(resp.BlockHeight) >= minerHeight {
			break
		}

		require.Falsef(ht, resp.WalletSynced,
			"wallet_synced=true while behind "+
				"(nodeHeight=%v, minerHeight=%v)",
			resp.BlockHeight, minerHeight)

		if time.Now().After(deadline) {
			require.Fail(ht, "timed out waiting for "+
				"node to catch up")
		}

		time.Sleep(50 * time.Millisecond)
	}

	// Final verification that wallet_synced is true.
	require.Eventually(ht, func() bool {
		return alice.RPC.GetInfo().WalletSynced
	}, lntest.DefaultTimeout, 200*time.Millisecond,
		"wallet should be synced after waiting")
}
