package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/stretchr/testify/require"
)

// testMultiHopLocalForceCloseOnChainHtlcTimeout tests that in a multi-hop HTLC
// scenario, if the node that extended the HTLC to the final node closes their
// commitment on-chain early, then it eventually recognizes this HTLC as one
// that's timed out. At this point, the node should timeout the HTLC using the
// HTLC timeout transaction, then cancel it backwards as normal.
func testMultiHopLocalForceCloseOnChainHtlcTimeout(ht *lntemp.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, true, c, zeroConf,
	)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const htlcAmt = btcutil.Amount(300_000)

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := ht.Random32Bytes()
	req := &routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	ht.AssertActiveHtlcs(alice, payHash)
	ht.AssertActiveHtlcs(bob, payHash)
	ht.AssertActiveHtlcs(carol, payHash)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	hasAnchors := commitTypeHasAnchors(c)
	stream, _ := ht.CloseChannelAssertPending(bob, bobChanPoint, true)
	closeTx := ht.AssertStreamChannelForceClosed(
		bob, bobChanPoint, hasAnchors, stream,
	)

	// If the channel closed has anchors, we should expect to see a sweep
	// transaction for Carol's anchor.
	htlcOutpoint := wire.OutPoint{Hash: *closeTx, Index: 0}
	bobCommitOutpoint := wire.OutPoint{Hash: *closeTx, Index: 1}
	if hasAnchors {
		htlcOutpoint.Index = 2
		bobCommitOutpoint.Index = 3
		ht.Miner.AssertNumTxsInMempool(1)
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
		ht.MineBlocksAssertNodesSync(defaultCSV - 1)

		commitSweepTx := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		txid := commitSweepTx.TxHash()
		block := ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
		ht.Miner.AssertTxInBlock(block, &txid)
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the utxo nursery.
	numBlocks := padCLTV(finalCltvDelta)
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Subtract the number of blocks already mined to confirm Bob's
		// commit sweep.
		numBlocks -= defaultCSV
	}
	ht.MineBlocksAssertNodesSync(numBlocks)

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)

	// We should also now find a transaction in the mempool, as Bob should
	// have broadcast his second layer timeout transaction.
	timeoutTx := ht.Miner.AssertOutpointInMempool(htlcOutpoint).TxHash()

	// Next, we'll mine an additional block. This should serve to confirm
	// the second layer timeout transaction.
	block := ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &timeoutTx)

	// With the second layer timeout transaction confirmed, Bob should have
	// canceled backwards the HTLC that carol sent.
	ht.AssertNumActiveHtlcs(alice, 0)

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// Bob should now broadcast a transaction that sweeps certain inputs
	// depending on the commitment type. We'll need to mine some blocks
	// before the broadcast is possible.
	resp := bob.RPC.PendingChannels()

	require.Len(ht, resp.PendingForceClosingChannels, 1)
	forceCloseChan := resp.PendingForceClosingChannels[0]
	require.Len(ht, forceCloseChan.PendingHtlcs, 1)
	pendingHtlc := forceCloseChan.PendingHtlcs[0]
	require.Positive(ht, pendingHtlc.BlocksTilMaturity)
	numBlocks = uint32(pendingHtlc.BlocksTilMaturity)

	ht.MineBlocksAssertNodesSync(numBlocks)

	// Now that the CSV/CLTV timelock has expired, the transaction should
	// either only sweep the HTLC timeout transaction, or sweep both the
	// HTLC timeout transaction and Bob's commit output depending on the
	// commitment type.
	htlcTimeoutOutpoint := wire.OutPoint{Hash: timeoutTx, Index: 0}
	sweepTx := ht.Miner.AssertOutpointInMempool(
		htlcTimeoutOutpoint,
	).TxHash()
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.Miner.AssertOutpointInMempool(bobCommitOutpoint)
	}

	block = ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &sweepTx)

	// At this point, Bob should no longer show any channels as pending
	// close.
	ht.AssertNumPendingForceClose(bob, 0)

	// Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}
