// +build rpctest

package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

// testMultiHopHtlcLocalTimeout tests that in a multi-hop HTLC scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it using the HTLC timeout transaction. Any dust HTLC's should be immediately
// canceled backwards. Once the timeout has been reached, then we should sweep
// it on-chain, and cancel the HTLC backwards.
func testMultiHopHtlcLocalTimeout(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode, c commitType) {

	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, alice, bob, true, c,
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	time.Sleep(time.Second * 1)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt    = btcutil.Amount(100)
		htlcAmt        = btcutil.Amount(30000)
		finalCltvDelta = 40
	)

	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	alicePayStream, err := alice.SendPayment(ctx)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := makeFakePayHash(t)
	payHash := makeFakePayHash(t)
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(dustHtlcAmt),
		PaymentHash:    dustPayHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	var predErr error
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.Predicate(func() bool {
		predErr = assertActiveHtlcs(nodes, dustPayHash, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default outgoing broadcast delta of zero, this will
	// be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lnd.DefaultOutgoingBroadcastDelta),
	)
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Bob's force close transaction should now be found in the mempool.
	bobFundingTxid, err := lnd.GetChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	closeTxid, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find closing txid: %v", err)
	}
	assertSpendingTxInMempool(
		t, net.Miner.Node, minerMempoolTimeout, wire.OutPoint{
			Hash:  *bobFundingTxid,
			Index: bobChanPoint.OutputIndex,
		},
	)

	// Mine a block to confirm the closing transaction.
	mineBlocks(t, net, 1, 1)

	// At this point, Bob should have canceled backwards the dust HTLC
	// that we sent earlier. This means Alice should now only have a single
	// HTLC on her channel.
	nodes = []*lntest.HarnessNode{alice}
	err = wait.Predicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transaction to be broadcast due to the expiry being reached.
	htlcTimeout, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's htlc timeout tx: %v", err)
	}

	// We'll mine the remaining blocks in order to generate the sweep
	// transaction of Bob's commitment output.
	mineBlocks(t, net, defaultCSV, 1)
	assertSpendingTxInMempool(
		t, net.Miner.Node, minerMempoolTimeout, wire.OutPoint{
			Hash:  *closeTxid,
			Index: 1,
		},
	)

	// Bob's pending channel report should show that he has a commitment
	// output awaiting sweeping, and also that there's an outgoing HTLC
	// output pending.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	pendingChanResp, err := bob.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}

	if len(pendingChanResp.PendingForceClosingChannels) == 0 {
		t.Fatalf("bob should have pending for close chan but doesn't")
	}
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.LimboBalance == 0 {
		t.Fatalf("bob should have nonzero limbo balance instead "+
			"has: %v", forceCloseChan.LimboBalance)
	}
	if len(forceCloseChan.PendingHtlcs) == 0 {
		t.Fatalf("bob should have pending htlc but doesn't")
	}

	// Now we'll mine an additional block, which should confirm Bob's commit
	// sweep. This block should also prompt Bob to broadcast their second
	// layer sweep due to the CSV on the HTLC timeout output.
	mineBlocks(t, net, 1, 1)
	assertSpendingTxInMempool(
		t, net.Miner.Node, minerMempoolTimeout, wire.OutPoint{
			Hash:  *htlcTimeout,
			Index: 0,
		},
	)

	// The block should have confirmed Bob's HTLC timeout transaction.
	// Therefore, at this point, there should be no active HTLC's on the
	// commitment transaction from Alice -> Bob.
	nodes = []*lntest.HarnessNode{alice}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, 0)
		if predErr != nil {
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("alice's channel still has active htlc's: %v", predErr)
	}

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is to be swept.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	pendingChanResp, err = bob.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	forceCloseChan = pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.PendingHtlcs[0].Stage != 2 {
		t.Fatalf("bob's htlc should have advanced to the second stage: %v", err)
	}

	// Next, we'll mine a final block that should confirm the second-layer
	// sweeping transaction.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err = bob.PendingChannels(ctxt, pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending "+
				"channels but shouldn't: %v",
				spew.Sdump(pendingChanResp))
			return false
		}

		return true

	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, alice, aliceChanPoint, false)
}
