// +build rpctest

package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

// testMultiHopLocalForceCloseOnChainHtlcTimeout tests that in a multi-hop HTLC
// scenario, if the node that extended the HTLC to the final node closes their
// commitment on-chain early, then it eventually recognizes this HTLC as one
// that's timed out. At this point, the node should timeout the HTLC using the
// HTLC timeout transaction, then cancel it backwards as normal.
func testMultiHopLocalForceCloseOnChainHtlcTimeout(net *lntest.NetworkHarness,
	t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol :=
		createThreeHopNetwork(t, net, true)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const (
		finalCltvDelta = 40
		htlcAmt        = btcutil.Amount(30000)
	)
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	alicePayStream, err := net.Alice.SendPayment(ctx)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := makeFakePayHash(t)
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = wait.Predicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	ctxt, _ := context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, bobChanPoint, true)

	// At this point, Bob should have a pending force close channel as he
	// just went to chain.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Bob.PendingChannels(ctxt,
			pendingChansRequest)
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

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if forceCloseChan.LimboBalance == 0 {
			predErr = fmt.Errorf("bob should have nonzero limbo "+
				"balance instead has: %v",
				forceCloseChan.LimboBalance)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// We'll mine defaultCSV blocks in order to generate the sweep transaction
	// of Bob's funding output.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's funding output sweep tx: %v", err)
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the utxo nursery.
	numBlocks := padCLTV(uint32(finalCltvDelta - defaultCSV - 1))
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	err = wait.Predicate(func() bool {
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
			predErr = fmt.Errorf("bob should have pending force " +
				"close chan but doesn't")
			return false
		}

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if len(forceCloseChan.PendingHtlcs) != 1 {
			predErr = fmt.Errorf("bob should have pending htlc " +
				"but doesn't")
			return false
		}
		if forceCloseChan.PendingHtlcs[0].Stage != 1 {
			predErr = fmt.Errorf("bob's htlc should have "+
				"advanced to the first stage: %v", err)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We should also now find a transaction in the mempool, as Bob should
	// have broadcast his second layer timeout transaction.
	timeoutTx, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's htlc timeout tx: %v", err)
	}

	// Next, we'll mine an additional block. This should serve to confirm
	// the second layer timeout transaction.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, timeoutTx)

	// With the second layer timeout transaction confirmed, Bob should have
	// canceled backwards the HTLC that carol sent.
	nodes = []*lntest.HarnessNode{net.Alice}
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

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	err = wait.Predicate(func() bool {
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

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if len(forceCloseChan.PendingHtlcs) != 1 {
			predErr = fmt.Errorf("bob should have pending htlc " +
				"but doesn't")
			return false
		}
		if forceCloseChan.PendingHtlcs[0].Stage != 2 {
			predErr = fmt.Errorf("bob's htlc should have "+
				"advanced to the second stage: %v", err)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We'll now mine 4 additional blocks. This should be enough for Bob's
	// CSV timelock to expire and the sweeping transaction of the HTLC to be
	// broadcast.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	sweepTx, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find bob's htlc sweep tx: %v", err)
	}

	// We'll then mine a final block which should confirm this second layer
	// sweep transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, sweepTx)

	// At this point, Bob should no longer show any channels as pending
	// close.
	err = wait.Predicate(func() bool {
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

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}
