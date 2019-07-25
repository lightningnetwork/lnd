// +build rpctest

package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
)

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we're forced to go to chain with an incoming HTLC, then when we find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain in
// order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
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
	preimage := lntypes.Preimage{1, 2, 3}
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

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	bobForceClose := closeChannelAndAssert(ctxt, t, net, net.Bob,
		aliceChanPoint, true)

	// Alice will sweep her output immediately.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find alice's sweep tx in miner mempool: %v",
			err)
	}

	// Suspend Bob to force Carol to go to chain.
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

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry -
		lnd.DefaultIncomingBroadcastDelta))

	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// Carol's commitment transaction should now be in the mempool.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}
	bobFundingTxid, err := lnd.GetChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The tx should be spending from the funding transaction,
	commitHash := txids[0]
	tx1, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx1.MsgTx().TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending fundingtx: %v",
			spew.Sdump(tx1))
	}

	// Mine a block that should confirm the commit tx.
	block := mineBlocks(t, net, 1, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, commitHash)

	// Restart bob again.
	if err := restartBob(); err != nil {
		t.Fatalf("unable to restart bob: %v", err)
	}

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

	// Mine a block to confirm the two transactions (+ the coinbase).
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
	// will extract the preimage and broadcast a second level tx to claim
	// the HTLC in his (already closed) channel with Alice.
	bobSecondLvlTx, err := waitForTxInMempool(net.Miner.Node,
		minerMempoolTimeout)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// It should spend from the commitment in the channel with Alice.
	tx, err := net.Miner.Node.GetRawTransaction(bobSecondLvlTx)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}

	if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash != *bobForceClose {
		t.Fatalf("tx did not spend from bob's force close tx")
	}

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
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

		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending for " +
				"close chan but doesn't")
			return false
		}

		for _, forceCloseChan := range pendingChanResp.PendingForceClosingChannels {
			if forceCloseChan.Channel.LocalBalance != 0 {
				continue
			}

			if len(forceCloseChan.PendingHtlcs) != 1 {
				predErr = fmt.Errorf("bob should have pending htlc " +
					"but doesn't")
				return false
			}
			stage := forceCloseChan.PendingHtlcs[0].Stage
			if stage != 1 {
				predErr = fmt.Errorf("bob's htlc should have "+
					"advanced to the first stage but was "+
					"stage: %v", stage)
				return false
			}
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We'll now mine a block which should confirm Bob's second layer
	// transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, bobSecondLvlTx)

	// Keep track of Bob's second level maturity, and decrement our track
	// of Carol's.
	bobSecondLevelCSV := uint32(defaultCSV)
	carolSecondLevelCSV--

	// If we then mine 3 additional blocks, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	if _, err := net.Miner.Node.Generate(carolSecondLevelCSV); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	bobSecondLevelCSV -= carolSecondLevelCSV

	carolSweep, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's sweeping transaction: %v", err)
	}

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output.
	block = mineBlocks(t, net, bobSecondLevelCSV, 1)[0]
	assertTxInBlock(t, block, carolSweep)

	bobSweep, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's sweeping transaction")
	}

	// Make sure it spends from the second level tx.
	tx, err = net.Miner.Node.GetRawTransaction(bobSweep)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash != *bobSecondLvlTx {
		t.Fatalf("tx did not spend from bob's second level tx")
	}

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, bobSweep)

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
		req := &lnrpc.ListChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		chanInfo, err := net.Bob.ListChannels(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for open "+
				"channels: %v", err)
			return false
		}
		if len(chanInfo.Channels) != 0 {
			predErr = fmt.Errorf("Bob should have no open "+
				"channels, instead he has %v",
				len(chanInfo.Channels))
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Also Carol should have no channels left (open nor pending).
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
			predErr = fmt.Errorf("bob carol has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		req := &lnrpc.ListChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		chanInfo, err := carol.ListChannels(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for open "+
				"channels: %v", err)
			return false
		}
		if len(chanInfo.Channels) != 0 {
			predErr = fmt.Errorf("carol should have no open "+
				"channels, instead she has %v",
				len(chanInfo.Channels))
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}
}

// waitForInvoiceAccepted waits until the specified invoice moved to the
// accepted state by the node.
func waitForInvoiceAccepted(t *harnessTest, node *lntest.HarnessNode,
	payHash lntypes.Hash) {

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	invoiceUpdates, err := node.SubscribeSingleInvoice(ctx,
		&invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: payHash[:],
		},
	)
	if err != nil {
		t.Fatalf("subscribe single invoice: %v", err)
	}

	for {
		update, err := invoiceUpdates.Recv()
		if err != nil {
			t.Fatalf("invoice update err: %v", err)
		}
		if update.State == lnrpc.Invoice_ACCEPTED {
			break
		}
	}
}
