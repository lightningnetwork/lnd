package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType) {

	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		t, net, alice, bob, false, c,
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
	require.NoError(t.t, err)

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	_, err = alice.RouterClient.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			PaymentRequest: carolInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	require.NoError(t.t, err)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash[:])
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	waitForInvoiceAccepted(t, carol, payHash)

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	net.SetFeeEstimate(30000)

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	hasAnchors := commitTypeHasAnchors(c)
	bobForceClose := closeChannelAndAssertType(
		ctxt, t, net, bob, aliceChanPoint, hasAnchors, true,
	)

	var expectedTxes int
	switch c {
	// Alice will sweep her commitment output immediately.
	case lnrpc.CommitmentType_LEGACY:
		expectedTxes = 1

	// Alice will sweep her commitment and anchor output immediately.
	case lnrpc.CommitmentType_ANCHORS:
		expectedTxes = 2

	// Alice will sweep her anchor output immediately. Her commitment output
	// cannot be swept yet as it has incurred an additional CLTV due to
	// being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		expectedTxes = 1

	default:
		t.Fatalf("unhandled commitment type %v", c)
	}
	_, err = waitForNTxsInMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	// Suspend Bob to force Carol to go to chain.
	restartBob, err := net.SuspendNode(bob)
	require.NoError(t.t, err)

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
	require.NoError(t.t, err)

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry -
		lncfg.DefaultIncomingBroadcastDelta))

	_, err = net.Miner.Client.Generate(numBlocks)
	require.NoError(t.t, err)

	// Carol's commitment transaction should now be in the mempool. If there
	// is an anchor, Carol will sweep that too.
	_, err = waitForNTxsInMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)
	bobFundingTxid, err := lnrpc.GetChanPointFundingTxid(bobChanPoint)
	require.NoError(t.t, err)
	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// Look up the closing transaction. It should be spending from the
	// funding transaction,
	closingTx := getSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, carolFundingPoint,
	)
	closingTxid := closingTx.TxHash()

	// Mine a block that should confirm the commit tx, the anchor if present
	// and the coinbase.
	block := mineBlocks(t, net, 1, expectedTxes)[0]
	require.Len(t.t, block.Transactions, expectedTxes+1)
	assertTxInBlock(t, block, &closingTxid)

	// Restart bob again.
	err = restartBob()
	require.NoError(t.t, err)

	// After the force close transacion is mined, transactions will be
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
		expectedTxes = 3

	default:
		t.Fatalf("unhandled commitment type %v", c)
	}
	txes, err := getNTxsFromMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	// Both Carol's second level transaction and Bob's sweep should be
	// spending from the commitment transaction.
	assertAllTxesSpendFrom(t, txes, closingTxid)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice, err := net.SuspendNode(alice)
	require.NoError(t.t, err)

	// Mine a block to confirm the expected transactions (+ the coinbase).
	block = mineBlocks(t, net, 1, expectedTxes)[0]
	require.Len(t.t, block.Transactions, expectedTxes+1)

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
	bobSecondLvlTx, err := waitForTxInMempool(
		net.Miner.Client, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

	// It should spend from the commitment in the channel with Alice.
	tx, err := net.Miner.Client.GetRawTransaction(bobSecondLvlTx)
	require.NoError(t.t, err)

	require.Equal(
		t.t, *bobForceClose, tx.MsgTx().TxIn[0].PreviousOutPoint.Hash,
	)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
	numPendingChans := 1
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		numPendingChans++
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNumChannelPendingForceClose(
		ctxt, bob, numPendingChans, func(c *lnrpcForceCloseChannel) error {
			if c.Channel.LocalBalance != 0 {
				return nil
			}

			if len(c.PendingHtlcs) != 1 {
				return fmt.Errorf("bob should have pending " +
					"htlc but doesn't")
			}

			if c.PendingHtlcs[0].Stage != 1 {
				return fmt.Errorf("bob's htlc should have "+
					"advanced to the first stage but was "+
					"stage: %v", c.PendingHtlcs[0].Stage)
			}

			return nil
		},
	)
	require.NoError(t.t, err)

	// We'll now mine a block which should confirm Bob's second layer
	// transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	require.Len(t.t, block.Transactions, 2)
	assertTxInBlock(t, block, bobSecondLvlTx)

	// Keep track of Bob's second level maturity, and decrement our track
	// of Carol's.
	bobSecondLevelCSV := secondLevelMaturity
	carolSecondLevelCSV--

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	err = restartAlice()
	require.NoError(t.t, err)

	// If we then mine 3 additional blocks, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	_, err = net.Miner.Client.Generate(carolSecondLevelCSV)
	require.NoError(t.t, err)
	bobSecondLevelCSV -= carolSecondLevelCSV

	carolSweep, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output.
	block = mineBlocks(t, net, bobSecondLevelCSV, 1)[0]
	assertTxInBlock(t, block, carolSweep)

	bobSweep, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	// Make sure it spends from the second level tx.
	tx, err = net.Miner.Client.GetRawTransaction(bobSweep)
	require.NoError(t.t, err)
	require.Equal(
		t.t, *bobSecondLvlTx, tx.MsgTx().TxIn[0].PreviousOutPoint.Hash,
	)

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, bobSweep)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		for _, node := range []*lntest.HarnessNode{alice, bob} {
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = waitForNumChannelPendingForceClose(ctxt, node, 1, nil)
			require.NoError(t.t, err)
		}

		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		resp, err := alice.PendingChannels(
			ctxt, &lnrpc.PendingChannelsRequest{},
		)
		require.NoError(t.t, err)
		require.Len(t.t, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(t.t, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		_, err = net.Miner.Client.Generate(numBlocks)
		require.NoError(t.t, err)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{Hash: *bobForceClose, Index: 3}
		aliceCommitSweep := assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout,
			aliceCommitOutpoint,
		)
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		bobCommitSweep := assertSpendingTxInMempool(
			t, net.Miner.Client, minerMempoolTimeout,
			bobCommitOutpoint,
		)

		// Confirm their sweeps.
		block := mineBlocks(t, net, 1, 2)[0]
		assertTxInBlock(t, block, &aliceCommitSweep)
		assertTxInBlock(t, block, &bobCommitSweep)
	}

	// All nodes should show zero pending and open channels.
	for _, node := range []*lntest.HarnessNode{alice, bob, carol} {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = waitForNumChannelPendingForceClose(ctxt, node, 0, nil)
		require.NoError(t.t, err)
		assertNodeNumChannels(t, node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	err = checkPaymentStatus(
		ctxt, alice, preimage, lnrpc.Payment_SUCCEEDED,
	)
	require.NoError(t.t, err)
}
