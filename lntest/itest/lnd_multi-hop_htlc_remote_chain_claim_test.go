package itest

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMultiHopHtlcRemoteChainClaim tests that in the multi-hop HTLC scenario,
// if the remote party goes to chain while we have an incoming HTLC, then when
// we found out the preimage via the witness beacon, we properly settle the
// HTLC directly on-chain using the preimage in order to ensure that we don't
// lose any funds.
func testMultiHopHtlcRemoteChainClaim(ht *lntest.HarnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c,
	)
	defer ht.Shutdown(carol)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	preimage := lntypes.Preimage{1, 2, 5}
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: 40,
		Hash:       payHash[:],
	}
	carolInvoice := ht.AddHoldInvoice(invoiceReq, carol)

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPayment(alice, req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(carol, payHash, lnrpc.Invoice_ACCEPTED)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	hasAnchors := commitTypeHasAnchors(c)
	aliceForceClose := ht.CloseChannelAndAssertType(
		alice, aliceChanPoint, hasAnchors, true,
	)

	// Wait for the channel to be marked pending force close.
	ht.AssertNumChannelPendingForceClose(alice, 1)

	// After closeChannelAndAssertType returns, it has mined a block so now
	// bob will attempt to redeem his anchor commitment (if the channel
	// type is of that type).
	if hasAnchors {
		ht.AssertNumTxsInMempool(1)
	}

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Mine enough blocks for Alice to sweep her funds from the
		// force closed channel. closeChannelAndAssertType() already
		// mined a block containing the commitment tx and the commit
		// sweep tx will be broadcast immediately before it can be
		// included in a block, so mine one less than defaultCSV in
		// order to perform mempool assertions.
		ht.MineBlocks(defaultCSV - 1)

		// Alice should now sweep her funds.
		ht.AssertNumTxsInMempool(1)
	}

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	ht.SettleInvoice(carol, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		numBlocks -= defaultCSV
	}

	ht.MineBlocks(numBlocks)

	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}

	// Carol's commitment transaction should now be in the mempool. If
	// there are anchors, Carol also sweeps her anchor.
	ht.AssertNumTxsInMempool(expectedTxes)

	// The closing transaction should be spending from the funding
	// transaction.
	closingTx := ht.AssertTxInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block, which should contain: the commitment, possibly an
	// anchor sweep and the coinbase tx.
	block := ht.MineBlocksAndAssertTx(1, expectedTxes)[0]
	ht.AssertTxInBlock(block, &closingTxid)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, we should expect Bob and
	// Carol to broadcast some transactions depending on the channel
	// commitment type.
	switch c {
	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his commitment output.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his commitment output and
	// another to sweep his anchor output.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 3

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his anchor output. Bob can't
	// sweep his commitment output yet as he has incurred an additional CLTV
	// due to being the channel initiator of a force closed script-enforced
	// leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}
	txes := ht.GetNumTxsFromMempool(expectedTxes)

	// All transactions should be pending from the commitment transaction.
	ht.AssertAllTxesSpendFrom(txes, closingTxid)

	// Mine a block to confirm the two transactions (+ coinbase).
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := uint32(defaultCSV)

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a sweep tx to directly claim
	// the HTLC in his (already closed) channel with Alice.
	bobHtlcSweep := ht.GetNumTxsFromMempool(1)[0]
	bobHtlcSweepTxid := bobHtlcSweep.TxHash()

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobHtlcSweep, *aliceForceClose)

	// We'll now mine a block which should confirm Bob's HTLC sweep
	// transaction.
	block = ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, &bobHtlcSweepTxid)
	carolSecondLevelCSV--

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts for the Bob-Carol channel have been
	// fully resolved
	aliceBobPendingChansLeft := 0
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		aliceBobPendingChansLeft = 1
	}
	for _, node := range []*lntest.HarnessNode{alice, bob} {
		ht.AssertNumChannelPendingForceClose(
			node, aliceBobPendingChansLeft,
		)
	}

	// If we then mine 3 additional blocks, Carol's second level tx will
	// mature, and she should pull the funds.
	ht.MineBlocks(carolSecondLevelCSV)
	carolSweep := ht.AssertNumTxsInMempool(1)[0]

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	block = ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, carolSweep)
	ht.AssertNumChannelPendingForceClose(carol, 0)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := ht.GetPendingChannels(alice)
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *aliceForceClose, Index: 3,
		}
		aliceCommitSweep := ht.AssertTxInMempool(aliceCommitOutpoint)
		aliceCommitSweepTxid := aliceCommitSweep.TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.AssertTxInMempool(bobCommitOutpoint)
		bobCommitSweepTxid := bobCommitSweep.TxHash()

		// Confirm their sweeps.
		block := ht.MineBlocksAndAssertTx(1, 2)[0]
		ht.AssertTxInBlock(block, &aliceCommitSweepTxid)
		ht.AssertTxInBlock(block, &bobCommitSweepTxid)

		// Alice and Bob should not show any pending channels anymore as
		// they have been fully resolved.
		for _, node := range []*lntest.HarnessNode{alice, bob} {
			ht.AssertNumChannelPendingForceClose(node, 0)
		}
	}

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	invoice := ht.AssertInvoiceState(carol, payHash, lnrpc.Invoice_SETTLED)
	require.Equal(ht, int64(invoiceAmt), invoice.AmtPaidSat)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}
