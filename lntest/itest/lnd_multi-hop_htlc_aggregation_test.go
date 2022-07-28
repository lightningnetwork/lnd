package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// testMultiHopHtlcAggregation tests that in a multi-hop HTLC scenario, if we
// force close a channel with both incoming and outgoing HTLCs, we can properly
// resolve them using the second level timeout and success transactions. In
// case of anchor channels, the second-level spends can also be aggregated and
// properly feebumped, so we'll check that as well.
func testMultiHopHtlcAggregation(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType,
	zeroConf bool) {

	const finalCltvDelta = 40
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetworkOld(
		t, net, alice, bob, false, c, zeroConf,
	)
	defer shutdownAndAssert(net, t, carol)

	// To ensure we have capacity in both directions of the route, we'll
	// make  a fairly large payment Alice->Carol and settle it.
	const reBalanceAmt = 500_000
	invoice := &lnrpc.Invoice{
		Value: reBalanceAmt,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	stream, err := alice.RouterClient.SendPaymentV2(ctxt, sendReq)
	require.NoError(t.t, err)

	result, err := getPaymentResult(stream)
	require.NoError(t.t, err)
	require.Equal(t.t, result.Status, lnrpc.Payment_SUCCEEDED)

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
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		carolInvoice, err := carol.AddHoldInvoice(ctxt, invoiceReq)
		require.NoError(t.t, err)

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
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		aliceInvoice, err := alice.AddHoldInvoice(ctxt, invoiceReq)
		require.NoError(t.t, err)

		aliceInvoices = append(aliceInvoices, aliceInvoice)
		alicePreimages = append(alicePreimages, preimage)
		payHashes = append(payHashes, payHash[:])
		alicePayHashes = append(alicePayHashes, payHash[:])
	}

	// Now that we've created the invoices, we'll pay them all from
	// Alice<->Carol, going through Bob. We won't wait for the response
	// however, as neither will immediately settle the payment.
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	// Alice will pay all of Carol's invoices.
	for _, carolInvoice := range carolInvoices {
		_, err = alice.RouterClient.SendPaymentV2(
			ctx, &routerrpc.SendPaymentRequest{
				PaymentRequest: carolInvoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
		require.NoError(t.t, err)
	}

	// And Carol will pay Alice's.
	for _, aliceInvoice := range aliceInvoices {
		_, err = carol.RouterClient.SendPaymentV2(
			ctx, &routerrpc.SendPaymentRequest{
				PaymentRequest: aliceInvoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
		require.NoError(t.t, err)
	}

	// At this point, all 3 nodes should now the HTLCs active on their
	// channels.
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHashes...)
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Wait for Alice and Carol to mark the invoices as accepted. There is
	// a small gap to bridge between adding the htlc to the channel and
	// executing the exit hop logic.
	for _, payHash := range carolPayHashes {
		h := lntypes.Hash{}
		copy(h[:], payHash)
		waitForInvoiceAccepted(t, carol, h)
	}

	for _, payHash := range alicePayHashes {
		h := lntypes.Hash{}
		copy(h[:], payHash)
		waitForInvoiceAccepted(t, alice, h)
	}

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	net.SetFeeEstimate(30000)

	// We want Carol's htlcs to expire off-chain to demonstrate bob's force
	// close. However, Carol will cancel her invoices to prevent force
	// closes, so we shut her down for now.
	restartCarol, err := net.SuspendNode(carol)
	require.NoError(t.t, err)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the Carol's HTLCs are
	// about to timeout. With the default outgoing broadcast delta of zero,
	// this will be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	_, err = net.Miner.Client.Generate(numBlocks)
	require.NoError(t.t, err)

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we also expect Bob's anchor sweep.
	hasAnchors := commitTypeHasAnchors(c)
	expectedTxes := 1
	if hasAnchors {
		expectedTxes = 2
	}

	bobFundingTxid, err := lnrpc.GetChanPointFundingTxid(bobChanPoint)
	require.NoError(t.t, err)
	_, err = waitForNTxsInMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)
	closeTx := getSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, wire.OutPoint{
			Hash:  *bobFundingTxid,
			Index: bobChanPoint.OutputIndex,
		},
	)
	closeTxid := closeTx.TxHash()

	// Go through the closing transaction outputs, and make an index for the HTLC outputs.
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

		// If the HTLC has direction towards Alice, Bob will
		// claim it with the success TX when he learns the preimage. In
		// this case one extra sat will be on the output, because of
		// the routing fee.
		case invoiceAmt + 1:
			successOuts[op] = struct{}{}
		}
	}

	// Once bob has force closed, we can restart carol.
	require.NoError(t.t, restartCarol())

	// Mine a block to confirm the closing transaction.
	mineBlocks(t, net, 1, expectedTxes)

	time.Sleep(1 * time.Second)

	// Let Alice settle her invoices. When Bob now gets the preimages, he
	// has no other option than to broadcast his second-level transactions
	// to claim the money.
	for _, preimage := range alicePreimages {
		ctx, cancel = context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		_, err = alice.SettleInvoice(ctx, &invoicesrpc.SettleInvoiceMsg{
			Preimage: preimage[:],
		})
		require.NoError(t.t, err)
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
		t.Fatalf("unhandled commitment type %v", c)
	}

	txes, err := getNTxsFromMempool(
		net.Miner.Client, expectedTxes, minerMempoolTimeout,
	)
	require.NoError(t.t, err)

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
		require.Len(t.t, timeoutTxs, 1)
		require.Len(t.t, successTxs, 1)
	} else {
		require.Len(t.t, timeoutTxs, numInvoices)
		require.Len(t.t, successTxs, numInvoices)
	}

	// All mempool transactions should be spending from the commitment
	// transaction.
	assertAllTxesSpendFrom(t, txes, closeTxid)

	// Mine a block to confirm the transactions.
	block := mineBlocks(t, net, 1, expectedTxes)[0]
	require.Len(t.t, block.Transactions, expectedTxes+1)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation,
	// or to the sweeper for sweeping.
	err = waitForNumChannelPendingForceClose(
		bob, 1, func(c *lnrpcForceCloseChannel) error {
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

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// If we then mine additional blocks, Bob can sweep his commitment
		// output.
		_, err = net.Miner.Client.Generate(defaultCSV - 2)
		require.NoError(t.t, err)

		// Find the commitment sweep.
		bobCommitSweepHash, err := waitForTxInMempool(
			net.Miner.Client, minerMempoolTimeout,
		)
		require.NoError(t.t, err)
		bobCommitSweep, err := net.Miner.Client.GetRawTransaction(
			bobCommitSweepHash,
		)
		require.NoError(t.t, err)

		require.Equal(
			t.t, closeTxid,
			bobCommitSweep.MsgTx().TxIn[0].PreviousOutPoint.Hash,
		)

		// Also ensure it is not spending from any of the HTLC output.
		for _, txin := range bobCommitSweep.MsgTx().TxIn {
			for _, timeoutTx := range timeoutTxs {
				if *timeoutTx == txin.PreviousOutPoint.Hash {
					t.Fatalf("found unexpected spend of " +
						"timeout tx")
				}
			}

			for _, successTx := range successTxs {
				if *successTx == txin.PreviousOutPoint.Hash {
					t.Fatalf("found unexpected spend of " +
						"success tx")
				}
			}
		}
	}

	switch c {
	// In case this is a non-anchor channel type, we must mine 2 blocks, as
	// the nursery waits an extra block before sweeping. Before the blocks
	// are mined, we should expect to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_LEGACY:
		_ = mineBlocks(t, net, 2, 1)

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output. Before the blocks are mined, we should expect
	// to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_ANCHORS:
		_ = mineBlocks(t, net, 1, 1)

	// Since Bob is the initiator of the Bob-Carol script-enforced leased
	// channel, he incurs an additional CLTV when sweeping outputs back to
	// his wallet. We'll need to mine enough blocks for the timelock to
	// expire to prompt his broadcast.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := bob.PendingChannels(
			ctxt, &lnrpc.PendingChannelsRequest{},
		)
		require.NoError(t.t, err)
		require.Len(t.t, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(t.t, forceCloseChan.BlocksTilMaturity)
		_ = mineBlocks(t, net, uint32(forceCloseChan.BlocksTilMaturity), 0)

	default:
		t.Fatalf("unhandled commitment type %v", c)
	}

	bobSweep, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	// Make sure it spends from the second level tx.
	secondLevelSweep, err := net.Miner.Client.GetRawTransaction(bobSweep)
	require.NoError(t.t, err)

	// It should be sweeping all the second-level outputs.
	var secondLvlSpends int
	for _, txin := range secondLevelSweep.MsgTx().TxIn {
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

	require.Equal(t.t, 2*numInvoices, secondLvlSpends)

	// When we mine one additional block, that will confirm Bob's second
	// level sweep.  Now Bob should have no pending channels anymore, as
	// this just resolved it by the confirmation of the sweep transaction.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, bobSweep)

	err = waitForNumChannelPendingForceClose(bob, 0, nil)
	require.NoError(t.t, err)

	// THe channel with Alice is still open.
	assertNodeNumChannels(t, bob, 1)

	// Carol should have no channels left (open nor pending).
	err = waitForNumChannelPendingForceClose(carol, 0, nil)
	require.NoError(t.t, err)
	assertNodeNumChannels(t, carol, 0)

	// Coop close channel, expect no anchors.
	closeChannelAndAssertType(t, net, alice, aliceChanPoint, false, false)
}
