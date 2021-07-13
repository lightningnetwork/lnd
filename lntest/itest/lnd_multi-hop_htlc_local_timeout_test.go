package itest

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testMultiHopHtlcLocalTimeout tests that in a multi-hop HTLC scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it using the HTLC timeout transaction. Any dust HTLC's should be immediately
// canceled backwards. Once the timeout has been reached, then we should sweep
// it on-chain, and cancel the HTLC backwards.
func testMultiHopHtlcLocalTimeout(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, alice, bob, true, c,
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	time.Sleep(time.Second * 1)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt    = btcutil.Amount(100)
		htlcAmt        = btcutil.Amount(300_000)
		finalCltvDelta = 40
	)

	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := makeFakePayHash(t)
	payHash := makeFakePayHash(t)

	_, err := alice.RouterClient.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			Dest:           carolPubKey,
			Amt:            int64(dustHtlcAmt),
			PaymentHash:    dustPayHash,
			FinalCltvDelta: finalCltvDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	require.NoError(t.t, err)

	_, err = alice.RouterClient.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			Dest:           carolPubKey,
			Amt:            int64(htlcAmt),
			PaymentHash:    payHash,
			FinalCltvDelta: finalCltvDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	require.NoError(t.t, err)

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, dustPayHash, payHash)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	net.SetFeeEstimate(30000)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default outgoing broadcast delta of zero, this will
	// be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	_, err = net.Miner.Client.Generate(numBlocks)
	require.NoError(t.t, err)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	expectedTxes := 1
	hasAnchors := commitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}
	_, err = waitForNTxsInMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	bobFundingTxid, err := lnrpc.GetChanPointFundingTxid(bobChanPoint)
	require.NoError(t.t, err)
	bobChanOutpoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}
	closeTxid := assertSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, bobChanOutpoint,
	)

	// Mine a block to confirm the closing transaction.
	mineBlocks(t, net, 1, expectedTxes)

	// At this point, Bob should have canceled backwards the dust HTLC
	// that we sent earlier. This means Alice should now only have a single
	// HTLC on her channel.
	nodes = []*lntest.HarnessNode{alice}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transaction to be broadcast due to the expiry being reached.
	// If there are anchors, we also expect Carol's anchor sweep now.
	_, err = getNTxsFromMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	// We'll also obtain the expected HTLC timeout transaction hash.
	htlcOutpoint := wire.OutPoint{Hash: closeTxid, Index: 0}
	commitOutpoint := wire.OutPoint{Hash: closeTxid, Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		commitOutpoint.Index = 3
	}
	htlcTimeoutTxid := assertSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, htlcOutpoint,
	)

	// Mine a block to confirm the expected transactions.
	_ = mineBlocks(t, net, 1, expectedTxes)

	// With Bob's HTLC timeout transaction confirmed, there should be no
	// active HTLC's on the commitment transaction from Alice -> Bob.
	err = wait.NoError(func() error {
		return assertNumActiveHtlcs([]*lntest.HarnessNode{alice}, 0)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is ready to be swept once the timelock is up.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := bob.PendingChannels(ctxt, pendingChansRequest)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(pendingChanResp.PendingForceClosingChannels))
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	require.NotZero(t.t, forceCloseChan.LimboBalance)
	require.Positive(t.t, forceCloseChan.BlocksTilMaturity)
	require.Equal(t.t, 1, len(forceCloseChan.PendingHtlcs))
	require.Equal(t.t, uint32(2), forceCloseChan.PendingHtlcs[0].Stage)

	htlcTimeoutOutpoint := wire.OutPoint{Hash: htlcTimeoutTxid, Index: 0}
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Since Bob is the initiator of the script-enforced leased
		// channel between him and Carol, he will incur an additional
		// CLTV on top of the usual CSV delay on any outputs that he can
		// sweep back to his wallet.
		blocksTilMaturity := uint32(forceCloseChan.BlocksTilMaturity)
		mineBlocks(t, net, blocksTilMaturity, 0)

		// Check that the sweep spends the expected inputs.
		_ = assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout,
			commitOutpoint, htlcTimeoutOutpoint,
		)
	} else {
		// Since Bob force closed the channel between him and Carol, he
		// will incur the usual CSV delay on any outputs that he can
		// sweep back to his wallet. We'll subtract one block from our
		// current maturity period to assert on the mempool.
		mineBlocks(t, net, uint32(forceCloseChan.BlocksTilMaturity-1), 0)

		// Check that the sweep spends from the mined commitment.
		_ = assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout, commitOutpoint,
		)

		// Mine a block to confirm Bob's commit sweep tx and assert it
		// was in fact mined.
		_ = mineBlocks(t, net, 1, 1)[0]

		// Mine an additional block to prompt Bob to broadcast their
		// second layer sweep due to the CSV on the HTLC timeout output.
		mineBlocks(t, net, 1, 0)
		_ = assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout,
			htlcTimeoutOutpoint,
		)
	}

	// Next, we'll mine a final block that should confirm the sweeping
	// transactions left.
	_, err = net.Miner.Client.Generate(1)
	require.NoError(t.t, err)

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	err = waitForNumChannelPendingForceClose(bob, 0, nil)
	require.NoError(t.t, err)

	// Coop close channel, expect no anchors.
	closeChannelAndAssertType(t, net, alice, aliceChanPoint, false, false)
}
