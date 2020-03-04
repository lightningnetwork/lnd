// +build rpctest

package itest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
)

func testMultiHopHtlcClaims(net *lntest.NetworkHarness, t *harnessTest) {

	type testCase struct {
		name string
		test func(net *lntest.NetworkHarness, t *harnessTest, alice,
			bob *lntest.HarnessNode)
	}

	subTests := []testCase{
		{
			// bob: outgoing our commit timeout
			// carol: incoming their commit watch and see timeout
			name: "local force close immediate expiry",
			test: testMultiHopHtlcLocalTimeout,
		},
		{
			// bob: outgoing watch and see, they sweep on chain
			// carol: incoming our commit, know preimage
			name: "receiver chain claim",
			test: testMultiHopReceiverChainClaim,
		},
		{
			// bob: outgoing our commit watch and see timeout
			// carol: incoming their commit watch and see timeout
			name: "local force close on-chain htlc timeout",
			test: testMultiHopLocalForceCloseOnChainHtlcTimeout,
		},
		{
			// bob: outgoing their commit watch and see timeout
			// carol: incoming our commit watch and see timeout
			name: "remote force close on-chain htlc timeout",
			test: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
		},
		{
			// bob: outgoing our commit watch and see, they sweep on chain
			// bob: incoming our commit watch and learn preimage
			// carol: incoming their commit know preimage
			name: "local chain claim",
			test: testMultiHopHtlcLocalChainClaim,
		},
		{
			// bob: outgoing their commit watch and see, they sweep on chain
			// bob: incoming their commit watch and learn preimage
			// carol: incoming our commit know preimage
			name: "remote chain claim",
			test: testMultiHopHtlcRemoteChainClaim,
		},
	}

	args := []string{}
	alice, err := net.NewNode("Alice", args)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, alice)

	bob, err := net.NewNode("Bob", args)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, bob)

	ctxb := context.Background()
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}

	for _, subTest := range subTests {

		subTest := subTest

		success := t.t.Run(subTest.name, func(t *testing.T) {
			ht := newHarnessTest(t, net)

			subTest.test(net, ht, alice, bob)
		})
		if !success {
			return
		}
	}
}

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode) {

	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, alice, bob, false,
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

	alicePayStream, err := alice.SendPayment(ctx)
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
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.Predicate(func() bool {
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
	bobForceClose := closeChannelAndAssert(ctxt, t, net, bob,
		aliceChanPoint, true)

	// Alice will sweep her output immediately.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find alice's sweep tx in miner mempool: %v",
			err)
	}

	// Suspend Bob to force Carol to go to chain.
	restartBob, err := net.SuspendNode(bob)
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

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice, err := net.SuspendNode(alice)
	if err != nil {
		t.Fatalf("unable to suspend alice: %v", err)
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
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := bob.PendingChannels(
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

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	if err := restartAlice(); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

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

	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := bob.PendingChannels(
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
		chanInfo, err := bob.ListChannels(ctxt, req)
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
	err = wait.Predicate(func() bool {
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

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	err = checkPaymentStatus(
		ctxt, alice, preimage, lnrpc.Payment_SUCCEEDED,
	)
	if err != nil {
		t.Fatalf(err.Error())
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

// checkPaymentStatus asserts that the given node list a payment with the given
// preimage has the expected status.
func checkPaymentStatus(ctxt context.Context, node *lntest.HarnessNode,
	preimage lntypes.Preimage, status lnrpc.Payment_PaymentStatus) error {

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	paymentsResp, err := node.ListPayments(ctxt, req)
	if err != nil {
		return fmt.Errorf("error when obtaining Alice payments: %v",
			err)
	}

	payHash := preimage.Hash()
	var found bool
	for _, p := range paymentsResp.Payments {
		if p.PaymentHash != payHash.String() {
			continue
		}

		found = true
		if p.Status != status {
			return fmt.Errorf("expected payment status "+
				"%v, got %v", status, p.Status)
		}

		switch status {

		// If this expected status is SUCCEEDED, we expect the final preimage.
		case lnrpc.Payment_SUCCEEDED:
			if p.PaymentPreimage != preimage.String() {
				return fmt.Errorf("preimage doesn't match: %v vs %v",
					p.PaymentPreimage, preimage.String())
			}

		// Otherwise we expect an all-zero preimage.
		default:
			if p.PaymentPreimage != (lntypes.Preimage{}).String() {
				return fmt.Errorf("expected zero preimage, got %v",
					p.PaymentPreimage)
			}
		}

	}

	if !found {
		return fmt.Errorf("payment with payment hash %v not found "+
			"in response", payHash)
	}

	return nil
}

func createThreeHopNetwork(t *harnessTest, net *lntest.NetworkHarness,
	alice, bob *lntest.HarnessNode, carolHodl bool) (*lnrpc.ChannelPoint,
	*lnrpc.ChannelPoint, *lntest.HarnessNode) {

	ctxb := context.Background()

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := net.EnsureConnected(ctxt, alice, bob)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}

	ctxt, _ = context.WithTimeout(context.Background(), defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, alice)
	if err != nil {
		t.Fatalf("unable to send coins to Alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(context.Background(), defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, bob)
	if err != nil {
		t.Fatalf("unable to send coins to Bob: %v", err)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	aliceChanPoint := openChannelAndAssert(
		ctxt, t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = alice.WaitForNetworkChannelOpen(ctxt, aliceChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = bob.WaitForNetworkChannelOpen(ctxt, aliceChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Next, we'll create a new node "carol" and have Bob connect to her. If
	// the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := []string{}
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}
	carol, err := net.NewNode("Carol", carolFlags)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	// We'll then create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	bobChanPoint := openChannelAndAssert(
		ctxt, t, net, bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = bob.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = alice.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	return aliceChanPoint, bobChanPoint, carol
}
