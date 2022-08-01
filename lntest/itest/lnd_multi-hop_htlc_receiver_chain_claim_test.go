package itest

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMultiHopReceiverChainClaim tests that in the multi-hop setting, if the
// receiver of an HTLC knows the preimage, but wasn't able to settle the HTLC
// off-chain, then it goes on chain to claim the HTLC uing the HTLC success
// transaction. In this scenario, the node that sent the outgoing HTLC should
// extract the preimage from the sweep transaction, and finish settling the
// HTLC backwards into the route.
func testMultiHopReceiverChainClaim(ht *lntemp.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Now we'll mine enough blocks to prompt carol to actually go to the
	// chain in order to sweep her HTLC since the value is high enough.
	// TODO(roasbeef): modify once go to chain policy changes
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineBlocksAssertNodesSync(numBlocks)

	// At this point, Carol should broadcast her active commitment
	// transaction in order to go to the chain and sweep her HTLC. If there
	// are anchors, Carol also sweeps hers.
	expectedTxes := 1
	hasAnchors := commitTypeHasAnchors(c)
	if hasAnchors {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Confirm the commitment.
	ht.Miner.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, a series of transactions
	// should be broadcast by Bob and Carol. When Bob notices Carol's second
	// level transaction in the mempool, he will extract the preimage and
	// settle the HTLC back off-chain.
	switch c {
	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his output in the channel with
	// Carol.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his output in the channel with
	// Carol, and another sweep tx to sweep his anchor output.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 3

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweep tx to sweep his anchor output. Bob's commit
	// output can't be swept yet as he's incurring an additional CLTV from
	// being the channel initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// All transactions should be spending from the commitment transaction.
	txes := ht.Miner.GetNumTxsFromMempool(expectedTxes)
	ht.AssertAllTxesSpendFrom(txes, closingTxid)

	// We'll now mine an additional block which should confirm both the
	// second layer transactions.
	ht.MineBlocksAssertNodesSync(1)

	// TODO(roasbeef): assert bob pending state as well

	// Carol's pending channel report should now show two outputs under
	// limbo: her commitment output, as well as the second-layer claim
	// output, and the pending HTLC should also now be in stage 2.
	ht.AssertNumHTLCsAndStage(carol, bobChanPoint, 1, 2)

	// Once the second-level transaction confirmed, Bob should have
	// extracted the preimage from the chain, and sent it back to Alice,
	// clearing the HTLC off-chain.
	ht.AssertNumActiveHtlcs(alice, 0)

	// If we mine 4 additional blocks, then Carol can sweep the second level
	// HTLC output.
	ht.MineBlocksAssertNodesSync(defaultCSV)

	// We should have a new transaction in the mempool.
	ht.Miner.AssertNumTxsInMempool(1)

	// Finally, if we mine an additional block to confirm these two sweep
	// transactions, Carol should not show a pending channel in her report
	// afterwards.
	ht.MineBlocksAssertNodesSync(1)
	ht.AssertNumPendingForceClose(carol, 0)

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	ht.AssertInvoiceSettled(carol, carolInvoice.PaymentAddr)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Bob still has his commit output to sweep to since he incurred
		// an additional CLTV from being the channel initiator of a
		// script-enforced leased channel, regardless of whether he
		// forced closed the channel or not.
		pendingChanResp := bob.RPC.PendingChannels()

		require.Len(ht, pendingChanResp.PendingForceClosingChannels, 1)
		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.LimboBalance)
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// TODO: Bob still shows a pending HTLC at this point when he
		// shouldn't, as he already extracted the preimage from Carol's
		// claim.
		// require.Len(t.t, forceCloseChan.PendingHtlcs, 0)

		// Mine enough blocks for Bob's commit output's CLTV to expire
		// and sweep it.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocksAssertNodesSync(numBlocks)
		commitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		ht.Miner.AssertOutpointInMempool(commitOutpoint)
		ht.MineBlocksAssertNodesSync(1)
	}

	ht.AssertNumPendingForceClose(bob, 0)

	// We'll close out the channel between Alice and Bob, then shutdown
	// carol to conclude the test.
	ht.CloseChannel(alice, aliceChanPoint)
}
