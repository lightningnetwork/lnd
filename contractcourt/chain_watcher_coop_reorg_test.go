package contractcourt

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// TestChainWatcherCoopCloseReorg tests that the chain watcher properly handles
// a reorganization during cooperative close confirmation waiting. When a
// cooperative close transaction is reorganized out, the chain watcher should
// re-register for spend notifications and detect an alternative transaction.
func TestChainWatcherCoopCloseReorg(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create two cooperative close transactions with different fees.
	tx1 := harness.createCoopCloseTx(5000)
	tx2 := harness.createCoopCloseTx(4900)

	// Run cooperative close flow with reorg.
	closeInfo := harness.runCoopCloseFlow(tx1, true, 2, tx2)

	// Assert that the second transaction was confirmed.
	harness.assertCoopCloseTx(closeInfo, tx2)
}

// TestChainWatcherCoopCloseSameTransactionAfterReorg tests that if the same
// transaction re-confirms after a reorganization, it is properly handled.
func TestChainWatcherCoopCloseSameTransactionAfterReorg(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(t)

	// Create a single cooperative close transaction.
	tx := harness.createCoopCloseTx(5000)

	// Run flow with the same tx confirming after the reorg.
	closeInfo := harness.runCoopCloseFlow(tx, true, 2, tx)

	harness.assertCoopCloseTx(closeInfo, tx)
}

// TestChainWatcherCoopCloseMultipleReorgs tests handling of multiple
// consecutive reorganizations during cooperative close confirmation.
func TestChainWatcherCoopCloseMultipleReorgs(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create multiple cooperative close transactions with different fees.
	txs := []*wire.MsgTx{
		harness.createCoopCloseTx(5000),
		harness.createCoopCloseTx(4950),
		harness.createCoopCloseTx(4900),
		harness.createCoopCloseTx(4850),
	}

	// Define reorg depths for each transition.
	reorgDepths := []int32{1, 2, 3}

	// Run multiple reorg flow.
	closeInfo := harness.runMultipleReorgFlow(txs, reorgDepths)

	// Assert that the final transaction was confirmed.
	harness.assertCoopCloseTx(closeInfo, txs[3])
}

// TestChainWatcherCoopCloseReorgNoAlternative tests that if a cooperative
// close is reorganized out and no alternative transaction appears, the
// chain watcher continues waiting.
func TestChainWatcherCoopCloseReorgNoAlternative(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create a cooperative close transaction.
	tx := harness.createCoopCloseTx(5000)

	// Send spend and wait for confirmation registration.
	harness.sendSpend(tx)
	harness.waitForConfRegistration()

	// Trigger reorg after some confirmations.
	harness.mineBlocks(2)
	harness.triggerReorg(tx, 2)

	// Assert no cooperative close event is received.
	harness.assertNoCoopClose(2 * time.Second)

	// Now send a new transaction after the timeout.
	harness.waitForSpendRegistration()
	newTx := harness.createCoopCloseTx(4900)
	harness.sendSpend(newTx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(newTx, harness.currentHeight)

	// Should receive cooperative close for the new transaction.
	closeInfo := harness.waitForCoopClose(5 * time.Second)
	harness.assertCoopCloseTx(closeInfo, newTx)
}

// TestChainWatcherCoopCloseScaledConfirmationsWithReorg tests that scaled
// confirmations (based on channel capacity) work correctly with reorgs.
func TestChainWatcherCoopCloseScaledConfirmationsWithReorg(t *testing.T) {
	t.Parallel()

	// Test with different confirmation requirements and reorg depths.
	// Note: We start at 3 confirmations because 1-conf uses the fast path
	// which bypasses reorg protection (it dispatches immediately).
	testCases := []struct {
		name          string
		requiredConfs uint32
		reorgDepth    int32
	}{
		{
			name:          "triple_conf",
			requiredConfs: 3,
			reorgDepth:    2,
		},
		{
			name:          "six_conf",
			requiredConfs: 6,
			reorgDepth:    4,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create harness with specific confirmation
			// requirements.
			harness := newChainWatcherTestHarness(
				t, withRequiredConfs(tc.requiredConfs),
			)

			// Create transactions.
			tx1 := harness.createCoopCloseTx(5000)
			tx2 := harness.createCoopCloseTx(4900)

			// Run with reorg at different depths based on capacity.
			closeInfo := harness.runCoopCloseFlow(
				tx1, true, tc.reorgDepth, tx2,
			)

			// Verify correct transaction confirmed.
			harness.assertCoopCloseTx(closeInfo, tx2)
		})
	}
}

// TestChainWatcherCoopCloseRapidReorgs tests that the chain watcher handles
// multiple rapid reorgs in succession without getting into a broken state.
func TestChainWatcherCoopCloseRapidReorgs(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create a cooperative close transaction.
	tx := harness.createCoopCloseTx(5000)

	// Send spend notification.
	harness.sendSpend(tx)

	// Trigger multiple rapid reorgs to stress the state machine.
	for i := 0; i < 5; i++ {
		harness.waitForConfRegistration()
		harness.mineBlocks(1)
		harness.triggerReorg(tx, int32(i+1))
		if i < 4 {
			// Re-register for spend after each reorg except the
			// last.
			harness.waitForSpendRegistration()
			harness.sendSpend(tx)
		}
	}

	// After stress, send a clean transaction.
	harness.waitForSpendRegistration()
	cleanTx := harness.createCoopCloseTx(4800)
	harness.sendSpend(cleanTx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(cleanTx, harness.currentHeight)

	// Should still receive the cooperative close.
	closeInfo := harness.waitForCoopClose(10 * time.Second)
	harness.assertCoopCloseTx(closeInfo, cleanTx)
}
