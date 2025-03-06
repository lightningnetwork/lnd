package itest

import (
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

// flakeInconsistentHTLCView documents a flake found that the `ListChannels` RPC
// can give inaccurate HTLC states, which is found when we call
// `AssertHTLCNotActive` after a commitment dance is finished. Suppose Carol is
// settling an invoice with Bob, from Bob's PoV, a typical healthy settlement
// flow goes like this:
//
//	[DBG] PEER brontide.go:2412: Peer([Carol]): Received UpdateFulfillHTLC
//	[DBG] HSWC switch.go:1315: Closed completed SETTLE circuit for ...
//	[INF] HSWC switch.go:3044: Forwarded HTLC...
//	[DBG] PEER brontide.go:2412: Peer([Carol]): Received CommitSig
//	[DBG] PEER brontide.go:2412: Peer([Carol]): Sending RevokeAndAck
//	[DBG] PEER brontide.go:2412: Peer([Carol]): Sending CommitSig
//	[DBG] PEER brontide.go:2412: Peer([Carol]): Received RevokeAndAck
//	[DBG] HSWC link.go:3617: ChannelLink([ChanPoint: Bob=>Carol]): settle-fail-filter: count=1, filter=[0]
//	[DBG] HSWC switch.go:3001: Circuit is closing for packet...
//
// Bob receives the preimage, closes the circuit, and exchanges commit sig and
// revoke msgs with Carol. Once Bob receives the `CommitSig` from Carol, the
// HTLC should be removed from his `LocalCommitment` via
// `RevokeCurrentCommitment`.
//
// However, in the test where `AssertHTLCNotActive` is called, although the
// above process is finished, the `ListChannels“ still finds the HTLC. Also note
// that the RPC makes direct call to the channeldb without any locks, which
// should be fine as the struct `OpenChannel.LocalCommitment` is passed by
// value, although we need to double check.
//
// TODO(yy): In order to fix it, we should make the RPC share the same view of
// our channel state machine. Instead of making DB queries, it should instead
// use `lnwallet.LightningChannel` instead to stay consistent.
//
//nolint:ll
func flakeInconsistentHTLCView() {
	// Perform a sleep so the commiment dance can be finished before we call
	// the ListChannels.
	time.Sleep(2 * time.Second)
}

// flakePaymentStreamReturnEarly documents a flake found in the test which
// relies on a given payment to be settled before testing other state changes.
// The issue comes from the payment stream created from the RPC `SendPaymentV2`
// gives premature settled event for a given payment, which is found in,
//   - if we force close the channel immediately, we may get an error because
//     the commitment dance is not finished.
//   - if we subscribe HTLC events immediately, we may get extra events, which
//     is also related to the above unfinished commitment dance.
//
// TODO(yy): Make sure we only mark the payment being settled once the
// commitment dance is finished. In addition, we should also fix the exit hop
// logic in the invoice settlement flow to make sure the invoice is only marked
// as settled after the commitment dance is finished.
func flakePaymentStreamReturnEarly() {
	// Sleep 2 seconds so the pending HTLCs will be removed from the
	// commitment.
	time.Sleep(2 * time.Second)
}
