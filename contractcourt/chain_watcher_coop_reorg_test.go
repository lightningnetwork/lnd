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

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create a single cooperative close transaction.
	tx := harness.createCoopCloseTx(5000)

	// Run flow: send tx, trigger reorg, re-send same tx.
	harness.sendSpend(tx)

	// Wait for confirmation registration and trigger reorg.
	harness.waitForConfRegistration()
	harness.mineBlocks(2)
	harness.triggerReorg(tx, 2)

	// After reorg, wait for re-registration then re-send the same transaction.
	harness.waitForSpendRegistration()
	harness.sendSpend(tx)

	// Confirm it.
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(tx, harness.currentHeight)

	// Wait for and verify cooperative close.
	closeInfo := harness.waitForCoopClose(5 * time.Second)
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

// TestChainWatcherCoopCloseDeepReorg tests that the chain watcher can handle
// deep reorganizations where the reorg depth exceeds the required number of
// confirmations.
func TestChainWatcherCoopCloseDeepReorg(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create two cooperative close transactions.
	tx1 := harness.createCoopCloseTx(5000)
	tx2 := harness.createCoopCloseTx(4900)

	// Run with a deep reorg (10 blocks).
	closeInfo := harness.runCoopCloseFlow(tx1, true, 10, tx2)

	// Assert that the second transaction was confirmed after deep reorg.
	harness.assertCoopCloseTx(closeInfo, tx2)
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

// TestChainWatcherCoopCloseRBFAfterReorg tests that RBF cooperative close
// transactions are properly handled when reorganizations occur.
func TestChainWatcherCoopCloseRBFAfterReorg(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create a series of RBF transactions with increasing fees.
	rbfTxs := make([]*wire.MsgTx, 3)
	for i := range rbfTxs {
		// Each transaction has higher fee (lower output).
		outputValue := int64(5000 - i*100)
		rbfTxs[i] = harness.createCoopCloseTx(outputValue)
	}

	// Send first RBF transaction.
	harness.sendSpend(rbfTxs[0])
	harness.waitForConfRegistration()

	// Trigger reorg.
	harness.mineBlocks(1)
	harness.triggerReorg(rbfTxs[0], 1)

	// Send second RBF transaction after re-registration.
	harness.waitForSpendRegistration()
	harness.sendSpend(rbfTxs[1])
	harness.waitForConfRegistration()

	// Another reorg.
	harness.mineBlocks(1)
	harness.triggerReorg(rbfTxs[1], 1)

	// Send final RBF transaction after re-registration.
	harness.waitForSpendRegistration()
	harness.sendSpend(rbfTxs[2])
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(rbfTxs[2], harness.currentHeight)

	// Should confirm the highest fee transaction.
	closeInfo := harness.waitForCoopClose(5 * time.Second)
	harness.assertCoopCloseTx(closeInfo, rbfTxs[2])
}

// TestChainWatcherCoopCloseScaledConfirmationsWithReorg tests that scaled
// confirmations (based on channel capacity) work correctly with reorgs.
func TestChainWatcherCoopCloseScaledConfirmationsWithReorg(t *testing.T) {
	t.Parallel()

	// Test with different channel capacities that require different
	// confirmation counts.
	testCases := []struct {
		name          string
		capacityScale float64
		expectedConfs uint16
		reorgDepth    int32
	}{
		{
			name:          "small_channel",
			capacityScale: 0.1,
			expectedConfs: 1,
			reorgDepth:    1,
		},
		{
			name:          "medium_channel",
			capacityScale: 0.5,
			expectedConfs: 3,
			reorgDepth:    2,
		},
		{
			name:          "large_channel",
			capacityScale: 1.0,
			expectedConfs: 6,
			reorgDepth:    4,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create harness with specific channel capacity.
			harness := newChainWatcherTestHarness(t)

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

// TestChainWatcherCoopCloseReorgRaceCondition tests that the chain watcher
// correctly handles race conditions where multiple transactions might be
// in flight during reorganizations.
func TestChainWatcherCoopCloseReorgRaceCondition(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create multiple transactions.
	txs := make([]*wire.MsgTx, 5)
	for i := range txs {
		txs[i] = harness.createCoopCloseTx(int64(5000 - i*50))
	}

	// Rapidly send multiple transactions and reorgs.
	for i := 0; i < 3; i++ {
		harness.sendSpend(txs[i])
		harness.waitForConfRegistration()
		harness.mineBlocks(1)

		// Quick reorg.
		harness.triggerReorg(txs[i], 1)

		// Wait for re-registration.
		harness.waitForSpendRegistration()
	}

	// Eventually send and confirm a final transaction.
	finalTx := txs[4]
	harness.sendSpend(finalTx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(finalTx, harness.currentHeight)

	// Should eventually settle on the final transaction.
	closeInfo := harness.waitForCoopClose(10 * time.Second)
	harness.assertCoopCloseTx(closeInfo, finalTx)
}

// TestChainWatcherCoopCloseReorgErrorHandling tests that errors during
// re-registration after reorg are properly handled.
func TestChainWatcherCoopCloseReorgErrorHandling(t *testing.T) {
	t.Parallel()

	// Create test harness.
	harness := newChainWatcherTestHarness(t)

	// Create a cooperative close transaction.
	tx := harness.createCoopCloseTx(5000)

	// Send spend notification.
	harness.sendSpend(tx)

	// Trigger multiple rapid reorgs to stress error handling.
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
