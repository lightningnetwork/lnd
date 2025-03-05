package itest

import (
	"runtime"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
)

// flakePreimageSettlement documents a flake found when testing the preimage
// extraction logic in a force close. The scenario is,
//   - Alice and Bob have a channel.
//   - Alice sends an HTLC to Bob, and Bob won't settle it.
//   - Alice goes offline.
//   - Bob force closes the channel and claims the HTLC using the preimage using
//     a sweeping tx.
//
// TODO(yy): Expose blockbeat to the link layer so the preimage extraction
// happens in the same block where it's spent.
func flakePreimageSettlement(ht *lntest.HarnessTest) {
	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched, which means Bob would miss the block to
	// launch the resolver.
	ht.MineEmptyBlocks(1)

	// Sleep for 2 seconds, which is needed to make sure the mempool has the
	// correct tx. The above mined block can cause an RBF, if the preimage
	// extraction has already been finished before the block is mined. This
	// means Bob would have created the sweeping tx - mining another block
	// would cause the sweeper to RBF it.
	time.Sleep(2 * time.Second)
}

// flakeFundExtraUTXO documents a flake found when testing the sweeping behavior
// of the given node, which would fail due to no wallet UTXO available, while
// there should be enough.
//
// TODO(yy): remove it once the issue is resolved.
func flakeFundExtraUTXO(ht *lntest.HarnessTest, node *node.HarnessNode) {
	// The node should have enough wallet UTXOs here to sweep the HTLC in
	// the end of this test. However, due to a known issue, the node's
	// wallet may report there's no UTXO available. For details,
	// - https://github.com/lightningnetwork/lnd/issues/8786
	ht.FundCoins(btcutil.SatoshiPerBitcoin, node)
}

// flakeTxNotifierNeutrino documents a flake found when running force close
// tests using neutrino backend, which is a race between two notifications - one
// for the spending notification, the other for the block which contains the
// spending tx.
//
// TODO(yy): remove it once the issue is resolved.
func flakeTxNotifierNeutrino(ht *lntest.HarnessTest) {
	// Mine an empty block the for neutrino backend. We need this step to
	// trigger Bob's chain watcher to detect the force close tx. Deep down,
	// this happens because the notification system for neutrino is very
	// different from others. Specifically, when a block contains the force
	// close tx is notified, these two calls,
	// - RegisterBlockEpochNtfn, will notify the block first.
	// - RegisterSpendNtfn, will wait for the neutrino notifier to sync to
	//   the block, then perform a GetUtxo, which, by the time the spend
	//   details are sent, the blockbeat is considered processed in Bob's
	//   chain watcher.
	//
	// TODO(yy): refactor txNotifier to fix the above issue.
	if ht.IsNeutrinoBackend() {
		ht.MineEmptyBlocks(1)
	}
}

// flakeSkipPendingSweepsCheckDarwin documents a flake found only in macOS
// build. When running in macOS, we might see three anchor sweeps - one from the
// local, one from the remote, and one from the pending remote:
//   - the local one will be swept.
//   - the remote one will be marked as failed due to `testmempoolaccept` check.
//   - the pending remote one will not be attempted due to it being uneconomical
//     since it was not used for CPFP.
//
// The anchor from the pending remote may or may not appear, which is a bug
// found only in macOS - when updating the commitments, the channel state
// machine somehow thinks we still have a pending remote commitment, causing it
// to sweep the anchor from that version.
//
// TODO(yy): fix the above bug in the channel state machine.
func flakeSkipPendingSweepsCheckDarwin(ht *lntest.HarnessTest,
	node *node.HarnessNode, num int) {

	// Skip the assertion below if it's on macOS.
	if isDarwin() {
		ht.Logf("Skipped AssertNumPendingSweeps for node %s",
			node.Name())

		return
	}

	ht.AssertNumPendingSweeps(node, num)
}

// isDarwin returns true if the test is running on a macOS.
func isDarwin() bool {
	return runtime.GOOS == "darwin"
}
