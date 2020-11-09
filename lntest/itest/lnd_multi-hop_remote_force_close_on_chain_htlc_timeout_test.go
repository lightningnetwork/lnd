package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testMultiHopRemoteForceCloseOnChainHtlcTimeout tests that if we extend a
// multi-hop HTLC, and the final destination of the HTLC force closes the
// channel, then we properly timeout the HTLC directly on *their* commitment
// transaction once the timeout has expired. Once we sweep the transaction, we
// should also cancel back the initial HTLC.
func testMultiHopRemoteForceCloseOnChainHtlcTimeout(net *lntest.NetworkHarness,
	t *harnessTest, alice, bob *lntest.HarnessNode, c commitType) {

	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, alice, bob, true, c,
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const (
		finalCltvDelta = 40
		htlcAmt        = btcutil.Amount(30000)
	)

	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := makeFakePayHash(t)
	_, err := alice.RouterClient.SendPaymentV2(
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

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	net.SetFeeEstimate(30000)

	// At this point, we'll now instruct Carol to force close the
	// transaction. This will let us exercise that Bob is able to sweep the
	// expired HTLC on Carol's version of the commitment transaction. If
	// Carol has an anchor, it will be swept too.
	ctxt, _ := context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssertType(
		ctxt, t, net, carol, bobChanPoint, c == commitTypeAnchors,
		true,
	)

	// At this point, Bob should have a pending force close channel as
	// Carol has gone directly to chain.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNumChannelPendingForceClose(ctxt, bob, 1, nil)
	require.NoError(t.t, err)

	// Bob can sweep his output immediately. If there is an anchor, Bob will
	// sweep that as well.
	expectedTxes := 1
	if c == commitTypeAnchors {
		expectedTxes = 2
	}

	_, err = waitForNTxsInMempool(
		net.Miner.Node, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	// Next, we'll mine enough blocks for the HTLC to expire. At this
	// point, Bob should hand off the output to his internal utxo nursery,
	// which will broadcast a sweep transaction.
	numBlocks := padCLTV(finalCltvDelta - 1)
	_, err = net.Miner.Node.Generate(numBlocks)
	require.NoError(t.t, err)

	// If we check Bob's pending channel report, it should show that he has
	// a single HTLC that's now in the second stage, as skip the initial
	// first stage since this is a direct HTLC.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNumChannelPendingForceClose(
		ctxt, bob, 1, func(c *lnrpcForceCloseChannel) error {
			if len(c.PendingHtlcs) != 1 {
				return fmt.Errorf("bob should have pending " +
					"htlc but doesn't")
			}

			if c.PendingHtlcs[0].Stage != 2 {
				return fmt.Errorf("bob's htlc should have "+
					"advanced to the second stage: %v", err)
			}

			return nil
		},
	)
	require.NoError(t.t, err)

	// We need to generate an additional block to trigger the sweep.
	_, err = net.Miner.Node.Generate(1)
	require.NoError(t.t, err)

	// Bob's sweeping transaction should now be found in the mempool at
	// this point.
	sweepTx, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		// If Bob's transaction isn't yet in the mempool, then due to
		// internal message passing and the low period between blocks
		// being mined, it may have been detected as a late
		// registration. As a result, we'll mine another block and
		// repeat the check. If it doesn't go through this time, then
		// we'll fail.
		// TODO(halseth): can we use waitForChannelPendingForceClose to
		// avoid this hack?
		_, err = net.Miner.Node.Generate(1)
		require.NoError(t.t, err)
		sweepTx, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
		require.NoError(t.t, err)
	}

	// If we mine an additional block, then this should confirm Bob's
	// transaction which sweeps the direct HTLC output.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, sweepTx)

	// Now that the sweeping transaction has been confirmed, Bob should
	// cancel back that HTLC. As a result, Alice should not know of any
	// active HTLC's.
	nodes = []*lntest.HarnessNode{alice}
	err = wait.NoError(func() error {
		return assertNumActiveHtlcs(nodes, 0)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Now we'll check Bob's pending channel report. Since this was Carol's
	// commitment, he doesn't have to wait for any CSV delays. As a result,
	// he should show no additional pending transactions.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNumChannelPendingForceClose(ctxt, bob, 0, nil)
	require.NoError(t.t, err)

	// We'll close out the test by closing the channel from Alice to Bob,
	// and then shutting down the new node we created as its no longer
	// needed. Coop close, no anchors.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssertType(
		ctxt, t, net, alice, aliceChanPoint, false, false,
	)
}
