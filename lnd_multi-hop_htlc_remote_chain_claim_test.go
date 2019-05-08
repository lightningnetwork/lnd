// +build rpctest

package lnd

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testMultiHopHtlcRemoteChainClaim tests that in the multi-hop HTLC scenario,
// if the remote party goes to chain while we have an incoming HTLC, then when
// we found out the preimage via the witness beacon, we properly settle the
// HTLC on-chain in order to ensure that we don't lose any funds.
func testMultiHopHtlcRemoteChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, true,
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With the network active, we'll now add a new invoice at Carol's end.
	const invoiceAmt = 100000
	invoiceReq := &lnrpc.Invoice{
		Value:      invoiceAmt,
		CltvExpiry: 40,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	carolInvoice, err := carol.AddInvoice(ctxt, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
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

	// We'll now wait until all 3 nodes have the HTLC as just sent fully
	// locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, carolInvoice.RHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	aliceForceClose := closeChannelAndAssert(ctxt, t, net, net.Alice,
		aliceChanPoint, true)

	// Wait for the channel to be marked pending force close.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForChannelPendingForceClose(ctxt, net.Alice, aliceChanPoint)
	if err != nil {
		t.Fatalf("channel not pending force close: %v", err)
	}

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel.
	_, err = net.Miner.Node.Generate(defaultCSV)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Alice should now sweep her funds.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find sweeping tx in mempool: %v", err)
	}

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := uint32(invoiceReq.CltvExpiry-
		defaultIncomingBroadcastDelta) - defaultCSV

	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// Carol's commitment transaction should now be in the mempool.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}
	bobFundingTxid, err := getChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The transaction should be spending from the funding transaction
	commitHash := txids[0]
	tx1, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx1.MsgTx().TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending fundingtx: %v",
			spew.Sdump(tx1))
	}

	// Mine a block, which should contain the commitment.
	block := mineBlocks(t, net, 1, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, commitHash)

	// After the force close transacion is mined, Carol should broadcast
	// her second level HTLC transacion. Bob will broadcast a sweep tx to
	// sweep his output in the channel with Carol. He can do this
	// immediately, as the output is not timelocked since Carol was the one
	// force closing.
	commitSpends, err := waitForNTxsInMempool(net.Miner.Node, 2,
		minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// Both Carol's second level transaction and Bob's sweep should be
	// spending from the commitment transaction.
	for _, txid := range commitSpends {
		tx, err := net.Miner.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to get txn: %v", err)
		}

		if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash != *commitHash {
			t.Fatalf("tx did not spend from commitment tx")
		}
	}

	// Mine a block to confirm the two transactions (+ coinbase).
	block = mineBlocks(t, net, 1, 2)[0]
	if len(block.Transactions) != 3 {
		t.Fatalf("expected 3 transactions in block, got %v",
			len(block.Transactions))
	}
	for _, txid := range commitSpends {
		assertTxInBlock(t, block, txid)
	}

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := uint32(defaultCSV)

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a sweep tx to directly claim
	// the HTLC in his (already closed) channel with Alice.
	bobHtlcSweep, err := waitForTxInMempool(net.Miner.Node,
		minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// It should spend from the commitment in the channel with Alice.
	tx, err := net.Miner.Node.GetRawTransaction(bobHtlcSweep)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash != *aliceForceClose {
		t.Fatalf("tx did not spend from alice's force close tx")
	}

	// We'll now mine a block which should confirm Bob's HTLC sweep
	// transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, bobHtlcSweep)
	carolSecondLevelCSV--

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts have been fully resolved, and show no
	// pending close channels.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// If we then mine 3 additional blocks, Carol's second level tx will
	// mature, and she should pull the funds.
	if _, err := net.Miner.Node.Generate(carolSecondLevelCSV); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	carolSweep, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's sweeping transaction: %v", err)
	}

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, carolSweep)

	pendingChansRequest = &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := carol.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("carol still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
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
}
