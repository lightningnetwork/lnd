package contractcourt

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// TestEarlyDispatchCoopClose verifies the headline behavior: when a
// cooperative close spend is first detected on chain in the async path
// (numConfs > 1), the chain watcher fires the early-notify callback exactly
// once with a summary that carries IsPending=true. The full N-conf flow
// still completes normally and produces the regular CooperativeCloseInfo
// downstream.
func TestEarlyDispatchCoopClose(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(3), withEarlyCoopCloseCapture(),
	)

	tx := harness.createCoopCloseTx(5000)

	harness.sendSpend(tx)
	harness.waitForConfRegistration()

	// The early-dispatch callback must have fired exactly once with a
	// preliminary close summary that identifies the right tx, channel
	// point, and close type.
	harness.waitForEarlyCoopClose(1, time.Second)
	require.Equal(t, 1, harness.earlyCoopCloseCount(),
		"exactly one early dispatch expected on first spend detection")

	earlySummary := harness.earlyCoopCloseAt(0)
	require.True(t, earlySummary.IsPending,
		"early dispatched summary must have IsPending=true")
	require.Equal(t, channeldb.CooperativeClose, earlySummary.CloseType)
	require.Equal(t, tx.TxHash(), earlySummary.ClosingTXID)
	require.Equal(t, harness.aliceChannel.State().FundingOutpoint,
		earlySummary.ChanPoint)

	// Drive the close to N confs so the regular post-N-conf dispatch
	// path also completes; the resulting CooperativeCloseInfo must
	// reference the same tx.
	harness.mineBlocks(1)
	harness.confirmTx(tx, harness.currentHeight)

	closeInfo := harness.waitForCoopClose(5 * time.Second)
	harness.assertCoopCloseTx(closeInfo, tx)
}

// TestEarlyDispatchForceCloseNotInvoked verifies that force-close spends do
// NOT trigger the early-dispatch callback. Force-close paths intentionally
// stay on the N-confirmation dispatch contract; their CLOSED_CHANNEL event
// fires from the channel arbitrator's MarkChannelClosed callback at N
// confs.
func TestEarlyDispatchForceCloseNotInvoked(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(3), withEarlyCoopCloseCapture(),
	)

	tx := harness.createRemoteForceCloseTx()
	harness.sendSpend(tx)

	// processDetectedSpend evaluates the early-dispatch path before
	// registering the conf ntfn, so once that registration lands the
	// decision is final: no need for a separate sleep window.
	harness.waitForConfRegistration()
	require.Equal(t, 0, harness.earlyCoopCloseCount(),
		"remote force close must not trigger early dispatch")
}

// TestEarlyDispatchSkippedOnFastPath verifies that when the chain watcher is
// in the fast (single-confirmation) path, the early-dispatch callback is NOT
// invoked. The fast-path's existing dispatchCooperativeClose ->
// MarkChannelClosed flow already fires CLOSED_CHANNEL at first conf with a
// fully populated summary (including close initiator from the historical
// bucket), so an early dispatch here would deliver a duplicate event with an
// unknown initiator and break the SubscribeChannelEvents contract.
// EarlyCoopCloseDispatched() must stay false so the channel arbitrator's
// suppression gate does not fire.
func TestEarlyDispatchSkippedOnFastPath(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(1), withEarlyCoopCloseCapture(),
	)

	tx := harness.createCoopCloseTx(5000)

	harness.sendSpend(tx)

	// The fast path dispatches the regular CooperativeCloseInfo
	// synchronously from processDetectedSpend, so once the coop close
	// event lands we know the early-dispatch branch (which runs in the
	// same goroutine, just below the fast-path return) was either taken
	// or skipped: no sleep window required.
	closeInfo := harness.waitForCoopClose(5 * time.Second)
	harness.assertCoopCloseTx(closeInfo, tx)

	require.Equal(t, 0, harness.earlyCoopCloseCount(),
		"single-conf fast path must not invoke early dispatch")
	require.False(t, harness.chainWatcher.EarlyCoopCloseDispatched(),
		"EarlyCoopCloseDispatched must stay false on fast path so the "+
			"arbitrator still fires NotifyClosedChannel at "+
			"MarkChannelClosed time")
}

