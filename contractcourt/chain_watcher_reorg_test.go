package contractcourt

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"pgregory.net/rapid"
)

// closeType represents the type of channel close for testing purposes.
type closeType int

const (
	// closeTypeCoop represents a cooperative channel close.
	closeTypeCoop closeType = iota

	// closeTypeRemoteUnilateral represents a remote unilateral close
	// (remote party broadcasting their commitment).
	closeTypeRemoteUnilateral

	// closeTypeLocalForce represents a local force close (us broadcasting
	// our commitment).
	closeTypeLocalForce

	// closeTypeBreach represents a breach (remote party broadcasting a
	// revoked commitment).
	closeTypeBreach
)

// String returns a string representation of the close type.
func (c closeType) String() string {
	switch c {
	case closeTypeCoop:
		return "cooperative"
	case closeTypeRemoteUnilateral:
		return "remote_unilateral"
	case closeTypeLocalForce:
		return "local_force"
	case closeTypeBreach:
		return "breach"
	default:
		return "unknown"
	}
}

// createCloseTx creates a close transaction of the specified type using the
// harness.
func createCloseTx(h *chainWatcherTestHarness, ct closeType,
	outputValue int64) *wire.MsgTx {

	switch ct {
	case closeTypeCoop:
		return h.createCoopCloseTx(outputValue)
	case closeTypeRemoteUnilateral:
		return h.createRemoteUnilateralCloseTx()
	case closeTypeLocalForce:
		return h.createLocalForceCloseTx()
	case closeTypeBreach:
		return h.createBreachCloseTx()
	default:
		h.t.Fatalf("unknown close type: %v", ct)
		return nil
	}
}

// waitForCloseEvent waits for the appropriate close event based on close type.
func waitForCloseEvent(h *chainWatcherTestHarness, ct closeType,
	timeout time.Duration) any {

	switch ct {
	case closeTypeCoop:
		return h.waitForCoopClose(timeout)
	case closeTypeRemoteUnilateral:
		return h.waitForRemoteUnilateralClose(timeout)
	case closeTypeLocalForce:
		return h.waitForLocalUnilateralClose(timeout)
	case closeTypeBreach:
		return h.waitForBreach(timeout)
	default:
		h.t.Fatalf("unknown close type: %v", ct)
		return nil
	}
}

// assertCloseEventTx asserts that the close event matches the expected
// transaction based on close type.
func assertCloseEventTx(h *chainWatcherTestHarness, ct closeType,
	event any, expectedTx *wire.MsgTx) {

	switch ct {
	case closeTypeCoop:
		h.assertCoopCloseTx(event.(*CooperativeCloseInfo), expectedTx)

	case closeTypeRemoteUnilateral:
		h.assertRemoteUnilateralCloseTx(
			event.(*RemoteUnilateralCloseInfo), expectedTx,
		)

	case closeTypeLocalForce:
		h.assertLocalUnilateralCloseTx(
			event.(*LocalUnilateralCloseInfo), expectedTx,
		)

	case closeTypeBreach:
		h.assertBreachTx(event.(*BreachCloseInfo), expectedTx)

	default:
		h.t.Fatalf("unknown close type: %v", ct)
	}
}

// generateAltTxsForReorgs generates alternative transactions for reorg
// scenarios. For commitment-based closes (breach, remote/local force), the same
// tx is reused since we can only have one commitment tx per channel state. For
// coop closes, new transactions with different output values are created.
func generateAltTxsForReorgs(h *chainWatcherTestHarness, ct closeType,
	originalTx *wire.MsgTx, numReorgs int, sameTxAtEnd bool) []*wire.MsgTx {

	altTxs := make([]*wire.MsgTx, numReorgs)

	for i := 0; i < numReorgs; i++ {
		switch ct {
		case closeTypeBreach, closeTypeRemoteUnilateral,
			closeTypeLocalForce:

			// Non-coop closes can only have one commitment tx, so
			// all reorgs use the same transaction.
			altTxs[i] = originalTx

		case closeTypeCoop:
			if i == numReorgs-1 && sameTxAtEnd {
				// Last reorg goes back to original transaction.
				altTxs[i] = originalTx
			} else {
				// Create different coop close tx with different
				// output value to make it unique.
				outputValue := int64(5000 - (i+1)*100)
				altTxs[i] = createCloseTx(h, ct, outputValue)
			}
		}
	}

	return altTxs
}

