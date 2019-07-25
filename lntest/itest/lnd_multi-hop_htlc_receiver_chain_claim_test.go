// +build rpctest

package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
)

// testMultiHopReceiverChainClaim tests that in the multi-hop setting, if the
// receiver of an HTLC knows the preimage, but wasn't able to settle the HTLC
// off-chain, then it goes on chain to claim the HTLC. In this scenario, the
// node that sent the outgoing HTLC should extract the preimage from the sweep
// transaction, and finish settling the HTLC backwards into the route.
func testMultiHopReceiverChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, false,
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.

	const invoiceAmt = 100000
	preimage := lntypes.Preimage{1, 2, 4}
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: 40,
		Hash:       payHash[:],
	}
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	carolInvoice, err := carol.AddHoldInvoice(ctxt, invoiceReq)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	alicePayStream, err := net.Alice.SendPayment(ctx)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	})
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash[:])
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	waitForInvoiceAccepted(t, carol, payHash)

	restartBob, err := net.SuspendNode(net.Bob)
	if err != nil {
		t.Fatalf("unable to suspend bob: %v", err)
	}

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	ctx, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	_, err = carol.SettleInvoice(ctx, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})
	if err != nil {
		t.Fatalf("settle invoice: %v", err)
	}

	// Now we'll mine enough blocks to prompt carol to actually go to the
	// chain in order to sweep her HTLC since the value is high enough.
	// TODO(roasbeef): modify once go to chain policy changes
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lnd.DefaultIncomingBroadcastDelta,
	))
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// At this point, Carol should broadcast her active commitment
	// transaction in order to go to the chain and sweep her HTLC.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("expected transaction not found in mempool: %v", err)
	}

	bobFundingTxid, err := lnd.GetChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}

	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The commitment transaction should be spending from the funding
	// transaction.
	commitHash := txids[0]
	tx, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	commitTx := tx.MsgTx()

	if commitTx.TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending from expected "+
			"outpoint: %v", spew.Sdump(commitTx))
	}

	// Confirm the commitment.
	mineBlocks(t, net, 1, 1)

	// Restart bob again.
	if err := restartBob(); err != nil {
		t.Fatalf("unable to restart bob: %v", err)
	}

	// After the force close transaction is mined, Carol should broadcast
	// her second level HTLC transaction. Bob will broadcast a sweep tx to
	// sweep his output in the channel with Carol. When Bob notices Carol's
	// second level transaction in the mempool, he will extract the
	// preimage and settle the HTLC back off-chain.
	secondLevelHashes, err := waitForNTxsInMempool(net.Miner.Node, 2,
		minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// Carol's second level transaction should be spending from
	// the commitment transaction.
	var secondLevelHash *chainhash.Hash
	for _, txid := range secondLevelHashes {
		tx, err := net.Miner.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to get txn: %v", err)
		}

		if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash == *commitHash {
			secondLevelHash = txid
		}
	}
	if secondLevelHash == nil {
		t.Fatalf("Carol's second level tx not found")
	}

	// We'll now mine an additional block which should confirm both the
	// second layer transactions.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	time.Sleep(time.Second * 4)

	// TODO(roasbeef): assert bob pending state as well

	// Carol's pending channel report should now show two outputs under
	// limbo: her commitment output, as well as the second-layer claim
	// output.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	pendingChanResp, err := carol.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}

	if len(pendingChanResp.PendingForceClosingChannels) == 0 {
		t.Fatalf("carol should have pending for close chan but doesn't")
	}
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.LimboBalance == 0 {
		t.Fatalf("carol should have nonzero limbo balance instead "+
			"has: %v", forceCloseChan.LimboBalance)
	}

	// The pending HTLC carol has should also now be in stage 2.
	if len(forceCloseChan.PendingHtlcs) != 1 {
		t.Fatalf("carol should have pending htlc but doesn't")
	}
	if forceCloseChan.PendingHtlcs[0].Stage != 2 {
		t.Fatalf("carol's htlc should have advanced to the second "+
			"stage: %v", err)
	}

	// Once the second-level transaction confirmed, Bob should have
	// extracted the preimage from the chain, and sent it back to Alice,
	// clearing the HTLC off-chain.
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, 0)
		if predErr != nil {
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// If we mine 4 additional blocks, then both outputs should now be
	// mature.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// We should have a new transaction in the mempool.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's sweeping transaction: %v", err)
	}

	// Finally, if we mine an additional block to confirm these two sweep
	// transactions, Carol should not show a pending channel in her report
	// afterwards.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine block: %v", err)
	}
	err = lntest.WaitPredicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err = carol.PendingChannels(ctxt, pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("carol still has pending channels: %v",
				spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	invoicesReq := &lnrpc.ListInvoiceRequest{}
	invoicesResp, err := carol.ListInvoices(ctxb, invoicesReq)
	if err != nil {
		t.Fatalf("unable to retrieve invoices: %v", err)
	}
	if len(invoicesResp.Invoices) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(invoicesResp.Invoices))
	}
	invoice := invoicesResp.Invoices[0]
	if invoice.State != lnrpc.Invoice_SETTLED {
		t.Fatalf("expected invoice to be settled on chain")
	}
	if invoice.AmtPaidSat != invoiceAmt {
		t.Fatalf("expected invoice to be settled with %d sat, got "+
			"%d sat", invoiceAmt, invoice.AmtPaidSat)
	}

	// We'll close out the channel between Alice and Bob, then shutdown
	// carol to conclude the test.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}