// TestEarlyDispatchFlagSetAfterAsyncDispatch verifies that
// EarlyCoopCloseDispatched flips to true once the chain watcher fires the
// preliminary CLOSED_CHANNEL on the async multi-conf path. This is the gate
// the channel arbitrator reads in MarkChannelClosed to suppress the duplicate
// notify; if it didn't flip, subscribers would see two CLOSED_CHANNEL events
// for the same close.
func TestEarlyDispatchFlagSetAfterAsyncDispatch(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(3), withEarlyCoopCloseCapture(),
	)

	require.False(t, harness.chainWatcher.EarlyCoopCloseDispatched(),
		"flag must be false before any spend is observed")

	tx := harness.createCoopCloseTx(5000)
	harness.sendSpend(tx)
	harness.waitForConfRegistration()
	harness.waitForEarlyCoopClose(1, time.Second)

	require.True(t, harness.chainWatcher.EarlyCoopCloseDispatched(),
		"flag must flip to true once the early dispatch fires so the "+
			"arbitrator suppresses the duplicate notify at "+
			"MarkChannelClosed time")
}

// TestEarlyDispatchReorgRefiresOnReReplacement verifies the reorg recovery
// path: once a deep reorg removes the close, the early-dispatch flag is
// cleared, and the next coop close re-fires the early event with its own
// summary. This is the contract that lets a subscriber observe each
// distinct close attempt rather than only the first one.
func TestEarlyDispatchReorgRefiresOnReReplacement(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(3), withEarlyCoopCloseCapture(),
	)

	tx1 := harness.createCoopCloseTx(5000)
	tx2 := harness.createCoopCloseTx(4900)

	// First close detected → early dispatch #1.
	harness.sendSpend(tx1)
	harness.waitForConfRegistration()
	harness.waitForEarlyCoopClose(1, time.Second)

	// Reorg flushes the conf ntfn and resets the flag.
	harness.triggerReorg(tx1, 2)
	harness.waitForSpendRegistration()

	// Replacement close detected → early dispatch #2 with the new tx.
	harness.sendSpend(tx2)
	harness.waitForConfRegistration()
	harness.waitForEarlyCoopClose(2, 2*time.Second)

	require.Equal(t, 2, harness.earlyCoopCloseCount(),
		"reorg + replacement close must re-fire the early dispatch")

	first := harness.earlyCoopCloseAt(0)
	second := harness.earlyCoopCloseAt(1)
	require.Equal(t, tx1.TxHash(), first.ClosingTXID,
		"first early dispatch must reference tx1")
	require.Equal(t, tx2.TxHash(), second.ClosingTXID,
		"second early dispatch must reference the replacement tx2")
}

// TestEarlyDispatchRefiresOnReplacementBeforeNegConf verifies the narrow
// reorg race where a replacement coop close is processed *before* the old
// confirmation subscription's NegativeConf has been drained. Without the
// reconciliation guard in processDetectedSpend, the stale
// coopCloseEarlyDispatched flag from the first spend would suppress the
// second early dispatch, and MarkChannelClosed's CloseType-gated
// suppression would then drop the final notify too — leaving subscribers
// with only the stale event for the no-longer-tracked txid. The test
// simulates that ordering by feeding the replacement spend directly
// without first draining the reorg path.
func TestEarlyDispatchRefiresOnReplacementBeforeNegConf(t *testing.T) {
	t.Parallel()

	harness := newChainWatcherTestHarness(
		t, withRequiredConfs(3), withEarlyCoopCloseCapture(),
	)

	tx1 := harness.createCoopCloseTx(5000)
	tx2 := harness.createCoopCloseTx(4900)

	// First close detected → early dispatch #1 with tx1.
	harness.sendSpend(tx1)
	harness.waitForConfRegistration()
	harness.waitForEarlyCoopClose(1, time.Second)

	// A different coop close lands while the watcher is still tracking
	// tx1 — this is the ordering ziggie flagged: the replacement arrives
	// before the prior spend's NegativeConf is observed. The watcher
	// must clear the stale flag and re-fire the early dispatch so the
	// new tx surfaces over the channel notifier.
	harness.sendSpend(tx2)
	harness.waitForConfRegistration()
	harness.waitForEarlyCoopClose(2, 2*time.Second)

	require.Equal(t, 2, harness.earlyCoopCloseCount(),
		"replacement spend must re-fire the early dispatch even when "+
			"NegativeConf has not yet drained")

	require.Equal(t, tx2.TxHash(), harness.earlyCoopCloseAt(1).ClosingTXID,
		"second early dispatch must reference the replacement tx2")
	require.True(t, harness.chainWatcher.EarlyCoopCloseDispatched(),
		"flag must remain set after the replacement's early dispatch "+
			"so the arbitrator's MarkChannelClosed suppression "+
			"still fires for the new tx")
}
