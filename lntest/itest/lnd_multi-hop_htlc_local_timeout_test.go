package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testMultiHopHtlcLocalTimeout tests that in a multi-hop HTLC scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it using the HTLC timeout transaction. Any dust HTLC's should be immediately
// canceled backwards. Once the timeout has been reached, then we should sweep
// it on-chain, and cancel the HTLC backwards.
func testMultiHopHtlcLocalTimeout(ht *lntest.HarnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, true, c,
	)

	// Clean up carol's node when the test finishes.
	defer ht.Shutdown(carol)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt    = btcutil.Amount(100)
		htlcAmt        = btcutil.Amount(300_000)
		finalCltvDelta = 40
	)

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := ht.Random32Bytes()
	payHash := ht.Random32Bytes()

	ht.SendPayment(
		alice, &routerrpc.SendPaymentRequest{
			Dest:           carolPubKey,
			Amt:            int64(dustHtlcAmt),
			PaymentHash:    dustPayHash,
			FinalCltvDelta: finalCltvDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	ht.SendPayment(
		alice, &routerrpc.SendPaymentRequest{
			Dest:           carolPubKey,
			Amt:            int64(htlcAmt),
			PaymentHash:    payHash,
			FinalCltvDelta: finalCltvDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	ht.AssertActiveHtlcs(alice, dustPayHash, payHash)
	ht.AssertActiveHtlcs(bob, dustPayHash, payHash)
	ht.AssertActiveHtlcs(carol, dustPayHash, payHash)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default outgoing broadcast delta of zero, this will
	// be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineBlocks(numBlocks)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	expectedTxes := 1
	hasAnchors := commitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}
	ht.AssertNumTxsInMempool(expectedTxes)

	closeTx := ht.AssertTxInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)

	// Mine a block to confirm the closing transaction.
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// At this point, Bob should have canceled backwards the dust HTLC
	// that we sent earlier. This means Alice should now only have a single
	// HTLC on her channel.
	ht.AssertActiveHtlcs(alice, payHash)

	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transaction to be broadcast due to the expiry being reached.
	// If there are anchors, we also expect Carol's anchor sweep now.
	ht.AssertNumTxsInMempool(expectedTxes)

	// We'll also obtain the expected HTLC timeout transaction hash.
	htlcOutpoint := wire.OutPoint{Hash: closeTx.TxHash(), Index: 0}
	commitOutpoint := wire.OutPoint{Hash: closeTx.TxHash(), Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		commitOutpoint.Index = 3
	}
	htlcTimeoutTxid := ht.AssertTxInMempool(htlcOutpoint).TxHash()

	// Mine a block to confirm the expected transactions.
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// With Bob's HTLC timeout transaction confirmed, there should be no
	// active HTLC's on the commitment transaction from Alice -> Bob.
	ht.AssertNumActiveHtlcs(alice, 0)

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is ready to be swept once the timelock is up.
	pendingChanResp := ht.GetPendingChannels(bob)
	require.Equal(ht, 1, len(pendingChanResp.PendingForceClosingChannels))
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	require.NotZero(ht, forceCloseChan.LimboBalance)
	require.Positive(ht, forceCloseChan.BlocksTilMaturity)
	require.Equal(ht, 1, len(forceCloseChan.PendingHtlcs))
	require.Equal(ht, uint32(2), forceCloseChan.PendingHtlcs[0].Stage)

	htlcTimeoutOutpoint := wire.OutPoint{Hash: htlcTimeoutTxid, Index: 0}
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Since Bob is the initiator of the script-enforced leased
		// channel between him and Carol, he will incur an additional
		// CLTV on top of the usual CSV delay on any outputs that he can
		// sweep back to his wallet.
		blocksTilMaturity := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(blocksTilMaturity)

		// Check that the sweep spends the expected inputs.
		ht.AssertTxInMempool(commitOutpoint)
		ht.AssertTxInMempool(htlcTimeoutOutpoint)
	} else {
		// Since Bob force closed the channel between him and Carol, he
		// will incur the usual CSV delay on any outputs that he can
		// sweep back to his wallet. We'll subtract one block from our
		// current maturity period to assert on the mempool.
		ht.MineBlocks(uint32(forceCloseChan.BlocksTilMaturity - 1))

		// Check that the sweep spends from the mined commitment.
		ht.AssertTxInMempool(commitOutpoint)

		// Mine a block to confirm Bob's commit sweep tx and assert it
		// was in fact mined.
		ht.MineBlocksAndAssertTx(1, 1)

		// Mine an additional block to prompt Bob to broadcast their
		// second layer sweep due to the CSV on the HTLC timeout output.
		ht.MineBlocksAndAssertTx(1, 0)
		ht.AssertTxInMempool(htlcTimeoutOutpoint)
	}

	// Next, we'll mine a final block that should confirm the sweeping
	// transactions left.
	ht.MineBlocks(1)

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	ht.AssertNumChannelPendingForceClose(bob, 0)

	// Coop close channel, expect no anchors.
	ht.CloseChannel(alice, aliceChanPoint, false)
}
