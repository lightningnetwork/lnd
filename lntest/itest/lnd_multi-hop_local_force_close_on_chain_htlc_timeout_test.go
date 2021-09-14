package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testMultiHopLocalForceCloseOnChainHtlcTimeout tests that in a multi-hop HTLC
// scenario, if the node that extended the HTLC to the final node closes their
// commitment on-chain early, then it eventually recognizes this HTLC as one
// that's timed out. At this point, the node should timeout the HTLC using the
// HTLC timeout transaction, then cancel it backwards as normal.
func testMultiHopLocalForceCloseOnChainHtlcTimeout(net *lntest.NetworkHarness,
	t *harnessTest, alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

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
		htlcAmt        = btcutil.Amount(300_000)
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

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	net.SetFeeEstimate(30000)

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	hasAnchors := commitTypeHasAnchors(c)
	closeTx := closeChannelAndAssertType(
		t, net, bob, bobChanPoint, hasAnchors, true,
	)

	// At this point, Bob should have a pending force close channel as he
	// just went to chain.
	err = waitForNumChannelPendingForceClose(
		bob, 1, func(c *lnrpcForceCloseChannel) error {
			if c.LimboBalance == 0 {
				return fmt.Errorf("bob should have nonzero "+
					"limbo balance instead has: %v",
					c.LimboBalance)
			}

			return nil
		},
	)
	require.NoError(t.t, err)

	// If the channel closed has anchors, we should expect to see a sweep
	// transaction for Carol's anchor.
	htlcOutpoint := wire.OutPoint{Hash: *closeTx, Index: 0}
	bobCommitOutpoint := wire.OutPoint{Hash: *closeTx, Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		bobCommitOutpoint.Index = 3
		_, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
		require.NoError(t.t, err)
	}

	// Before the HTLC times out, we'll need to assert that Bob broadcasts a
	// sweep transaction for his commit output. Note that if the channel has
	// a script-enforced lease, then Bob will have to wait for an additional
	// CLTV before sweeping it.
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// The sweep is broadcast on the block immediately before the
		// CSV expires and the commitment was already mined inside
		// closeChannelAndAssertType(), so mine one block less than
		// defaultCSV in order to perform mempool assertions.
		_, err = net.Miner.Client.Generate(defaultCSV - 1)
		require.NoError(t.t, err)

		commitSweepTx := assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout,
			bobCommitOutpoint,
		)
		blocks := mineBlocks(t, net, 1, 1)
		assertTxInBlock(t, blocks[0], &commitSweepTx)
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the utxo nursery.
	numBlocks := padCLTV(finalCltvDelta)
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Subtract the number of blocks already mined to confirm Bob's
		// commit sweep.
		numBlocks -= defaultCSV
	}
	_, err = net.Miner.Client.Generate(numBlocks)
	require.NoError(t.t, err)

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	err = waitForNumChannelPendingForceClose(
		bob, 1, func(c *lnrpcForceCloseChannel) error {
			if len(c.PendingHtlcs) != 1 {
				return fmt.Errorf("bob should have pending " +
					"htlc but doesn't")
			}

			if c.PendingHtlcs[0].Stage != 1 {
				return fmt.Errorf("bob's htlc should have "+
					"advanced to the first stage: %v", err)
			}

			return nil
		},
	)
	require.NoError(t.t, err)

	// We should also now find a transaction in the mempool, as Bob should
	// have broadcast his second layer timeout transaction.
	timeoutTx := assertSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, htlcOutpoint,
	)

	// Next, we'll mine an additional block. This should serve to confirm
	// the second layer timeout transaction.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, &timeoutTx)

	// With the second layer timeout transaction confirmed, Bob should have
	// canceled backwards the HTLC that carol sent.
	nodes = []*lntest.HarnessNode{alice}
	err = wait.NoError(func() error {
		return assertNumActiveHtlcs(nodes, 0)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	err = waitForNumChannelPendingForceClose(
		bob, 1, func(c *lnrpcForceCloseChannel) error {
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

	// Bob should now broadcast a transaction that sweeps certain inputs
	// depending on the commitment type. We'll need to mine some blocks
	// before the broadcast is possible.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := bob.PendingChannels(ctxt, &lnrpc.PendingChannelsRequest{})
	require.NoError(t.t, err)

	require.Len(t.t, resp.PendingForceClosingChannels, 1)
	forceCloseChan := resp.PendingForceClosingChannels[0]
	require.Len(t.t, forceCloseChan.PendingHtlcs, 1)
	pendingHtlc := forceCloseChan.PendingHtlcs[0]
	require.Positive(t.t, pendingHtlc.BlocksTilMaturity)
	numBlocks = uint32(pendingHtlc.BlocksTilMaturity)

	_, err = net.Miner.Client.Generate(numBlocks)
	require.NoError(t.t, err)

	// Now that the CSV/CLTV timelock has expired, the transaction should
	// either only sweep the HTLC timeout transaction, or sweep both the
	// HTLC timeout transaction and Bob's commit output depending on the
	// commitment type.
	htlcTimeoutOutpoint := wire.OutPoint{Hash: timeoutTx, Index: 0}
	expectedInputsSwept := []wire.OutPoint{htlcTimeoutOutpoint}
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		expectedInputsSwept = append(expectedInputsSwept, bobCommitOutpoint)
	}
	sweepTx := assertSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, expectedInputsSwept...,
	)
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, &sweepTx)

	// At this point, Bob should no longer show any channels as pending
	// close.
	err = waitForNumChannelPendingForceClose(bob, 0, nil)
	require.NoError(t.t, err)

	// Coop close, no anchors.
	closeChannelAndAssertType(t, net, alice, aliceChanPoint, false, false)
}