// testReorgProperties is the main property-based test for reorg handling
// across all close types.
//
// The testingT parameter is captured from the outer test function and used
// for operations that require *testing.T (like channel creation), while the
// rapid.T is used for all test reporting and property generation.
func testReorgProperties(testingT *testing.T) func(*rapid.T) {
	return func(t *rapid.T) {
		// Generate random close type.
		allCloseTypes := []closeType{
			closeTypeCoop,
			closeTypeRemoteUnilateral,
			closeTypeLocalForce,
			closeTypeBreach,
		}
		ct := rapid.SampledFrom(allCloseTypes).Draw(t, "closeType")

		// Generate random number of required confirmations (2-6). We
		// use at least 2 so we have room for reorgs during
		// confirmation.
		requiredConfs := rapid.IntRange(2, 6).Draw(t, "requiredConfs")

		// Generate number of reorgs (1-3 to keep test runtime
		// reasonable).
		numReorgs := rapid.IntRange(1, 3).Draw(t, "numReorgs")

		// Generate whether the final transaction is the same as the
		// original.
		sameTxAtEnd := rapid.Bool().Draw(t, "sameTxAtEnd")

		// Log test parameters for debugging.
		t.Logf("Testing %s close with %d confs, %d reorgs, "+
			"sameTxAtEnd=%v",
			ct, requiredConfs, numReorgs, sameTxAtEnd)

		// Create test harness using both the concrete *testing.T for
		// channel creation and the rapid.T for test reporting.
		harness := newChainWatcherTestHarnessFromReporter(
			testingT, t, withRequiredConfs(uint32(requiredConfs)),
		)

		// Create initial transaction.
		tx1 := createCloseTx(harness, ct, 5000)

		// Generate alternative transactions for each reorg.
		altTxs := generateAltTxsForReorgs(
			harness, ct, tx1, numReorgs, sameTxAtEnd,
		)

		// Send the initial spend.
		harness.sendSpend(tx1)
		harness.waitForConfRegistration()

		// Execute the set of re-orgs, based on our random sample, we'll
		// mine N blocks, do a re-org of size N, then wait for
		// detection, and repeat.
		for i := 0; i < numReorgs; i++ {
			// Generate random reorg depth (1 to requiredConfs-1).
			// We cap it to avoid reorging too far back.
			reorgDepth := rapid.IntRange(
				1, requiredConfs-1,
			).Draw(t, "reorgDepth")

			// Mine some blocks (but less than required confs).
			blocksToMine := rapid.IntRange(
				1, requiredConfs-1,
			).Draw(t, "blocksToMine")
			harness.mineBlocks(int32(blocksToMine))

			// Trigger reorg.
			if i == 0 {
				harness.triggerReorg(
					tx1, int32(reorgDepth),
				)
			} else {
				harness.triggerReorg(
					altTxs[i-1], int32(reorgDepth),
				)
			}

			harness.waitForSpendRegistration()

			harness.sendSpend(altTxs[i])
			harness.waitForConfRegistration()
		}

		// Mine enough blocks to confirm final transaction.
		harness.mineBlocks(1)
		finalTx := altTxs[numReorgs-1]
		harness.confirmTx(finalTx, harness.currentHeight)

		// Wait for and verify close event.
		event := waitForCloseEvent(harness, ct, 10*time.Second)
		assertCloseEventTx(harness, ct, event, finalTx)
	}
}

// TestChainWatcherReorgAllCloseTypes runs property-based tests for reorg
// handling across all channel close types. It generates random combinations of:
// - Close type (coop, remote unilateral, local force, breach)
// - Number of confirmations required (2-6)
// - Number of reorgs (1-3)
// - Whether the final tx is same as original or different
func TestChainWatcherReorgAllCloseTypes(t *testing.T) {
	t.Parallel()

	rapid.Check(t, testReorgProperties(t))
}

