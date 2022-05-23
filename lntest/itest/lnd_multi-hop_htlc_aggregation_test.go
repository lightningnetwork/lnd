package itest

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMultiHopHtlcAggregation tests that in a multi-hop HTLC scenario, if we
// force close a channel with both incoming and outgoing HTLCs, we can properly
// resolve them using the second level timeout and success transactions. In
// case of anchor channels, the second-level spends can also be aggregated and
// properly feebumped, so we'll check that as well.
func testMultiHopHtlcAggregation(ht *lntest.HarnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

	const finalCltvDelta = 40

	// First, we'll create a three hop network: Alice -> Bob -> Carol.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c,
	)
	defer ht.Shutdown(carol)

	// To ensure we have capacity in both directions of the route, we'll
	// make a fairly large payment Alice->Carol and settle it.
	const reBalanceAmt = 500_000
	invoice := &lnrpc.Invoice{Value: reBalanceAmt}
	resp := ht.AddInvoice(invoice, carol)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	stream := ht.SendPayment(alice, sendReq)
	ht.AssertPaymentStatusFromStream(stream, lnrpc.Payment_SUCCEEDED)

	// With the network active, we'll now add a new hodl invoices at both
	// Alice's and Carol's end. Make sure the cltv expiry delta is large
	// enough, otherwise Bob won't send out the outgoing htlc.
	const numInvoices = 5
	const invoiceAmt = 50_000

	var (
		carolInvoices  []*invoicesrpc.AddHoldInvoiceResp
		aliceInvoices  []*invoicesrpc.AddHoldInvoiceResp
		alicePreimages []lntypes.Preimage
		payHashes      [][]byte
		alicePayHashes [][]byte
		carolPayHashes [][]byte
	)

	// Add Carol invoices.
	for i := 0; i < numInvoices; i++ {
		preimage := lntypes.Preimage{1, 1, 1, byte(i)}
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: finalCltvDelta,
			Hash:       payHash[:],
		}
		carolInvoice := ht.AddHoldInvoice(invoiceReq, carol)

		carolInvoices = append(carolInvoices, carolInvoice)
		payHashes = append(payHashes, payHash[:])
		carolPayHashes = append(carolPayHashes, payHash[:])
	}

	// We'll give Alice's invoices a longer CLTV expiry, to ensure the
	// channel Bob<->Carol will be closed first.
	for i := 0; i < numInvoices; i++ {
		preimage := lntypes.Preimage{2, 2, 2, byte(i)}
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: 2 * finalCltvDelta,
			Hash:       payHash[:],
		}
		aliceInvoice := ht.AddHoldInvoice(invoiceReq, alice)

		aliceInvoices = append(aliceInvoices, aliceInvoice)
		alicePreimages = append(alicePreimages, preimage)
		payHashes = append(payHashes, payHash[:])
		alicePayHashes = append(alicePayHashes, payHash[:])
	}

	// Now that we've created the invoices, we'll pay them all from
	// Alice<->Carol, going through Bob. We won't wait for the response
	// however, as neither will immediately settle the payment.

	// Alice will pay all of Carol's invoices.
	for _, carolInvoice := range carolInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: carolInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		ht.SendPayment(alice, req)
	}

	// And Carol will pay Alice's.
	for _, aliceInvoice := range aliceInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: aliceInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		ht.SendPayment(carol, req)
	}

	// At this point, all 3 nodes should now the HTLCs active on their
	// channels.
	ht.AssertActiveHtlcs(alice, payHashes...)
	ht.AssertActiveHtlcs(bob, payHashes...)
	ht.AssertActiveHtlcs(carol, payHashes...)

	// Wait for Alice and Carol to mark the invoices as accepted. There is
	// a small gap to bridge between adding the htlc to the channel and
	// executing the exit hop logic.
	for _, payHash := range carolPayHashes {
		h := lntypes.Hash{}
		copy(h[:], payHash)
		ht.AssertInvoiceState(carol, h, lnrpc.Invoice_ACCEPTED)
	}

	for _, payHash := range alicePayHashes {
		h := lntypes.Hash{}
		copy(h[:], payHash)
		ht.AssertInvoiceState(alice, h, lnrpc.Invoice_ACCEPTED)
	}

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We want Carol's htlcs to expire off-chain to demonstrate bob's force
	// close. However, Carol will cancel her invoices to prevent force
	// closes, so we shut her down for now.
	restartCarol := ht.SuspendNode(carol)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the Carol's HTLCs are
	// about to timeout. With the default outgoing broadcast delta of zero,
	// this will be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineBlocks(numBlocks)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	hasAnchors := commitTypeHasAnchors(c)
	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}
	ht.AssertNumTxsInMempool(expectedTxes)

	closeTx := ht.AssertTxInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closeTxid := closeTx.TxHash()

	// Go through the closing transaction outputs, and make an index for
	// the HTLC outputs.
	successOuts := make(map[wire.OutPoint]struct{})
	timeoutOuts := make(map[wire.OutPoint]struct{})
	for i, txOut := range closeTx.TxOut {
		op := wire.OutPoint{
			Hash:  closeTxid,
			Index: uint32(i),
		}

		switch txOut.Value {
		// If this HTLC goes towards Carol, Bob will claim it with a
		// timeout Tx. In this case the value will be the invoice
		// amount.
		case invoiceAmt:
			timeoutOuts[op] = struct{}{}

		// If the HTLC has direction towards Alice, Bob will claim it
		// with the success TX when he learns the preimage. In this
		// case one extra sat will be on the output, because of the
		// routing fee.
		case invoiceAmt + 1:
			successOuts[op] = struct{}{}
		}
	}

	// Once bob has force closed, we can restart carol.
	require.NoError(ht, restartCarol())

	// Mine a block to confirm the closing transaction.
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// Let Alice settle her invoices. When Bob now gets the preimages, he
	// has no other option than to broadcast his second-level transactions
	// to claim the money.
	for _, preimage := range alicePreimages {
		ht.SettleInvoice(alice, &invoicesrpc.SettleInvoiceMsg{
			Preimage: preimage[:],
		})
	}

	switch c {
	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transactions to be broadcast due to the expiry being reached.
	// We will also expect the success transactions, since he learnt the
	// preimages from Alice. We also expect Carol to sweep her commitment
	// output.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 2*numInvoices + 1

	// In case of anchors, all success transactions will be aggregated into
	// one, the same is the case for the timeout transactions. In this case
	// Carol will also sweep her commitment and anchor output as separate
	// txs (since it will be low fee).
	case lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 4

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}
	txes := ht.GetNumTxsFromMempool(expectedTxes)

	// Since Bob can aggregate the transactions, we expect a single
	// transaction, that have multiple spends from the commitment.
	var (
		timeoutTxs []*chainhash.Hash
		successTxs []*chainhash.Hash
	)
	for _, tx := range txes {
		txid := tx.TxHash()

		for i := range tx.TxIn {
			prevOp := tx.TxIn[i].PreviousOutPoint
			if _, ok := successOuts[prevOp]; ok {
				successTxs = append(successTxs, &txid)
				break
			}

			if _, ok := timeoutOuts[prevOp]; ok {
				timeoutTxs = append(timeoutTxs, &txid)
				break
			}
		}
	}

	// In case of anchor we expect all the timeout and success second
	// levels to be aggregated into one tx. For earlier channel types, they
	// will be separate transactions.
	if hasAnchors {
		require.Len(ht, timeoutTxs, 1)
		require.Len(ht, successTxs, 1)
	} else {
		require.Len(ht, timeoutTxs, numInvoices)
		require.Len(ht, successTxs, numInvoices)
	}

	// All mempool transactions should be spending from the commitment
	// transaction.
	ht.AssertAllTxesSpendFrom(txes, closeTxid)

	// Mine a block to confirm the transactions.
	block := ht.MineBlocksAndAssertTx(1, expectedTxes)[0]
	require.Len(ht, block.Transactions, expectedTxes+1)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation,
	// or to the sweeper for sweeping.
	ht.AssertNumChannelPendingForceClose(bob, 1)

	// For this channel, we also check the number of HTLCs and the stage
	// are correct.
	pendingChanResp := ht.GetPendingChannels(bob)
	pendingChan := pendingChanResp.PendingForceClosingChannels[0]
	require.Len(ht, pendingChan.PendingHtlcs, numInvoices*2)
	require.EqualValues(ht, 2, pendingChan.PendingHtlcs[0].Stage)

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// If we then mine additional blocks, Bob can sweep his
		// commitment output.
		ht.MineBlocks(defaultCSV - 2)

		// Find the commitment sweep.
		bobCommitSweep := ht.GetNumTxsFromMempool(1)[0]
		ht.AssertTxSpendFrom(bobCommitSweep, closeTxid)

		// Also ensure it is not spending from any of the HTLC output.
		for _, txin := range bobCommitSweep.TxIn {
			for _, timeoutTx := range timeoutTxs {
				require.NotEqual(ht, *timeoutTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of timeout tx")
			}

			for _, successTx := range successTxs {
				require.NotEqual(ht, *successTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of success tx")
			}
		}
	}

	switch c {
	// In case this is a non-anchor channel type, we must mine 2 blocks, as
	// the nursery waits an extra block before sweeping. Before the blocks
	// are mined, we should expect to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_LEGACY:
		ht.MineBlocksAndAssertTx(2, 1)

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output. Before the blocks are mined, we should expect
	// to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_ANCHORS:
		ht.MineBlocksAndAssertTx(1, 1)

	// Since Bob is the initiator of the Bob-Carol script-enforced leased
	// channel, he incurs an additional CLTV when sweeping outputs back to
	// his wallet. We'll need to mine enough blocks for the timelock to
	// expire to prompt his broadcast.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		resp := ht.GetPendingChannels(bob)
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)
		ht.MineBlocks(uint32(forceCloseChan.BlocksTilMaturity))

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Make sure it spends from the second level tx.
	secondLevelSweep := ht.GetNumTxsFromMempool(1)[0]
	bobSweep := secondLevelSweep.TxHash()

	// It should be sweeping all the second-level outputs.
	var secondLvlSpends int
	for _, txin := range secondLevelSweep.TxIn {
		for _, timeoutTx := range timeoutTxs {
			if *timeoutTx == txin.PreviousOutPoint.Hash {
				secondLvlSpends++
			}
		}

		for _, successTx := range successTxs {
			if *successTx == txin.PreviousOutPoint.Hash {
				secondLvlSpends++
			}
		}
	}

	require.Equal(ht, 2*numInvoices, secondLvlSpends)

	// When we mine one additional block, that will confirm Bob's second
	// level sweep.  Now Bob should have no pending channels anymore, as
	// this just resolved it by the confirmation of the sweep transaction.
	block = ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, &bobSweep)
	ht.AssertNumChannelPendingForceClose(bob, 0)

	// THe channel with Alice is still open.
	ht.AssertNodeNumChannels(bob, 1)

	// Carol should have no channels left (open nor pending).
	ht.AssertNumChannelPendingForceClose(carol, 0)
	ht.AssertNodeNumChannels(carol, 0)

	// Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint, false)
}
