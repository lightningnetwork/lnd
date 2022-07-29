package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMultiHopRemoteForceCloseOnChainHtlcTimeout tests that if we extend a
// multi-hop HTLC, and the final destination of the HTLC force closes the
// channel, then we properly timeout the HTLC directly on *their* commitment
// transaction once the timeout has expired. Once we sweep the transaction, we
// should also cancel back the initial HTLC.
func testMultiHopRemoteForceCloseOnChainHtlcTimeout(ht *lntemp.HarnessTest,
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
	const (
		finalCltvDelta = 40
		htlcAmt        = btcutil.Amount(30000)
	)

	// We'll now send a single HTLC across our multi-hop network.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(htlcAmt),
		CltvExpiry: 40,
		Hash:       payHash[:],
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// At this point, we'll now instruct Carol to force close the
	// transaction. This will let us exercise that Bob is able to sweep the
	// expired HTLC on Carol's version of the commitment transaction. If
	// Carol has an anchor, it will be swept too.
	hasAnchors := commitTypeHasAnchors(c)
	closeStream, _ := ht.CloseChannelAssertPending(
		carol, bobChanPoint, true,
	)
	closeTx := ht.AssertStreamChannelForceClosed(
		carol, bobChanPoint, hasAnchors, closeStream,
	)

	// At this point, Bob should have a pending force close channel as
	// Carol has gone directly to chain.
	ht.AssertNumPendingForceClose(bob, 1)

	var expectedTxes int
	switch c {
	// Bob can sweep his commit output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Bob can sweep his commit and anchor outputs immediately.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 2

	// Bob can't sweep his commit output yet as he was the initiator of a
	// script-enforced leased channel, so he'll always incur the additional
	// CLTV. He can still sweep his anchor output however.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 1

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Next, we'll mine enough blocks for the HTLC to expire. At this
	// point, Bob should hand off the output to his internal utxo nursery,
	// which will broadcast a sweep transaction.
	numBlocks := padCLTV(finalCltvDelta - 1)
	ht.MineBlocksAssertNodesSync(numBlocks)

	// If we check Bob's pending channel report, it should show that he has
	// a single HTLC that's now in the second stage, as skip the initial
	// first stage since this is a direct HTLC.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// We need to generate an additional block to trigger the sweep.
	ht.MineBlocksAssertNodesSync(1)

	// Bob's sweeping transaction should now be found in the mempool at
	// this point.
	sweepTx := ht.Miner.AssertNumTxsInMempool(1)[0]
	// The following issue is believed to have been resolved. Keep the
	// original comments here for future reference in case anything goes
	// wrong.
	//
	// If Bob's transaction isn't yet in the mempool, then due to
	// internal message passing and the low period between blocks
	// being mined, it may have been detected as a late
	// registration. As a result, we'll mine another block and
	// repeat the check. If it doesn't go through this time, then
	// we'll fail.
	// TODO(halseth): can we use waitForChannelPendingForceClose to
	// avoid this hack?

	// If we mine an additional block, then this should confirm Bob's
	// transaction which sweeps the direct HTLC output.
	block := ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, sweepTx)

	// Now that the sweeping transaction has been confirmed, Bob should
	// cancel back that HTLC. As a result, Alice should not know of any
	// active HTLC's.
	ht.AssertNumActiveHtlcs(alice, 0)

	// Now we'll check Bob's pending channel report. Since this was Carol's
	// commitment, he doesn't have to wait for any CSV delays, but he may
	// still need to wait for a CLTV on his commit output to expire
	// depending on the commitment type.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		resp := bob.RPC.PendingChannels()

		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocksAssertNodesSync(numBlocks)

		bobCommitOutpoint := wire.OutPoint{Hash: *closeTx, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		)
		bobCommitSweepTxid := bobCommitSweep.TxHash()
		block := ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
		ht.Miner.AssertTxInBlock(block, &bobCommitSweepTxid)
	}
	ht.AssertNumPendingForceClose(bob, 0)

	// While we're here, we assert that our expired invoice's state is
	// correctly updated, and can no longer be settled.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_CANCELED)

	// We'll close out the test by closing the channel from Alice to Bob,
	// and then shutting down the new node we created as its no longer
	// needed. Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}
