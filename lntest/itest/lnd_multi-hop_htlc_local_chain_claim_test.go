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

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(ht *lntemp.HarnessTest,
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
		CltvExpiry: 40,
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

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	hasAnchors := commitTypeHasAnchors(c)
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchors, closeStream,
	)

	var expectedTxes int
	switch c {
	// Alice will sweep her commitment output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Alice will sweep her commitment and anchor output immediately.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 2

	// Alice will sweep her anchor output immediately. Her commitment
	// output cannot be swept yet as it has incurred an additional CLTV due
	// to being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 1

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Suspend Bob to force Carol to go to chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry -
		lncfg.DefaultIncomingBroadcastDelta))
	ht.MineBlocksAssertNodesSync(numBlocks)

	// Carol's commitment transaction should now be in the mempool. If
	// there is an anchor, Carol will sweep that too.
	if commitTypeHasAnchors(c) {
		expectedTxes = 2
	}
	ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Look up the closing transaction. It should be spending from the
	// funding transaction,
	closingTx := ht.Miner.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block that should confirm the commit tx, the anchor if
	// present and the coinbase.
	block := ht.Miner.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	ht.Miner.AssertTxInBlock(block, &closingTxid)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, transactions will be
	// broadcast by both Bob and Carol.
	switch c {
	// Carol will broadcast her second level HTLC transaction and Bob will
	// sweep his commitment output.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2

	// Carol will broadcast her second level HTLC transaction and Bob will
	// sweep his commitment and anchor output.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 3

	// Carol will broadcast her second level HTLC transaction and anchor
	// sweep, and Bob will sweep his anchor output. Bob can't sweep his
	// commitment output yet as it has incurred an additional CLTV due to
	// being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 2

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	txes := ht.Miner.GetNumTxsFromMempool(expectedTxes)

	// Both Carol's second level transaction and Bob's sweep should be
	// spending from the commitment transaction.
	ht.AssertAllTxesSpendFrom(txes, closingTxid)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the expected transactions (+ the coinbase).
	block = ht.Miner.MineBlocksAndAssertNumTxes(1, expectedTxes)[0]
	require.Len(ht, block.Transactions, expectedTxes+1)

	// For non-anchor channel types, the nursery will handle sweeping the
	// second level output, and it will wait one extra block before
	// sweeping it.
	secondLevelMaturity := uint32(defaultCSV)

	// If this is a channel of the anchor type, we will subtract one block
	// from the default CSV, as the Sweeper will handle the input, and the
	// Sweeper sweeps the input as soon as the lock expires.
	if hasAnchors {
		secondLevelMaturity = defaultCSV - 1
	}

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := secondLevelMaturity

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a second level tx to claim
	// the HTLC in his (already closed) channel with Alice.
	bobSecondLvlTx := ht.Miner.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobSecondLvlTx, *bobForceClose)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
	numPendingChans := 1
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		numPendingChans++
	}
	ht.AssertNumPendingForceClose(bob, numPendingChans)
	ht.AssertNumHTLCsAndStage(bob, aliceChanPoint, 1, 1)

	// We'll now mine a block which should confirm Bob's second layer
	// transaction.
	block = ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
	bobSecondLvlTxid := bobSecondLvlTx.TxHash()
	ht.Miner.AssertTxInBlock(block, &bobSecondLvlTxid)

	// Keep track of Bob's second level maturity, and decrement our track
	// of Carol's.
	bobSecondLevelCSV := secondLevelMaturity
	carolSecondLevelCSV--

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine 3 additional blocks, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	ht.MineBlocksAssertNodesSync(carolSecondLevelCSV)
	carolSweep := ht.Miner.AssertNumTxsInMempool(1)[0]

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output.
	bobSecondLevelCSV -= carolSecondLevelCSV
	block = ht.Miner.MineBlocksAndAssertNumTxes(bobSecondLevelCSV, 1)[0]
	ht.Miner.AssertTxInBlock(block, carolSweep)

	bobSweep := ht.Miner.GetNumTxsFromMempool(1)[0]
	bobSweepTxid := bobSweep.TxHash()

	// Make sure it spends from the second level tx.
	ht.AssertTxSpendFrom(bobSweep, bobSecondLvlTxid)

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	block = ht.Miner.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, &bobSweepTxid)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 1)
		}

		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineBlocksAssertNodesSync(numBlocks)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: *bobForceClose, Index: 3,
		}
		aliceCommitSweep := ht.Miner.AssertOutpointInMempool(
			aliceCommitOutpoint,
		).TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := ht.Miner.AssertOutpointInMempool(
			bobCommitOutpoint,
		).TxHash()

		// Confirm their sweeps.
		block := ht.Miner.MineBlocksAndAssertNumTxes(1, 2)[0]
		ht.Miner.AssertTxInBlock(block, &aliceCommitSweep)
		ht.Miner.AssertTxInBlock(block, &bobCommitSweep)
	}

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}