// TestRemoteUnilateralCloseWithSingleReorg tests that a remote unilateral
// close is properly handled when a single reorg occurs during confirmation.
func TestRemoteUnilateralCloseWithSingleReorg(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(t)

	// Create two remote unilateral close transactions.
	// Since these are commitment transactions, we can only have one per
	// state, so we'll use the current one as tx1.
	tx1 := harness.createRemoteUnilateralCloseTx()

	// Advance channel state to get a different commitment.
	_ = harness.createBreachCloseTx()
	tx2 := harness.createRemoteUnilateralCloseTx()

	// Send initial spend.
	harness.sendSpend(tx1)
	harness.waitForConfRegistration()

	// Mine a block and trigger reorg.
	harness.mineBlocks(1)
	harness.triggerReorg(tx1, 1)

	// Send alternative transaction after reorg.
	harness.waitForSpendRegistration()
	harness.sendSpend(tx2)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(tx2, harness.currentHeight)

	// Verify correct event.
	closeInfo := harness.waitForRemoteUnilateralClose(5 * time.Second)
	harness.assertRemoteUnilateralCloseTx(closeInfo, tx2)
}

// TestLocalForceCloseWithMultipleReorgs tests that a local force close is
// properly handled through multiple consecutive reorgs.
func TestLocalForceCloseWithMultipleReorgs(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(t)

	// For local force close, we can only broadcast our current commitment.
	// We'll simulate multiple reorgs where the same tx keeps getting
	// reorganized out and re-broadcast.
	tx := harness.createLocalForceCloseTx()

	// First spend and reorg.
	harness.sendSpend(tx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.triggerReorg(tx, 1)

	// Second spend and reorg.
	harness.waitForSpendRegistration()
	harness.sendSpend(tx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.triggerReorg(tx, 1)

	// Third spend - this one confirms.
	harness.waitForSpendRegistration()
	harness.sendSpend(tx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(tx, harness.currentHeight)

	// Verify correct event.
	closeInfo := harness.waitForLocalUnilateralClose(5 * time.Second)
	harness.assertLocalUnilateralCloseTx(closeInfo, tx)
}

// TestBreachCloseWithDeepReorg tests that a breach (revoked commitment) is
// properly detected after a deep reorganization.
func TestBreachCloseWithDeepReorg(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(t)

	// Create a revoked commitment transaction.
	revokedTx := harness.createBreachCloseTx()

	// Send spend and wait for confirmation registration.
	harness.sendSpend(revokedTx)
	harness.waitForConfRegistration()

	// Mine several blocks and then trigger a deep reorg.
	harness.mineBlocks(5)
	harness.triggerReorg(revokedTx, 5)

	// Re-broadcast same transaction after reorg.
	harness.waitForSpendRegistration()
	harness.sendSpend(revokedTx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(revokedTx, harness.currentHeight)

	// Verify breach detection.
	breachInfo := harness.waitForBreach(5 * time.Second)
	harness.assertBreachTx(breachInfo, revokedTx)
}

// TestCoopCloseReorgToForceClose tests the edge case where a cooperative
// close gets reorged out and is replaced by a force close.
func TestCoopCloseReorgToForceClose(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(t)

	// Create a cooperative close and a force close transaction.
	coopTx := harness.createCoopCloseTx(5000)
	forceTx := harness.createRemoteUnilateralCloseTx()

	// Send cooperative close.
	harness.sendSpend(coopTx)
	harness.waitForConfRegistration()

	// Trigger reorg that removes coop close.
	harness.mineBlocks(1)
	harness.triggerReorg(coopTx, 1)

	// Send force close as alternative.
	harness.waitForSpendRegistration()
	harness.sendSpend(forceTx)
	harness.waitForConfRegistration()
	harness.mineBlocks(1)
	harness.confirmTx(forceTx, harness.currentHeight)

	// Should receive remote unilateral close event, not coop close.
	closeInfo := harness.waitForRemoteUnilateralClose(5 * time.Second)
	harness.assertRemoteUnilateralCloseTx(closeInfo, forceTx)
}
