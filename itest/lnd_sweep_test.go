package itest

import (
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// testSweepCPFPAnchorOutgoingTimeout checks when a channel is force closed by
// a local node due to the outgoing HTLC times out, the anchor output is used
// for CPFPing the force close tx.
//
// Setup:
//  1. Fund Alice with 1 UTXO - she only needs one for the funding process,
//  2. Fund Bob with 1 UTXO - he only needs one for the funding process, and
//     the change output will be used for sweeping his anchor on local commit.
//  3. Create a linear network from Alice -> Bob -> Carol.
//  4. Alice pays an invoice to Carol through Bob, with Carol holding the
//     settlement.
//  5. Carol goes offline.
//
// Test:
//  1. Bob force closes the channel with Carol, using the anchor output for
//     CPFPing the force close tx.
//  2. Bob's anchor output is swept and fee bumped based on its deadline and
//     budget.
func testSweepCPFPAnchorOutgoingTimeout(ht *lntest.HarnessTest) {
	// Setup testing params.
	//
	// Invoice is 100k sats.
	invoiceAmt := btcutil.Amount(100_000)

	// Use the smallest CLTV so we can mine fewer blocks.
	cltvDelta := routing.MinCLTVDelta

	// deadlineDeltaAnchor is the expected deadline delta for the CPFP
	// anchor sweeping tx.
	deadlineDeltaAnchor := uint32(cltvDelta / 2)

	// startFeeRateAnchor is the starting fee rate for the CPFP anchor
	// sweeping tx.
	startFeeRateAnchor := chainfee.SatPerKWeight(2500)

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	//
	// TODO(yy): switch to conf when `blockbeat` is in place.
	// ht.SetFeeEstimateWithConf(startFeeRateAnchor, deadlineDeltaAnchor)
	ht.SetFeeEstimate(startFeeRateAnchor)

	// htlcValue is the outgoing HTLC's value.
	htlcValue := invoiceAmt

	// htlcBudget is the budget used to sweep the outgoing HTLC.
	htlcBudget := htlcValue.MulF64(contractcourt.DefaultBudgetRatio)

	// cpfpBudget is the budget used to sweep the CPFP anchor.
	// In addition to the htlc amount to protect we also need to include
	// the anchor amount itself for the budget.
	cpfpBudget := (htlcValue - htlcBudget).MulF64(
		contractcourt.DefaultBudgetRatio,
	) + contractcourt.AnchorOutputValue

	// Create a preimage, that will be held by Carol.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()

	// We now set up the force close scenario. We will create a network
	// from Alice -> Bob -> Carol, where Alice will send a payment to Carol
	// via Bob, Carol goes offline. We expect Bob to sweep his anchor and
	// outgoing HTLC.
	//
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// Use a small CLTV to mine less blocks.
		fmt.Sprintf("--bitcoin.timelockdelta=%d", cltvDelta),
		// Use a very large CSV, this way to_local outputs are never
		// swept so we can focus on testing HTLCs.
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", cltvDelta*10),
	}
	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := createSimpleNetwork(ht, cfg, 3, openChannelParams)

	// Unwrap the results.
	abChanPoint, bcChanPoint := chanPoints[0], chanPoints[1]
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// For neutrino backend, we need one more UTXO for Bob to create his
	// sweeping txns.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// Subscribe the invoice.
	streamCarol := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// With the network active, we'll now add a hodl invoice at Carol's
	// end.
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(invoiceAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	invoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Let Alice pay the invoices.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}

	// Assert the payments are inflight.
	ht.SendPaymentAndAssertStatus(alice, req, lnrpc.Payment_IN_FLIGHT)

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(streamCarol, lnrpc.Invoice_ACCEPTED)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLCs pending on all of them.
	//
	// Alice should have one outgoing HTLCs on channel Alice -> Bob.
	ht.AssertOutgoingHTLCActive(alice, abChanPoint, payHash[:])

	// Bob should have one incoming HTLC on channel Alice -> Bob, and one
	// outgoing HTLC on channel Bob -> Carol.
	ht.AssertIncomingHTLCActive(bob, abChanPoint, payHash[:])
	ht.AssertOutgoingHTLCActive(bob, bcChanPoint, payHash[:])

	// Carol should have one incoming HTLC on channel Bob -> Carol.
	ht.AssertIncomingHTLCActive(carol, bcChanPoint, payHash[:])

	// Let Carol go offline so we can focus on testing Bob's sweeping
	// behavior.
	ht.Shutdown(carol)

	// We'll now mine enough blocks to trigger Bob to force close channel
	// Bob->Carol due to his outgoing HTLC is about to timeout. With the
	// default outgoing broadcast delta of zero, this will be the same
	// height as the outgoing htlc's expiry height.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultOutgoingBroadcastDelta,
	))
	ht.MineEmptyBlocks(int(numBlocks))

	// Assert Bob's force closing tx has been broadcast.
	closeTxid := ht.AssertNumTxsInMempool(1)[0]

	// Remember the force close height so we can calculate the deadline
	// height.
	forceCloseHeight := ht.CurrentHeight()

	// Bob should have two pending sweeps,
	// - anchor sweeping from his local commitment.
	// - anchor sweeping from his remote commitment (invalid).
	//
	// TODO(yy): consider only sweeping the anchor from the local
	// commitment. Previously we would sweep up to three versions of
	// anchors because we don't know which one will be confirmed - if we
	// only broadcast the local anchor sweeping, our peer can broadcast
	// their commitment tx and replaces ours. With the new fee bumping, we
	// should be safe to only sweep our local anchor since we RBF it on
	// every new block, which destroys the remote's ability to pin us.
	sweeps := ht.AssertNumPendingSweeps(bob, 2)

	// The two anchor sweeping should have the same deadline height.
	deadlineHeight := forceCloseHeight + deadlineDeltaAnchor
	require.Equal(ht, deadlineHeight, sweeps[0].DeadlineHeight)
	require.Equal(ht, deadlineHeight, sweeps[1].DeadlineHeight)

	// Remember the deadline height for the CPFP anchor.
	anchorDeadline := sweeps[0].DeadlineHeight

	// Mine a block so Bob's force closing tx stays in the mempool, which
	// also triggers the CPFP anchor sweep.
	ht.MineEmptyBlocks(1)

	// Bob should still have two pending sweeps,
	// - anchor sweeping from his local commitment.
	// - anchor sweeping from his remote commitment (invalid).
	ht.AssertNumPendingSweeps(bob, 2)

	// We now check the expected fee and fee rate are used for Bob's anchor
	// sweeping tx.
	//
	// We should see Bob's anchor sweeping tx triggered by the above
	// block, along with his force close tx.
	txns := ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Get the weight for Bob's anchor sweeping tx.
	txWeight := ht.CalculateTxWeight(sweepTx)

	// Bob should start with the initial fee rate of 2500 sat/kw.
	startFeeAnchor := startFeeRateAnchor.FeeForWeight(txWeight)

	// Calculate the fee and fee rate of Bob's sweeping tx.
	fee := uint64(ht.CalculateTxFee(sweepTx))
	feeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// feeFuncWidth is the width of the fee function. By the time we got
	// here, we've already mined one block, and the fee function maxes
	// out one block before the deadline, so the width is the original
	// deadline minus 2.
	feeFuncWidth := deadlineDeltaAnchor - 2

	// Calculate the expected delta increased per block.
	feeDelta := (cpfpBudget - startFeeAnchor).MulF64(
		1 / float64(feeFuncWidth),
	)

	// We expect the startingFee and startingFeeRate being used. Allow some
	// deviation because weight estimates during tx generation are
	// estimates.
	//
	// TODO(yy): unify all the units and types re int vs uint!
	require.InEpsilonf(ht, uint64(startFeeAnchor), fee, 0.01,
		"want %d, got %d", startFeeAnchor, fee)
	require.InEpsilonf(ht, uint64(startFeeRateAnchor), feeRate,
		0.01, "want %d, got %d", startFeeRateAnchor, fee)

	// We now mine deadline-2 empty blocks. For each block mined, Bob
	// should perform an RBF on his CPFP anchor sweeping tx. By the end of
	// this iteration, we expect Bob to use up his CPFP budget after one
	// more block.
	for i := uint32(1); i <= feeFuncWidth-1; i++ {
		// Mine an empty block. Since the sweeping tx is not confirmed,
		// Bob's fee bumper should increase its fees.
		ht.MineEmptyBlocks(1)

		// Bob should still have two pending sweeps,
		// - anchor sweeping from his local commitment.
		// - anchor sweeping from his remote commitment (invalid).
		ht.AssertNumPendingSweeps(bob, 2)

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(sweepTx.TxHash())

		// We expect to see two txns in the mempool,
		// - Bob's force close tx.
		// - Bob's anchor sweep tx.
		ht.AssertNumTxsInMempool(2)

		// We expect the fees to increase by i*delta.
		expectedFee := startFeeAnchor + feeDelta.MulF64(float64(i))
		expectedFeeRate := chainfee.NewSatPerKWeight(
			expectedFee, txWeight,
		)

		// We should see Bob's anchor sweeping tx being fee bumped
		// since it's not confirmed, along with his force close tx.
		txns = ht.GetNumTxsFromMempool(2)

		// Find the sweeping tx.
		sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

		// Calculate the fee rate of Bob's new sweeping tx.
		feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

		// Calculate the fee of Bob's new sweeping tx.
		fee = uint64(ht.CalculateTxFee(sweepTx))

		ht.Logf("Bob(position=%v): txWeight=%v, expected: [fee=%d, "+
			"feerate=%v], got: [fee=%v, feerate=%v]",
			feeFuncWidth-i, txWeight, expectedFee,
			expectedFeeRate, fee, feeRate)

		// Assert Bob's tx has the expected fee and fee rate.
		require.InEpsilonf(ht, uint64(expectedFee), fee, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFee, fee)
		require.InEpsilonf(ht, uint64(expectedFeeRate), feeRate, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFeeRate,
			feeRate)
	}

	// We now check the budget has been used up at the deadline-1 block.
	//
	// Once out of the above loop, we expect to be 2 blocks before the CPFP
	// deadline.
	currentHeight := ht.CurrentHeight()
	require.Equal(ht, int(anchorDeadline-2), int(currentHeight))

	// Mine one more block, we'd use up all the CPFP budget.
	ht.MineEmptyBlocks(1)

	// Make sure Bob's old sweeping tx has been removed from the mempool.
	ht.AssertTxNotInMempool(sweepTx.TxHash())

	// Get the last sweeping tx - we should see two txns here, Bob's anchor
	// sweeping tx and his force close tx.
	txns = ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Calculate the fee of Bob's new sweeping tx.
	fee = uint64(ht.CalculateTxFee(sweepTx))

	// Assert the budget is now used up.
	require.InEpsilonf(ht, uint64(cpfpBudget), fee, 0.01, "want %d, got %d",
		cpfpBudget, fee)

	// Mine one more block. Since Bob's budget has been used up, there
	// won't be any more sweeping attempts. We now assert this by checking
	// that the sweeping tx stayed unchanged.
	ht.MineEmptyBlocks(1)

	// Get the current sweeping tx and assert it stays unchanged.
	//
	// We expect two txns here, one for the anchor sweeping, the other for
	// the force close tx.
	txns = ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	currentSweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Assert the anchor sweep tx stays unchanged.
	require.Equal(ht, sweepTx.TxHash(), currentSweepTx.TxHash())

	// Mine a block to confirm Bob's sweeping and force close txns, this is
	// needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// The above mined block should confirm Bob's force close tx, and his
	// contractcourt will offer the HTLC to his sweeper. We are not testing
	// the HTLC sweeping behaviors so we just perform a simple check and
	// exit the test.
	ht.AssertNumPendingSweeps(bob, 1)

	// Finally, clean the mempool for the next test.
	ht.CleanShutDown()
}

// testSweepCPFPAnchorIncomingTimeout checks when a channel is force closed by
// a local node due to the incoming HTLC is about to time out, the anchor
// output is used for CPFPing the force close tx.
//
// Setup:
//  1. Fund Alice with 1 UTXOs - she only needs one for the funding process,
//  2. Fund Bob with 1 UTXO - he only needs one for the funding process, and
//     the change output will be used for sweeping his anchor on local commit.
//  3. Create a linear network from Alice -> Bob -> Carol.
//  4. Alice pays an invoice to Carol through Bob.
//  5. Alice goes offline.
//  6. Carol settles the invoice.
//
// Test:
//  1. Bob force closes the channel with Alice, using the anchor output for
//     CPFPing the force close tx.
//  2. Bob's anchor output is swept and fee bumped based on its deadline and
//     budget.
func testSweepCPFPAnchorIncomingTimeout(ht *lntest.HarnessTest) {
	// Setup testing params.
	//
	// Invoice is 100k sats.
	invoiceAmt := btcutil.Amount(100_000)

	// Use the smallest CLTV so we can mine fewer blocks.
	cltvDelta := routing.MinCLTVDelta

	// goToChainDelta is the broadcast delta of Bob's incoming HTLC. When
	// the block height is at CLTV-goToChainDelta, Bob will force close the
	// channel Alice=>Bob.
	goToChainDelta := uint32(lncfg.DefaultIncomingBroadcastDelta)

	// deadlineDeltaAnchor is the expected deadline delta for the CPFP
	// anchor sweeping tx.
	deadlineDeltaAnchor := goToChainDelta / 2

	// startFeeRateAnchor is the starting fee rate for the CPFP anchor
	// sweeping tx.
	startFeeRateAnchor := chainfee.SatPerKWeight(2500)

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	//
	// TODO(yy): switch to conf when `blockbeat` is in place.
	// ht.SetFeeEstimateWithConf(startFeeRateAnchor, deadlineDeltaAnchor)
	ht.SetFeeEstimate(startFeeRateAnchor)

	// Create a preimage, that will be held by Carol.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()

	// We now set up the force close scenario. We will create a network
	// from Alice -> Bob -> Carol, where Alice will send a payment to Carol
	// via Bob, Alice goes offline, Carol settles the payment. We expect
	// Bob to sweep his anchor and incoming HTLC.
	//
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// Use a small CLTV to mine less blocks.
		fmt.Sprintf("--bitcoin.timelockdelta=%d", cltvDelta),
		// Use a very large CSV, this way to_local outputs are never
		// swept so we can focus on testing HTLCs.
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", cltvDelta*10),
	}
	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := createSimpleNetwork(ht, cfg, 3, openChannelParams)

	// Unwrap the results.
	abChanPoint, bcChanPoint := chanPoints[0], chanPoints[1]
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// For neutrino backend, we need one more UTXO for Bob to create his
	// sweeping txns.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// Subscribe the invoice.
	streamCarol := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// With the network active, we'll now add a hodl invoice at Carol's
	// end.
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(invoiceAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	invoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Let Alice pay the invoices.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}

	// Assert the payments are inflight.
	ht.SendPaymentAndAssertStatus(alice, req, lnrpc.Payment_IN_FLIGHT)

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(streamCarol, lnrpc.Invoice_ACCEPTED)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLCs pending on all of them.
	//
	// Alice should have one outgoing HTLCs on channel Alice -> Bob.
	ht.AssertOutgoingHTLCActive(alice, abChanPoint, payHash[:])

	// Bob should have one incoming HTLC on channel Alice -> Bob, and one
	// outgoing HTLC on channel Bob -> Carol.
	htlc := ht.AssertIncomingHTLCActive(bob, abChanPoint, payHash[:])
	ht.AssertOutgoingHTLCActive(bob, bcChanPoint, payHash[:])

	// Calculate the budget used for Bob's anchor sweeping.
	//
	// htlcValue is the incoming HTLC's value.
	htlcValue := btcutil.Amount(htlc.Amount)

	// htlcBudget is the budget used to sweep the incoming HTLC.
	htlcBudget := htlcValue.MulF64(contractcourt.DefaultBudgetRatio)

	// cpfpBudget is the budget used to sweep the CPFP anchor.
	// In addition to the htlc amount to protect we also need to include
	// the anchor amount itself for the budget.
	cpfpBudget := (htlcValue - htlcBudget).MulF64(
		contractcourt.DefaultBudgetRatio,
	) + contractcourt.AnchorOutputValue

	// Carol should have one incoming HTLC on channel Bob -> Carol.
	ht.AssertIncomingHTLCActive(carol, bcChanPoint, payHash[:])

	// Let Alice go offline. Once Bob later learns the preimage, he
	// couldn't settle it with Alice so he has to go onchain to collect it.
	ht.Shutdown(alice)

	// Carol settles invoice.
	carol.RPC.SettleInvoice(preimage[:])

	// Bob should have settled his outgoing HTLC with Carol.
	ht.AssertHTLCNotActive(bob, bcChanPoint, payHash[:])

	// We'll now mine enough blocks to trigger Bob to force close channel
	// Alice->Bob due to his incoming HTLC is about to timeout. With the
	// default incoming broadcast delta of 10, this will be the same
	// height as the incoming htlc's expiry height minus 10.
	forceCloseHeight := htlc.ExpirationHeight - goToChainDelta

	// Mine till the goToChainHeight is reached.
	currentHeight := ht.CurrentHeight()
	numBlocks := forceCloseHeight - currentHeight
	ht.MineEmptyBlocks(int(numBlocks))

	// Assert Bob's force closing tx has been broadcast.
	closeTxid := ht.AssertNumTxsInMempool(1)[0]

	// Bob should have two pending sweeps,
	// - anchor sweeping from his local commitment.
	// - anchor sweeping from his remote commitment (invalid).
	sweeps := ht.AssertNumPendingSweeps(bob, 2)

	// The two anchor sweeping should have the same deadline height.
	deadlineHeight := forceCloseHeight + deadlineDeltaAnchor
	require.Equal(ht, deadlineHeight, sweeps[0].DeadlineHeight)
	require.Equal(ht, deadlineHeight, sweeps[1].DeadlineHeight)

	// Remember the deadline height for the CPFP anchor.
	anchorDeadline := sweeps[0].DeadlineHeight

	// Mine a block so Bob's force closing tx stays in the mempool, which
	// also triggers the CPFP anchor sweep.
	ht.MineEmptyBlocks(1)

	// Bob should still have two pending sweeps,
	// - anchor sweeping from his local commitment.
	// - anchor sweeping from his remote commitment (invalid).
	ht.AssertNumPendingSweeps(bob, 2)

	// We now check the expected fee and fee rate are used for Bob's anchor
	// sweeping tx.
	//
	// We should see Bob's anchor sweeping tx triggered by the above
	// block, along with his force close tx.
	txns := ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Get the weight for Bob's anchor sweeping tx.
	txWeight := ht.CalculateTxWeight(sweepTx)

	// Bob should start with the initial fee rate of 2500 sat/kw.
	startFeeAnchor := startFeeRateAnchor.FeeForWeight(txWeight)

	// Calculate the fee and fee rate of Bob's sweeping tx.
	fee := uint64(ht.CalculateTxFee(sweepTx))
	feeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// feeFuncWidth is the width of the fee function. By the time we got
	// here, we've already mined one block, and the fee function maxes
	// out one block before the deadline, so the width is the original
	// deadline minus 2.
	feeFuncWidth := deadlineDeltaAnchor - 2

	// Calculate the expected delta increased per block.
	feeDelta := (cpfpBudget - startFeeAnchor).MulF64(
		1 / float64(feeFuncWidth),
	)

	// We expect the startingFee and startingFeeRate being used. Allow some
	// deviation because weight estimates during tx generation are
	// estimates.
	//
	// TODO(yy): unify all the units and types re int vs uint!
	require.InEpsilonf(ht, uint64(startFeeAnchor), fee, 0.01,
		"want %d, got %d", startFeeAnchor, fee)
	require.InEpsilonf(ht, uint64(startFeeRateAnchor), feeRate,
		0.01, "want %d, got %d", startFeeRateAnchor, fee)

	// We now mine deadline-2 empty blocks. For each block mined, Bob
	// should perform an RBF on his CPFP anchor sweeping tx. By the end of
	// this iteration, we expect Bob to use up his CPFP budget after one
	// more block.
	for i := uint32(1); i <= feeFuncWidth-1; i++ {
		// Mine an empty block. Since the sweeping tx is not confirmed,
		// Bob's fee bumper should increase its fees.
		ht.MineEmptyBlocks(1)

		// Bob should still have two pending sweeps,
		// - anchor sweeping from his local commitment.
		// - anchor sweeping from his remote commitment (invalid).
		ht.AssertNumPendingSweeps(bob, 2)

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(sweepTx.TxHash())

		// We expect to see two txns in the mempool,
		// - Bob's force close tx.
		// - Bob's anchor sweep tx.
		ht.AssertNumTxsInMempool(2)

		// We expect the fees to increase by i*delta.
		expectedFee := startFeeAnchor + feeDelta.MulF64(float64(i))
		expectedFeeRate := chainfee.NewSatPerKWeight(
			expectedFee, txWeight,
		)

		// We should see Bob's anchor sweeping tx being fee bumped
		// since it's not confirmed, along with his force close tx.
		txns = ht.GetNumTxsFromMempool(2)

		// Find the sweeping tx.
		sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

		// Calculate the fee rate of Bob's new sweeping tx.
		feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

		// Calculate the fee of Bob's new sweeping tx.
		fee = uint64(ht.CalculateTxFee(sweepTx))

		ht.Logf("Bob(position=%v): txWeight=%v, expected: [fee=%d, "+
			"feerate=%v], got: [fee=%v, feerate=%v]",
			feeFuncWidth-i, txWeight, expectedFee,
			expectedFeeRate, fee, feeRate)

		// Assert Bob's tx has the expected fee and fee rate.
		require.InEpsilonf(ht, uint64(expectedFee), fee, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFee, fee)
		require.InEpsilonf(ht, uint64(expectedFeeRate), feeRate, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFeeRate,
			feeRate)
	}

	// We now check the budget has been used up at the deadline-1 block.
	//
	// Once out of the above loop, we expect to be 2 blocks before the CPFP
	// deadline.
	currentHeight = ht.CurrentHeight()
	require.Equal(ht, int(anchorDeadline-2), int(currentHeight))

	// Mine one more block, we'd use up all the CPFP budget.
	ht.MineEmptyBlocks(1)

	// Make sure Bob's old sweeping tx has been removed from the mempool.
	ht.AssertTxNotInMempool(sweepTx.TxHash())

	// Get the last sweeping tx - we should see two txns here, Bob's anchor
	// sweeping tx and his force close tx.
	txns = ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Calculate the fee of Bob's new sweeping tx.
	fee = uint64(ht.CalculateTxFee(sweepTx))

	// Assert the budget is now used up.
	require.InEpsilonf(ht, uint64(cpfpBudget), fee, 0.01, "want %d, got %d",
		cpfpBudget, fee)

	// Mine one more block. Since Bob's budget has been used up, there
	// won't be any more sweeping attempts. We now assert this by checking
	// that the sweeping tx stayed unchanged.
	ht.MineEmptyBlocks(1)

	// Get the current sweeping tx and assert it stays unchanged.
	//
	// We expect two txns here, one for the anchor sweeping, the other for
	// the force close tx.
	txns = ht.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	currentSweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Assert the anchor sweep tx stays unchanged.
	require.Equal(ht, sweepTx.TxHash(), currentSweepTx.TxHash())

	// Mine a block to confirm Bob's sweeping and force close txns, this is
	// needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// The above mined block should confirm Bob's force close tx, and his
	// contractcourt will offer the HTLC to his sweeper. We are not testing
	// the HTLC sweeping behaviors so we just perform a simple check and
	// exit the test.
	ht.AssertNumPendingSweeps(bob, 1)

	// Finally, clean the mempool for the next test.
	ht.CleanShutDown()
}

// testSweepHTLCs checks the sweeping behavior for HTLC outputs. Since HTLCs
// are time-sensitive, we expect to see both the incoming and outgoing HTLCs
// are fee bumped properly based on their budgets and deadlines.
//
// Setup:
//  1. Fund Alice with 1 UTXOs - she only needs one for the funding process,
//  2. Fund Bob with 3 UTXOs - he needs one for the funding process, one for
//     his CPFP anchor sweeping, and one for sweeping his outgoing HTLC.
//  3. Create a linear network from Alice -> Bob -> Carol.
//  4. Alice pays two invoices to Carol, with Carol holding the settlement.
//  5. Alice goes offline.
//  7. Carol settles one of the invoices with Bob, so Bob has an incoming HTLC
//     that he can claim onchain since he has the preimage.
//  8. Carol goes offline.
//  9. Assert Bob sweeps his incoming and outgoing HTLCs with the expected fee
//     rates.
//
// Test:
//  1. Bob's outgoing HTLC is swept and fee bumped based on its deadline and
//     budget.
//  2. Bob's incoming HTLC is swept and fee bumped based on its deadline and
//     budget.
func testSweepHTLCs(ht *lntest.HarnessTest) {
	// Setup testing params.
	//
	// Invoice is 100k sats.
	invoiceAmt := btcutil.Amount(100_000)

	// Use the smallest CLTV so we can mine fewer blocks.
	cltvDelta := routing.MinCLTVDelta

	// Start tracking the deadline delta of Bob's HTLCs. We need one block
	// for the CSV lock, and another block to trigger the sweeper to sweep.
	outgoingHTLCDeadline := int32(cltvDelta - 2)
	incomingHTLCDeadline := int32(lncfg.DefaultIncomingBroadcastDelta - 2)

	// startFeeRate1 and startFeeRate2 are returned by the fee estimator in
	// sat/kw. They will be used as the starting fee rate for the linear
	// fee func used by Bob. The values are chosen from calling the cli in
	// bitcoind:
	// - `estimatesmartfee 18 conservative`.
	// - `estimatesmartfee 10 conservative`.
	startFeeRate1 := chainfee.SatPerKWeight(2500)
	startFeeRate2 := chainfee.SatPerKWeight(3000)

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	ht.SetFeeEstimateWithConf(startFeeRate1, uint32(outgoingHTLCDeadline))
	ht.SetFeeEstimateWithConf(startFeeRate2, uint32(incomingHTLCDeadline))

	// Create two preimages, one that will be settled, the other be hold.
	var preimageSettled, preimageHold lntypes.Preimage
	copy(preimageSettled[:], ht.Random32Bytes())
	copy(preimageHold[:], ht.Random32Bytes())
	payHashSettled := preimageSettled.Hash()
	payHashHold := preimageHold.Hash()

	// We now set up the force close scenario. We will create a network
	// from Alice -> Bob -> Carol, where Alice will send two payments to
	// Carol via Bob, Alice goes offline, then Carol settles the first
	// payment, goes offline. We expect Bob to sweep his incoming and
	// outgoing HTLCs.
	//
	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// Use a small CLTV to mine less blocks.
		fmt.Sprintf("--bitcoin.timelockdelta=%d", cltvDelta),
		// Use a very large CSV, this way to_local outputs are never
		// swept so we can focus on testing HTLCs.
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", cltvDelta*10),
	}
	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := createSimpleNetwork(ht, cfg, 3, openChannelParams)

	// Unwrap the results.
	abChanPoint, bcChanPoint := chanPoints[0], chanPoints[1]
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// Bob needs two more wallet utxos:
	// - when sweeping anchors, he needs one utxo for each sweep.
	// - when sweeping HTLCs, he needs one utxo for each sweep.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)

	// For neutrino backend, we need two more UTXOs for Bob to create his
	// sweeping txns.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// Subscribe the invoices.
	stream1 := carol.RPC.SubscribeSingleInvoice(payHashSettled[:])
	stream2 := carol.RPC.SubscribeSingleInvoice(payHashHold[:])

	// With the network active, we'll now add two hodl invoices at Carol's
	// end.
	invoiceReqSettle := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(invoiceAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHashSettled[:],
	}
	invoiceSettle := carol.RPC.AddHoldInvoice(invoiceReqSettle)

	invoiceReqHold := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(invoiceAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHashHold[:],
	}
	invoiceHold := carol.RPC.AddHoldInvoice(invoiceReqHold)

	// Let Alice pay the invoices.
	req1 := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceSettle.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	req2 := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceHold.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}

	// Assert the payments are inflight.
	ht.SendPaymentAndAssertStatus(alice, req1, lnrpc.Payment_IN_FLIGHT)
	ht.SendPaymentAndAssertStatus(alice, req2, lnrpc.Payment_IN_FLIGHT)

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream1, lnrpc.Invoice_ACCEPTED)
	ht.AssertInvoiceState(stream2, lnrpc.Invoice_ACCEPTED)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLCs pending on all of them.
	//
	// Alice should have two outgoing HTLCs on channel Alice -> Bob.
	ht.AssertOutgoingHTLCActive(alice, abChanPoint, payHashSettled[:])
	ht.AssertOutgoingHTLCActive(alice, abChanPoint, payHashHold[:])

	// Bob should have two incoming HTLCs on channel Alice -> Bob, and two
	// outgoing HTLCs on channel Bob -> Carol.
	ht.AssertIncomingHTLCActive(bob, abChanPoint, payHashSettled[:])
	ht.AssertIncomingHTLCActive(bob, abChanPoint, payHashHold[:])
	ht.AssertOutgoingHTLCActive(bob, bcChanPoint, payHashSettled[:])
	ht.AssertOutgoingHTLCActive(bob, bcChanPoint, payHashHold[:])

	// Carol should have two incoming HTLCs on channel Bob -> Carol.
	ht.AssertIncomingHTLCActive(carol, bcChanPoint, payHashSettled[:])
	ht.AssertIncomingHTLCActive(carol, bcChanPoint, payHashHold[:])

	// Let Alice go offline. Once Bob later learns the preimage, he
	// couldn't settle it with Alice so he has to go onchain to collect it.
	ht.Shutdown(alice)

	// Carol settles the first invoice.
	carol.RPC.SettleInvoice(preimageSettled[:])

	// Let Carol go offline so we can focus on testing Bob's sweeping
	// behavior.
	ht.Shutdown(carol)

	// Bob should have settled his outgoing HTLC with Carol.
	ht.AssertHTLCNotActive(bob, bcChanPoint, payHashSettled[:])

	// We'll now mine enough blocks to trigger Bob to force close channel
	// Bob->Carol due to his outgoing HTLC is about to timeout. With the
	// default outgoing broadcast delta of zero, this will be the same
	// height as the htlc expiry height.
	numBlocks := padCLTV(uint32(
		invoiceReqHold.CltvExpiry - lncfg.DefaultOutgoingBroadcastDelta,
	))
	ht.MineBlocks(int(numBlocks))

	// Before we mine empty blocks to check the RBF behavior, we need to be
	// aware that Bob's incoming HTLC will expire before his outgoing HTLC
	// deadline is reached. This happens because the incoming HTLC is sent
	// onchain at CLTVDelta-BroadcastDelta=18-10=8, which means after 8
	// blocks are mined, we expect Bob force closes the channel Alice->Bob.
	blocksTillIncomingSweep := cltvDelta -
		lncfg.DefaultIncomingBroadcastDelta

	// Bob should now have two pending sweeps, one for the anchor on the
	// local commitment, the other on the remote commitment.
	ht.AssertNumPendingSweeps(bob, 2)

	// Assert Bob's force closing tx has been broadcast.
	ht.AssertNumTxsInMempool(1)

	// Mine the force close tx, which triggers Bob's contractcourt to offer
	// his outgoing HTLC to his sweeper.
	//
	// NOTE: HTLC outputs are only offered to sweeper when the force close
	// tx is confirmed and the CSV has reached.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Update the blocks left till Bob force closes Alice->Bob.
	blocksTillIncomingSweep--

	// Bob should have two pending sweeps, one for the anchor sweeping, the
	// other for the outgoing HTLC.
	ht.AssertNumPendingSweeps(bob, 2)

	// Mine one block to confirm Bob's anchor sweeping tx, which will
	// trigger his sweeper to publish the HTLC sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Update the blocks left till Bob force closes Alice->Bob.
	blocksTillIncomingSweep--

	// Bob should now have one sweep and one sweeping tx in the mempool.
	ht.AssertNumPendingSweeps(bob, 1)
	outgoingSweep := ht.GetNumTxsFromMempool(1)[0]

	// Check the shape of the sweeping tx - we expect it to be
	// 2-input-2-output as a wallet utxo is used and a required output is
	// made.
	require.Len(ht, outgoingSweep.TxIn, 2)
	require.Len(ht, outgoingSweep.TxOut, 2)

	// Calculate the ending fee rate.
	//
	// TODO(yy): the budget we use to sweep the first-level outgoing HTLC
	// is twice its value. This is a temporary mitigation to prevent
	// cascading FCs and the test should be updated once it's properly
	// fixed.
	outgoingBudget := 2 * invoiceAmt
	outgoingTxSize := ht.CalculateTxWeight(outgoingSweep)
	outgoingEndFeeRate := chainfee.NewSatPerKWeight(
		outgoingBudget, outgoingTxSize,
	)

	// Assert the initial sweeping tx is using the start fee rate.
	outgoingStartFeeRate := ht.CalculateTxFeeRate(outgoingSweep)
	require.InEpsilonf(ht, uint64(startFeeRate1),
		uint64(outgoingStartFeeRate), 0.01, "want %d, got %d",
		startFeeRate1, outgoingStartFeeRate)

	// Now the start fee rate is checked, we can calculate the fee rate
	// delta.
	outgoingFeeRateDelta := (outgoingEndFeeRate - outgoingStartFeeRate) /
		chainfee.SatPerKWeight(outgoingHTLCDeadline-1)

	// outgoingFuncPosition records the position of Bob's fee function used
	// for his outgoing HTLC sweeping tx.
	outgoingFuncPosition := int32(0)

	// assertSweepFeeRate is a helper closure that asserts the expected fee
	// rate is used at the given position for a sweeping tx.
	assertSweepFeeRate := func(sweepTx *wire.MsgTx,
		startFeeRate, delta chainfee.SatPerKWeight,
		txSize lntypes.WeightUnit,
		deadline, position int32, desc string) {

		// Bob's HTLC sweeping tx should be fee bumped.
		feeRate := ht.CalculateTxFeeRate(sweepTx)
		expectedFeeRate := startFeeRate + delta*chainfee.SatPerKWeight(
			position,
		)

		ht.Logf("Bob's %s HTLC (deadline=%v): txWeight=%v, want "+
			"feerate=%v, got feerate=%v, delta=%v", desc,
			deadline-position, txSize, expectedFeeRate,
			feeRate, delta)

		require.InEpsilonf(ht, uint64(expectedFeeRate), uint64(feeRate),
			0.01, "want %v, got %v in tx=%v", expectedFeeRate,
			feeRate, sweepTx.TxHash())
	}

	// We now mine enough blocks to trigger Bob to force close channel
	// Alice->Bob. Along the way, we will check his outgoing HTLC sweeping
	// tx is RBFed as expected.
	for i := 0; i < blocksTillIncomingSweep-1; i++ {
		// Mine an empty block. Since the sweeping tx is not confirmed,
		// Bob's fee bumper should increase its fees.
		ht.MineEmptyBlocks(1)

		// Update Bob's fee function position.
		outgoingFuncPosition++

		// We should see Bob's sweeping tx in the mempool.
		ht.AssertNumTxsInMempool(1)

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(outgoingSweep.TxHash())

		// Bob should still have the outgoing HTLC sweep.
		ht.AssertNumPendingSweeps(bob, 1)

		// We should see Bob's replacement tx in the mempool.
		outgoingSweep = ht.GetNumTxsFromMempool(1)[0]

		// Bob's outgoing HTLC sweeping tx should be fee bumped.
		assertSweepFeeRate(
			outgoingSweep, outgoingStartFeeRate,
			outgoingFeeRateDelta, outgoingTxSize,
			outgoingHTLCDeadline, outgoingFuncPosition, "Outgoing",
		)
	}

	// Once exited the above loop and mine one more block, we'd have mined
	// enough blocks to trigger Bob to force close his channel with Alice.
	ht.MineEmptyBlocks(1)

	// Update Bob's fee function position.
	outgoingFuncPosition++

	// Bob should now have three pending sweeps:
	// 1. the outgoing HTLC output.
	// 2. the anchor output from his local commitment.
	// 3. the anchor output from his remote commitment.
	ht.AssertNumPendingSweeps(bob, 3)

	// We should see two txns in the mempool:
	// 1. Bob's outgoing HTLC sweeping tx.
	// 2. Bob's force close tx for Alice->Bob.
	txns := ht.GetNumTxsFromMempool(2)

	// Find the force close tx - we expect it to have a single input.
	closeTx := txns[0]
	if len(closeTx.TxIn) != 1 {
		closeTx = txns[1]
	}

	// We don't care the behavior of the anchor sweep in this test, so we
	// mine the force close tx to trigger Bob's contractcourt to offer his
	// incoming HTLC to his sweeper.
	ht.MineBlockWithTx(closeTx)

	// Update Bob's fee function position.
	outgoingFuncPosition++

	// Bob should now have three pending sweeps:
	// 1. the outgoing HTLC output on Bob->Carol.
	// 2. the incoming HTLC output on Alice->Bob.
	// 3. the anchor sweeping on Alice-> Bob.
	ht.AssertNumPendingSweeps(bob, 3)

	// Mine one block, which will trigger his sweeper to publish his
	// incoming HTLC sweeping tx.
	ht.MineEmptyBlocks(1)

	// Update the fee function's positions.
	outgoingFuncPosition++

	// We should see three txns in the mempool:
	// 1. the outgoing HTLC sweeping tx.
	// 2. the incoming HTLC sweeping tx.
	// 3. the anchor sweeping tx.
	txns = ht.GetNumTxsFromMempool(3)

	abCloseTxid := closeTx.TxHash()

	// Identify the sweeping txns spent from Alice->Bob.
	txns = ht.FindSweepingTxns(txns, 2, abCloseTxid)

	// Identify the anchor and incoming HTLC sweeps - if the tx has 1
	// output, then it's the anchor sweeping tx.
	var incomingSweep, anchorSweep = txns[0], txns[1]
	if len(anchorSweep.TxOut) != 1 {
		incomingSweep, anchorSweep = anchorSweep, incomingSweep
	}

	// Calculate the ending fee rate for the incoming HTLC sweep.
	incomingBudget := invoiceAmt.MulF64(contractcourt.DefaultBudgetRatio)
	incomingTxSize := ht.CalculateTxWeight(incomingSweep)
	incomingEndFeeRate := chainfee.NewSatPerKWeight(
		incomingBudget, incomingTxSize,
	)

	// Assert the initial sweeping tx is using the start fee rate.
	incomingStartFeeRate := ht.CalculateTxFeeRate(incomingSweep)
	require.InEpsilonf(ht, uint64(startFeeRate2),
		uint64(incomingStartFeeRate), 0.01, "want %d, got %d in tx=%v",
		startFeeRate2, incomingStartFeeRate, incomingSweep.TxHash())

	// Now the start fee rate is checked, we can calculate the fee rate
	// delta.
	incomingFeeRateDelta := (incomingEndFeeRate - incomingStartFeeRate) /
		chainfee.SatPerKWeight(incomingHTLCDeadline-1)

	// incomingFuncPosition records the position of Bob's fee function used
	// for his incoming HTLC sweeping tx.
	incomingFuncPosition := int32(0)

	// Mine the anchor sweeping tx to reduce noise in this test.
	ht.MineBlockWithTx(anchorSweep)

	// Update the fee function's positions.
	outgoingFuncPosition++
	incomingFuncPosition++

	// identifySweepTxns is a helper closure that identifies the incoming
	// and outgoing HTLC sweeping txns. It always assumes there are two
	// sweeping txns in the mempool, and returns the incoming HTLC sweep
	// first.
	identifySweepTxns := func() (*wire.MsgTx, *wire.MsgTx) {
		// We should see two txns in the mempool:
		// 1. the outgoing HTLC sweeping tx.
		// 2. the incoming HTLC sweeping tx.
		txns = ht.GetNumTxsFromMempool(2)

		var incoming, outgoing *wire.MsgTx

		// The sweeping tx has two inputs, one from wallet, the other
		// from the force close tx. We now check whether the first tx
		// spends from the force close tx of Alice->Bob.
		found := fn.Any(func(inp *wire.TxIn) bool {
			return inp.PreviousOutPoint.Hash == abCloseTxid
		}, txns[0].TxIn)

		// If the first tx spends an outpoint from the force close tx
		// of Alice->Bob, then it must be the incoming HTLC sweeping
		// tx.
		if found {
			incoming, outgoing = txns[0], txns[1]
		} else {
			// Otherwise the second tx must be the incoming HTLC
			// sweep.
			incoming, outgoing = txns[1], txns[0]
		}

		return incoming, outgoing
	}

	//nolint:lll
	// For neutrino backend, we need to give it more time to sync the
	// blocks. There's a potential bug we need to fix:
	// 2024-04-18 23:36:07.046 [ERR] NTFN: unable to get missed blocks: starting height 487 is greater than ending height 486
	//
	// TODO(yy): investigate and fix it.
	time.Sleep(10 * time.Second)

	// We should see Bob's sweeping txns in the mempool.
	incomingSweep, outgoingSweep = identifySweepTxns()

	// We now mine enough blocks till we reach the end of the outgoing
	// HTLC's deadline. Along the way, we check the expected fee rates are
	// used for both incoming and outgoing HTLC sweeping txns.
	//
	// NOTE: We need to subtract 1 from the deadline as the budget must be
	// used up before the deadline.
	blocksLeft := outgoingHTLCDeadline - outgoingFuncPosition - 1
	for i := int32(0); i < blocksLeft; i++ {
		// Mine an empty block.
		ht.MineEmptyBlocks(1)

		// Update Bob's fee function position.
		outgoingFuncPosition++
		incomingFuncPosition++

		// We should see two txns in the mempool,
		// - the incoming HTLC sweeping tx.
		// - the outgoing HTLC sweeping tx.
		ht.AssertNumTxsInMempool(2)

		// Make sure Bob's old sweeping txns have been removed from the
		// mempool.
		ht.AssertTxNotInMempool(outgoingSweep.TxHash())
		ht.AssertTxNotInMempool(incomingSweep.TxHash())

		// Bob should have two pending sweeps:
		// 1. the outgoing HTLC output on Bob->Carol.
		// 2. the incoming HTLC output on Alice->Bob.
		ht.AssertNumPendingSweeps(bob, 2)

		// We should see Bob's replacement txns in the mempool.
		incomingSweep, outgoingSweep = identifySweepTxns()

		// Bob's outgoing HTLC sweeping tx should be fee bumped.
		assertSweepFeeRate(
			outgoingSweep, outgoingStartFeeRate,
			outgoingFeeRateDelta, outgoingTxSize,
			outgoingHTLCDeadline, outgoingFuncPosition, "Outgoing",
		)

		// Bob's incoming HTLC sweeping tx should be fee bumped.
		assertSweepFeeRate(
			incomingSweep, incomingStartFeeRate,
			incomingFeeRateDelta, incomingTxSize,
			incomingHTLCDeadline, incomingFuncPosition, "Incoming",
		)
	}

	// Mine an empty block.
	ht.MineEmptyBlocks(1)

	// We should see Bob's old txns in the mempool.
	currentIncomingSweep, currentOutgoingSweep := identifySweepTxns()
	require.Equal(ht, incomingSweep.TxHash(), currentIncomingSweep.TxHash())
	require.Equal(ht, outgoingSweep.TxHash(), currentOutgoingSweep.TxHash())

	// Mine a block to confirm the HTLC sweeps.
	ht.MineBlocksAndAssertNumTxes(1, 2)
}

// testSweepCommitOutputAndAnchor checks when a channel is force closed without
// any time-sensitive HTLCs, the anchor output is swept without any CPFP
// attempts. In addition, the to_local output should be swept using the
// specified deadline and budget.
//
// Setup:
//  1. Fund Alice with 1 UTXOs - she only needs one for the funding process,
//     and no wallet utxos are needed for her sweepings.
//  2. Fund Bob with no UTXOs - his sweeping txns don't need wallet utxos as he
//     doesn't need to sweep any time-sensitive outputs.
//  3. Alice opens a channel with Bob, and successfully sends him an HTLC.
//  4. Alice force closes the channel.
//
// Test:
//  1. Alice's anchor sweeping is not attempted, instead, it should be swept
//     together with her to_local output using the no deadline path.
//  2. Bob would also sweep his anchor and to_local outputs in a single
//     sweeping tx using the no deadline path.
//  3. Both Alice and Bob's RBF attempts are using the fee rates calculated
//     from the deadline and budget.
//  4. Wallet UTXOs requirements are met - neither Alice nor Bob needs wallet
//     utxos to finish their sweeps.
func testSweepCommitOutputAndAnchor(ht *lntest.HarnessTest) {
	// Setup testing params for Alice.
	//
	// deadline is the expected deadline when sweeping the anchor and
	// to_local output. We will use a customized deadline to test the
	// config.
	deadline := uint32(1000)

	// The actual deadline used by the fee function will be one block off
	// from the deadline configured as we require one block to be mined to
	// trigger the sweep.
	deadlineA, deadlineB := deadline-1, deadline-1

	// startFeeRate is returned by the fee estimator in sat/kw. This
	// will be used as the starting fee rate for the linear fee func used
	// by Alice. Since there are no time-sensitive HTLCs, Alice's sweeper
	// should start with the above default deadline, which will result in
	// the min relay fee rate being used since it's >= MaxBlockTarget.
	startFeeRate := chainfee.FeePerKwFloor

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	ht.SetFeeEstimateWithConf(startFeeRate, deadlineA)

	// toLocalCSV is the CSV delay for Alice's to_local output. We use a
	// small value to save us from mining blocks.
	//
	// NOTE: once the force close tx is confirmed, we expect anchor
	// sweeping starts. Then two more block later the commit output
	// sweeping starts.
	//
	// NOTE: The CSV value is chosen to be 3 instead of 2, to reduce the
	// possibility of flakes as there is a race between the two goroutines:
	// G1 - Alice's sweeper receives the commit output.
	// G2 - Alice's sweeper receives the new block mined.
	// G1 is triggered by the same block being received by Alice's
	// contractcourt, deciding the commit output is mature and offering it
	// to her sweeper. Normally, we'd expect G2 to be finished before G1
	// because it's the same block processed by both contractcourt and
	// sweeper. However, if G2 is delayed (maybe the sweeper is slow in
	// finishing its previous round), G1 may finish before G2. This will
	// cause the sweeper to add the commit output to its pending inputs,
	// and once G2 fires, it will then start sweeping this output,
	// resulting a valid sweep tx being created using her commit and anchor
	// outputs.
	//
	// TODO(yy): fix the above issue by making sure subsystems share the
	// same view on current block height.
	toLocalCSV := 3

	// htlcAmt is the amount of the HTLC in sats, this should be Alice's
	// to_remote amount that goes to Bob.
	htlcAmt := int64(100_000)

	// fundAmt is the funding amount.
	fundAmt := btcutil.Amount(1_000_000)

	// We now set up testing params for Bob.
	//
	// bobBalance is the push amount when Alice opens the channel with Bob.
	// We will use zero here so Bob's balance equals to the htlc amount by
	// the time Alice force closes.
	bobBalance := btcutil.Amount(0)

	// We now set up the force close scenario. Alice will open a channel
	// with Bob, send an HTLC, Bob settles it, and then Alice force closes
	// the channel without any pending HTLCs.
	//
	// Prepare node params.
	cfg := []string{
		"--protocol.anchors",
		fmt.Sprintf("--sweeper.nodeadlineconftarget=%v", deadline),
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", toLocalCSV),
	}
	openChannelParams := lntest.OpenChannelParams{
		Amt:     fundAmt,
		PushAmt: bobBalance,
	}

	// Create a two hop network: Alice -> Bob.
	chanPoints, nodes := createSimpleNetwork(ht, cfg, 2, openChannelParams)

	// Unwrap the results.
	chanPoint := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	invoice := &lnrpc.Invoice{
		Memo:       "bob",
		Value:      htlcAmt,
		CltvExpiry: finalCltvDelta,
	}
	resp := bob.RPC.AddInvoice(invoice)

	// Send a payment with a specified finalCTLVDelta, and assert it's
	// succeeded.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertSettled(alice, req)

	// Assert Alice's to_remote (Bob's to_local) output is the htlc amount.
	ht.AssertChannelLocalBalance(bob, chanPoint, htlcAmt)
	bobToLocal := htlcAmt

	// Get Alice's channel to calculate Alice's to_local output amount.
	aliceChan := ht.GetChannelByChanPoint(alice, chanPoint)
	expectedToLocal := int64(fundAmt) - aliceChan.CommitFee - htlcAmt -
		330*2

	// Assert Alice's to_local output is correct.
	aliceToLocal := aliceChan.LocalBalance
	require.EqualValues(ht, expectedToLocal, aliceToLocal)

	// Alice force closes the channel.
	ht.CloseChannelAssertPending(alice, chanPoint, true)

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	ht.AssertChannelWaitingClose(alice, chanPoint)

	// Alice should see 2 anchor sweeps for the local and remote commitment.
	// Even without HTLCs at stake the anchors are registered with the
	// sweeper subsytem.
	ht.AssertNumPendingSweeps(alice, 2)

	// Bob did not force close the channel therefore he should have no
	// pending sweeps.
	ht.AssertNumPendingSweeps(bob, 0)

	// Mine a block to confirm Alice's force closing tx. Once it's
	// confirmed, we should see both Alice and Bob's anchors being offered
	// to their sweepers.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Alice should have one pending sweep,
	// - anchor sweeping from her local commitment.
	ht.AssertNumPendingSweeps(alice, 1)

	// Bob should have two pending sweeps,
	// - anchor sweeping from the remote anchor on Alice's commit tx.
	// - commit sweeping from the to_remote on Alice's commit tx.
	ht.AssertNumPendingSweeps(bob, 2)

	// Mine one more empty block should trigger Bob's sweeping. Since we
	// use a CSV of 3, this means Alice's to_local output is one block away
	// from being mature.
	ht.MineEmptyBlocks(1)

	// We expect to see one sweeping tx in the mempool:
	// - Alice's anchor sweeping tx must have been failed due to the fee
	//   rate chosen in this test - the anchor sweep tx has no output.
	// - Bob's sweeping tx, which sweeps both his anchor and commit outputs.
	bobSweepTx := ht.GetNumTxsFromMempool(1)[0]

	// We expect two pending sweeps for Bob - anchor and commit outputs.
	pendingSweepBob := ht.AssertNumPendingSweeps(bob, 2)[0]

	// The sweeper may be one block behind contractcourt, so we double
	// check the actual deadline.
	//
	// TODO(yy): assert they are equal once blocks are synced via
	// `blockbeat`.
	currentHeight := int32(ht.CurrentHeight())
	actualDeadline := int32(pendingSweepBob.DeadlineHeight) - currentHeight
	if actualDeadline != int32(deadlineB) {
		ht.Logf("!!! Found unsynced block between sweeper and "+
			"contractcourt, expected deadline=%v, got=%v",
			deadlineB, actualDeadline)

		deadlineB = uint32(actualDeadline)
	}

	// Alice should still have one pending sweep - the anchor output.
	ht.AssertNumPendingSweeps(alice, 1)

	// We now check Bob's sweeping tx.
	//
	// Bob's sweeping tx should have 2 inputs, one from his commit output,
	// the other from his anchor output.
	require.Len(ht, bobSweepTx.TxIn, 2)

	// Because Bob is sweeping without deadline pressure, the starting fee
	// rate should be the min relay fee rate.
	bobStartFeeRate := ht.CalculateTxFeeRate(bobSweepTx)
	require.InEpsilonf(ht, uint64(chainfee.FeePerKwFloor),
		uint64(bobStartFeeRate), 0.01, "want %v, got %v",
		chainfee.FeePerKwFloor, bobStartFeeRate)

	// With Bob's starting fee rate being validated, we now calculate his
	// ending fee rate and fee rate delta.
	//
	// Bob sweeps two inputs - anchor and commit, so the starting budget
	// should come from the sum of these two.
	bobValue := btcutil.Amount(bobToLocal + 330)
	bobBudget := bobValue.MulF64(contractcourt.DefaultBudgetRatio)

	// Calculate the ending fee rate and fee rate delta used in his fee
	// function.
	bobTxWeight := ht.CalculateTxWeight(bobSweepTx)
	bobEndingFeeRate := chainfee.NewSatPerKWeight(bobBudget, bobTxWeight)
	bobFeeRateDelta := (bobEndingFeeRate - bobStartFeeRate) /
		chainfee.SatPerKWeight(deadlineB-1)

	// Mine an empty block, which should trigger Alice's contractcourt to
	// offer her commit output to the sweeper.
	ht.MineEmptyBlocks(1)

	// Alice should have both anchor and commit as the pending sweep
	// requests.
	aliceSweeps := ht.AssertNumPendingSweeps(alice, 2)
	aliceAnchor, aliceCommit := aliceSweeps[0], aliceSweeps[1]
	if aliceAnchor.AmountSat > aliceCommit.AmountSat {
		aliceAnchor, aliceCommit = aliceCommit, aliceAnchor
	}

	// The sweeper may be one block behind contractcourt, so we double
	// check the actual deadline.
	//
	// TODO(yy): assert they are equal once blocks are synced via
	// `blockbeat`.
	currentHeight = int32(ht.CurrentHeight())
	actualDeadline = int32(aliceCommit.DeadlineHeight) - currentHeight
	if actualDeadline != int32(deadlineA) {
		ht.Logf("!!! Found unsynced block between Alice's sweeper and "+
			"contractcourt, expected deadline=%v, got=%v",
			deadlineA, actualDeadline)

		deadlineA = uint32(actualDeadline)
	}

	// We now wait for 30 seconds to overcome the flake - there's a block
	// race between contractcourt and sweeper, causing the sweep to be
	// broadcast earlier.
	//
	// TODO(yy): remove this once `blockbeat` is in place.
	aliceStartPosition := 0
	var aliceFirstSweepTx *wire.MsgTx
	err := wait.NoError(func() error {
		mem := ht.GetRawMempool()
		if len(mem) != 2 {
			return fmt.Errorf("want 2, got %v in mempool: %v",
				len(mem), mem)
		}

		// If there are two txns, it means Alice's sweep tx has been
		// created and published.
		aliceStartPosition = 1

		txns := ht.GetNumTxsFromMempool(2)
		aliceFirstSweepTx = txns[0]

		// Reassign if the second tx is larger.
		if txns[1].TxOut[0].Value > aliceFirstSweepTx.TxOut[0].Value {
			aliceFirstSweepTx = txns[1]
		}

		return nil
	}, wait.DefaultTimeout)
	ht.Logf("Checking mempool got: %v", err)

	// Mine an empty block, which should trigger Alice's sweeper to publish
	// her commit sweep along with her anchor output.
	ht.MineEmptyBlocks(1)

	// If Alice has already published her initial sweep tx, the above mined
	// block would trigger an RBF. We now need to assert the mempool has
	// removed the replaced tx.
	if aliceFirstSweepTx != nil {
		ht.AssertTxNotInMempool(aliceFirstSweepTx.TxHash())
	}

	// We also remember the positions of fee functions used by Alice and
	// Bob. They will be used to calculate the expected fee rates later.
	//
	// Alice's sweeping tx has just been created, so she is at the starting
	// position. For Bob, due to the above mined blocks, his fee function
	// is now at position 2.
	alicePosition, bobPosition := uint32(aliceStartPosition), uint32(2)

	// We should see two txns in the mempool:
	// - Alice's sweeping tx, which sweeps her commit output at the
	//   starting fee rate - Alice's anchor output won't be swept with her
	//   commit output together because they have different deadlines.
	// - Bob's previous sweeping tx, which sweeps both his anchor and
	//   commit outputs, at the starting fee rate.
	txns := ht.GetNumTxsFromMempool(2)

	// Assume the first tx is Alice's sweeping tx, if the second tx has a
	// larger output value, then that's Alice's as her to_local value is
	// much gearter.
	aliceSweepTx := txns[0]
	bobSweepTx = txns[1]

	// Swap them if bobSweepTx is smaller.
	if bobSweepTx.TxOut[0].Value > aliceSweepTx.TxOut[0].Value {
		aliceSweepTx, bobSweepTx = bobSweepTx, aliceSweepTx
	}

	// We now check Alice's sweeping tx.
	//
	// Alice's sweeping tx should have a shape of 1-in-1-out since it's not
	// used for CPFP, so it shouldn't take any wallet utxos.
	require.Len(ht, aliceSweepTx.TxIn, 1)
	require.Len(ht, aliceSweepTx.TxOut, 1)

	// We now check Alice's sweeping tx to see if it's already published.
	//
	// TODO(yy): remove this check once we have better block control.
	aliceSweeps = ht.AssertNumPendingSweeps(alice, 2)
	aliceCommit = aliceSweeps[0]
	if aliceCommit.AmountSat < aliceSweeps[1].AmountSat {
		aliceCommit = aliceSweeps[1]
	}
	if aliceCommit.BroadcastAttempts > 1 {
		ht.Logf("!!! Alice's commit sweep has already been broadcast, "+
			"broadcast_attempts=%v", aliceCommit.BroadcastAttempts)
		alicePosition = aliceCommit.BroadcastAttempts
	}

	// Alice's sweeping tx should use the min relay fee rate as there's no
	// deadline pressure.
	aliceStartingFeeRate := chainfee.FeePerKwFloor

	// With Alice's starting fee rate being validated, we now calculate her
	// ending fee rate and fee rate delta.
	//
	// Alice sweeps two inputs - anchor and commit, so the starting budget
	// should come from the sum of these two. However, due to the value
	// being too large, the actual ending fee rate used should be the
	// sweeper's max fee rate configured.
	aliceTxWeight := uint64(ht.CalculateTxWeight(aliceSweepTx))
	aliceEndingFeeRate := sweep.DefaultMaxFeeRate.FeePerKWeight()
	aliceFeeRateDelta := (aliceEndingFeeRate - aliceStartingFeeRate) /
		chainfee.SatPerKWeight(deadlineA-1)

	aliceFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
	expectedFeeRateAlice := aliceStartingFeeRate +
		aliceFeeRateDelta*chainfee.SatPerKWeight(alicePosition)
	require.InEpsilonf(ht, uint64(expectedFeeRateAlice),
		uint64(aliceFeeRate), 0.02, "want %v, got %v",
		expectedFeeRateAlice, aliceFeeRate)

	// We now check Bob' sweeping tx.
	//
	// The above mined block will trigger Bob's sweeper to RBF his previous
	// sweeping tx, which will fail due to RBF rule#4 - the additional fees
	// paid are not sufficient. This happens as our default incremental
	// relay fee rate is 1 sat/vb, with the tx size of 771 weight units, or
	// 192 vbytes, we need to pay at least 192 sats more to be able to RBF.
	// However, since Bob's budget delta is (100_000 + 330) * 0.5 / 1008 =
	// 49.77 sats, it means Bob can only perform a successful RBF every 4
	// blocks.
	//
	// Assert Bob's sweeping tx is not RBFed.
	bobFeeRate := ht.CalculateTxFeeRate(bobSweepTx)
	expectedFeeRateBob := bobStartFeeRate
	require.InEpsilonf(ht, uint64(expectedFeeRateBob), uint64(bobFeeRate),
		0.01, "want %d, got %d", expectedFeeRateBob, bobFeeRate)

	// reloclateAlicePosition is a temp hack to find the actual fee
	// function position used for Alice. Due to block sync issue among the
	// subsystems, we can end up having this situation:
	// - sweeper is at block 2, starts sweeping an input with deadline 100.
	// - fee bumper is at block 1, and thinks the conf target is 99.
	// - new block 3 arrives, the func now is at position 2.
	//
	// TODO(yy): fix it using `blockbeat`.
	reloclateAlicePosition := func() {
		// Mine an empty block to trigger the possible RBF attempts.
		ht.MineEmptyBlocks(1)

		// Increase the positions for both fee functions.
		alicePosition++
		bobPosition++

		// We expect two pending sweeps for both nodes as we are mining
		// empty blocks.
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 2)

		// We expect to see both Alice's and Bob's sweeping txns in the
		// mempool.
		ht.AssertNumTxsInMempool(2)

		// Make sure Alice's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(aliceSweepTx.TxHash())

		// We should see two txns in the mempool:
		// - Alice's sweeping tx, which sweeps both her anchor and
		//   commit outputs, using the increased fee rate.
		// - Bob's previous sweeping tx, which sweeps both his anchor
		//   and commit outputs, at the possible increased fee rate.
		txns = ht.GetNumTxsFromMempool(2)

		// Assume the first tx is Alice's sweeping tx, if the second tx
		// has a larger output value, then that's Alice's as her
		// to_local value is much gearter.
		aliceSweepTx = txns[0]
		bobSweepTx = txns[1]

		// Swap them if bobSweepTx is smaller.
		if bobSweepTx.TxOut[0].Value > aliceSweepTx.TxOut[0].Value {
			aliceSweepTx, bobSweepTx = bobSweepTx, aliceSweepTx
		}

		// Alice's sweeping tx should be increased.
		aliceFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
		expectedFeeRate := aliceStartingFeeRate +
			aliceFeeRateDelta*chainfee.SatPerKWeight(alicePosition)

		ht.Logf("Alice(deadline=%v): txWeight=%v, want feerate=%v, "+
			"got feerate=%v, delta=%v", deadlineA-alicePosition,
			aliceTxWeight, expectedFeeRate, aliceFeeRate,
			aliceFeeRateDelta)

		nextPosition := alicePosition + 1
		nextFeeRate := aliceStartingFeeRate +
			aliceFeeRateDelta*chainfee.SatPerKWeight(nextPosition)

		// Calculate the distances.
		delta := math.Abs(float64(aliceFeeRate - expectedFeeRate))
		deltaNext := math.Abs(float64(aliceFeeRate - nextFeeRate))

		// Exit early if the first distance is smaller - it means we
		// are at the right fee func position.
		if delta < deltaNext {
			require.InEpsilonf(ht, uint64(expectedFeeRate),
				uint64(aliceFeeRate), 0.02, "want %v, got %v "+
					"in tx=%v", expectedFeeRate,
				aliceFeeRate, aliceSweepTx.TxHash())

			return
		}

		alicePosition++
		ht.Logf("Jump position for Alice(deadline=%v): txWeight=%v, "+
			"want feerate=%v, got feerate=%v, delta=%v",
			deadlineA-alicePosition, aliceTxWeight, nextFeeRate,
			aliceFeeRate, aliceFeeRateDelta)

		require.InEpsilonf(ht, uint64(nextFeeRate),
			uint64(aliceFeeRate), 0.02, "want %v, got %v in tx=%v",
			nextFeeRate, aliceFeeRate, aliceSweepTx.TxHash())
	}

	reloclateAlicePosition()

	// We now mine 7 empty blocks. For each block mined, we'd see Alice's
	// sweeping tx being RBFed. For Bob, he performs a fee bump every
	// block, but will only publish a tx every 4 blocks mined as some of
	// the fee bumps is not sufficient to meet the fee requirements
	// enforced by RBF. Since his fee function is already at position 1,
	// mining 7 more blocks means he will RBF his sweeping tx twice.
	for i := 1; i < 7; i++ {
		// Mine an empty block to trigger the possible RBF attempts.
		ht.MineEmptyBlocks(1)

		// Increase the positions for both fee functions.
		alicePosition++
		bobPosition++

		// We expect two pending sweeps for both nodes as we are mining
		// empty blocks.
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 2)

		// We expect to see both Alice's and Bob's sweeping txns in the
		// mempool.
		ht.AssertNumTxsInMempool(2)

		// Make sure Alice's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(aliceSweepTx.TxHash())

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool. Since Bob's sweeping tx will only be successfully
		// RBFed every 4 blocks, his old sweeping tx only will be
		// removed when there are 4 blocks increased.
		if bobPosition%4 == 0 {
			ht.AssertTxNotInMempool(bobSweepTx.TxHash())
		}

		// We should see two txns in the mempool:
		// - Alice's sweeping tx, which sweeps both her anchor and
		//   commit outputs, using the increased fee rate.
		// - Bob's previous sweeping tx, which sweeps both his anchor
		//   and commit outputs, at the possible increased fee rate.
		txns := ht.GetNumTxsFromMempool(2)

		// Assume the first tx is Alice's sweeping tx, if the second tx
		// has a larger output value, then that's Alice's as her
		// to_local value is much gearter.
		aliceSweepTx = txns[0]
		bobSweepTx = txns[1]

		// Swap them if bobSweepTx is smaller.
		if bobSweepTx.TxOut[0].Value > aliceSweepTx.TxOut[0].Value {
			aliceSweepTx, bobSweepTx = bobSweepTx, aliceSweepTx
		}

		// We now check Alice's sweeping tx.
		//
		// Alice's sweeping tx should have a shape of 1-in-1-out since
		// it's not used for CPFP, so it shouldn't take any wallet
		// utxos.
		require.Len(ht, aliceSweepTx.TxIn, 1)
		require.Len(ht, aliceSweepTx.TxOut, 1)

		// Alice's sweeping tx should be increased.
		aliceFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
		expectedFeeRateAlice := aliceStartingFeeRate +
			aliceFeeRateDelta*chainfee.SatPerKWeight(alicePosition)

		ht.Logf("Alice(deadline=%v): txWeight=%v, want feerate=%v, "+
			"got feerate=%v, delta=%v", deadlineA-alicePosition,
			aliceTxWeight, expectedFeeRateAlice, aliceFeeRate,
			aliceFeeRateDelta)

		require.InEpsilonf(ht, uint64(expectedFeeRateAlice),
			uint64(aliceFeeRate), 0.02, "want %v, got %v in tx=%v",
			expectedFeeRateAlice, aliceFeeRate,
			aliceSweepTx.TxHash())

		// We now check Bob' sweeping tx.
		bobFeeRate := ht.CalculateTxFeeRate(bobSweepTx)

		// accumulatedDelta is the delta that Bob has accumulated so
		// far. This will only be added when there's a successful RBF
		// attempt.
		accumulatedDelta := bobFeeRateDelta *
			chainfee.SatPerKWeight(bobPosition)

		// Bob's sweeping tx will only be successfully RBFed every 4
		// blocks.
		if bobPosition%4 == 0 {
			expectedFeeRateBob = bobStartFeeRate + accumulatedDelta
		}

		ht.Logf("Bob(deadline=%v): txWeight=%v, want feerate=%v, "+
			"got feerate=%v, delta=%v", deadlineB-bobPosition,
			bobTxWeight, expectedFeeRateBob, bobFeeRate,
			bobFeeRateDelta)

		require.InEpsilonf(ht, uint64(expectedFeeRateBob),
			uint64(bobFeeRate), 0.02, "want %d, got %d in tx=%v",
			expectedFeeRateBob, bobFeeRate, bobSweepTx.TxHash())
	}

	// Mine a block to confirm both sweeping txns, this is needed to clean
	// up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)
}

// createSimpleNetwork creates the specified number of nodes and makes a
// topology of `node1 -> node2 -> node3...`. Each node is created using the
// specified config, the neighbors are connected, and the channels are opened.
// Each node will be funded with a single UTXO of 1 BTC except the last one.
func createSimpleNetwork(ht *lntest.HarnessTest, nodeCfg []string,
	numNodes int, p lntest.OpenChannelParams) ([]*lnrpc.ChannelPoint,
	[]*node.HarnessNode) {

	// Make a slice of nodes.
	nodes := make([]*node.HarnessNode, numNodes)

	// Create new nodes.
	for i := range nodes {
		nodeName := fmt.Sprintf("Node%q", string(rune('A'+i)))
		n := ht.NewNode(nodeName, nodeCfg)
		nodes[i] = n
	}

	// Connect the nodes in a chain.
	for i := 1; i < len(nodes); i++ {
		nodeA := nodes[i-1]
		nodeB := nodes[i]
		ht.EnsureConnected(nodeA, nodeB)
	}

	// Fund all the nodes expect the last one.
	for i := 0; i < len(nodes)-1; i++ {
		node := nodes[i]
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, node)
	}

	// Mine 1 block to get the above coins confirmed.
	ht.MineBlocksAndAssertNumTxes(1, numNodes-1)

	// Open channels in batch to save blocks mined.
	reqs := make([]*lntest.OpenChannelRequest, 0, len(nodes)-1)
	for i := 0; i < len(nodes)-1; i++ {
		nodeA := nodes[i]
		nodeB := nodes[i+1]

		req := &lntest.OpenChannelRequest{
			Local:  nodeA,
			Remote: nodeB,
			Param:  p,
		}
		reqs = append(reqs, req)
	}
	resp := ht.OpenMultiChannelsAsync(reqs)

	// Make sure the nodes know each other's channels if they are public.
	if !p.Private {
		for _, node := range nodes {
			for _, chanPoint := range resp {
				ht.AssertTopologyChannelOpen(node, chanPoint)
			}
		}
	}

	return resp, nodes
}

// testBumpFee checks that when a new input is requested, it's first bumped via
// CPFP, then RBF. Along the way, we check the `BumpFee` can properly update
// the fee function used by supplying new params.
func testBumpFee(ht *lntest.HarnessTest) {
	runBumpFee(ht, ht.Alice)
}

// runBumpFee checks the `BumpFee` RPC can properly bump the fee of a given
// input.
func runBumpFee(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping BumpFee test for neutrino backend")
	}

	// startFeeRate is the min fee rate in sats/vbyte. This value should be
	// used as the starting fee rate when the default no deadline is used.
	startFeeRate := uint64(1)

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to Bob.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Alice sends a coin to herself.
	tx := ht.SendCoins(alice, alice, btcutil.SatoshiPerBitcoin)
	txid := tx.TxHash()

	// Alice now tries to bump the first output on this tx.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(0),
	}
	value := btcutil.Amount(tx.TxOut[0].Value)

	// assertPendingSweepResp is a helper closure that asserts the response
	// from `PendingSweep` RPC is returned with expected values. It also
	// returns the sweeping tx for further checks.
	assertPendingSweepResp := func(broadcastAttempts uint32, budget uint64,
		deadline uint32, startingFeeRate uint64) *wire.MsgTx {

		// Alice should still have one pending sweep.
		pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

		// Validate all fields returned from `PendingSweeps` are as
		// expected.
		require.Equal(ht, op.TxidBytes, pendingSweep.Outpoint.TxidBytes)
		require.Equal(ht, op.OutputIndex,
			pendingSweep.Outpoint.OutputIndex)
		require.Equal(ht, walletrpc.WitnessType_TAPROOT_PUB_KEY_SPEND,
			pendingSweep.WitnessType)
		require.EqualValuesf(ht, value, pendingSweep.AmountSat,
			"amount not matched: want=%d, got=%d", value,
			pendingSweep.AmountSat)
		require.True(ht, pendingSweep.Immediate)

		require.Equal(ht, broadcastAttempts,
			pendingSweep.BroadcastAttempts)
		require.EqualValuesf(ht, budget, pendingSweep.Budget,
			"budget not matched: want=%d, got=%d", budget,
			pendingSweep.Budget)

		// Since the request doesn't specify a deadline, we expect the
		// existing deadline to be used.
		require.Equalf(ht, deadline, pendingSweep.DeadlineHeight,
			"deadline height not matched: want=%d, got=%d",
			deadline, pendingSweep.DeadlineHeight)

		// Since the request specifies a starting fee rate, we expect
		// that to be used as the starting fee rate.
		require.Equalf(ht, startingFeeRate,
			pendingSweep.RequestedSatPerVbyte, "requested "+
				"starting fee rate not matched: want=%d, "+
				"got=%d", startingFeeRate,
			pendingSweep.RequestedSatPerVbyte)

		// We expect to see Alice's original tx and her CPFP tx in the
		// mempool.
		txns := ht.GetNumTxsFromMempool(2)

		// Find the sweeping tx - assume it's the first item, if it has
		// the same txid as the parent tx, use the second item.
		sweepTx := txns[0]
		if sweepTx.TxHash() == tx.TxHash() {
			sweepTx = txns[1]
		}

		return sweepTx
	}

	// assertFeeRateEqual is a helper closure that asserts the fee rate of
	// the pending sweep tx is equal to the expected fee rate.
	assertFeeRateEqual := func(expected uint64) {
		err := wait.NoError(func() error {
			// Alice should still have one pending sweep.
			pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

			if pendingSweep.SatPerVbyte == expected {
				return nil
			}

			return fmt.Errorf("expected current fee rate %d, got "+
				"%d", expected, pendingSweep.SatPerVbyte)
		}, wait.DefaultTimeout)
		require.NoError(ht, err, "fee rate not updated")
	}

	// assertFeeRateGreater is a helper closure that asserts the fee rate
	// of the pending sweep tx is greater than the expected fee rate.
	assertFeeRateGreater := func(expected uint64) {
		err := wait.NoError(func() error {
			// Alice should still have one pending sweep.
			pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

			if pendingSweep.SatPerVbyte > expected {
				return nil
			}

			return fmt.Errorf("expected current fee rate greater "+
				"than %d, got %d", expected,
				pendingSweep.SatPerVbyte)
		}, wait.DefaultTimeout)
		require.NoError(ht, err, "fee rate not updated")
	}

	// First bump request - we'll specify nothing except `Immediate` to let
	// the sweeper handle the fee, and we expect a fee func that has,
	// - starting fee rate: 1 sat/vbyte (min relay fee rate).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Since the request doesn't specify a deadline, we expect the default
	// deadline to be used.
	currentHeight := int32(ht.CurrentHeight())
	deadline := uint32(currentHeight + sweep.DefaultDeadlineDelta)

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 1.
	// - starting fee rate: 1 sat/vbyte (min relay fee rate).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	sweepTx1 := assertPendingSweepResp(1, uint64(value/2), deadline, 0)

	// Since the request doesn't specify a starting fee rate, we expect the
	// min relay fee rate is used as the current fee rate.
	assertFeeRateEqual(startFeeRate)

	// testFeeRate sepcifies a starting fee rate in sat/vbyte.
	const testFeeRate = uint64(100)

	// Second bump request - we will specify the fee rate and expect a fee
	// func that has,
	// - starting fee rate: 100 sat/vbyte.
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:   true,
		SatPerVbyte: testFeeRate,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx1.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 2.
	// - starting fee rate: 100 sat/vbyte.
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	sweepTx2 := assertPendingSweepResp(
		2, uint64(value/2), deadline, testFeeRate,
	)

	// We expect the requested starting fee rate to be the current fee
	// rate.
	assertFeeRateEqual(testFeeRate)

	// testBudget specifies a budget in sats.
	testBudget := uint64(float64(value) * 0.1)

	// Third bump request - we will specify the budget and expect a fee
	// func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 1008 (default deadline).
	// - budget: 10% of the input value.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
		Budget:    testBudget,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx2.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 3.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 1008 (default deadline).
	// - budget: 10% of the input value.
	sweepTx3 := assertPendingSweepResp(3, testBudget, deadline, 0)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Create a test deadline delta to use in the next test.
	testDeadlineDelta := uint32(100)
	deadlineHeight := uint32(currentHeight) + testDeadlineDelta

	// Fourth bump request - we will specify the deadline and expect a fee
	// func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100.
	// - budget: 10% of the input value, stays unchanged.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:  true,
		TargetConf: testDeadlineDelta,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx3.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 4.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100.
	// - budget: 10% of the input value, stays unchanged.
	sweepTx4 := assertPendingSweepResp(4, testBudget, deadlineHeight, 0)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Fifth bump request - we test the behavior of `Immediate` - every
	// time it's called, the fee function will keep increasing the fee rate
	// until the broadcast can succeed. The fee func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100, stays unchanged.
	// - budget: 10% of the input value, stays unchanged.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx4.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 5.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100, stays unchanged.
	// - budget: 10% of the input value, stays unchanged.
	sweepTx5 := assertPendingSweepResp(5, testBudget, deadlineHeight, 0)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	smallBudget := uint64(1000)

	// Finally, we test the behavior of lowering the fee rate. The fee func
	// that has,
	// - starting fee rate: 1 sat/vbyte.
	// - deadline: 1008.
	// - budget: 1000 sats.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:   true,
		SatPerVbyte: startFeeRate,
		Budget:      smallBudget,
		TargetConf:  uint32(sweep.DefaultDeadlineDelta),
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 6.
	// - starting fee rate: 1 sat/vbyte.
	// - deadline: 1008.
	// - budget: 1000 sats.
	sweepTx6 := assertPendingSweepResp(
		6, smallBudget, deadline, startFeeRate,
	)

	// Since this budget is too small to cover the RBF, we expect the
	// sweeping attempt to fail.
	//
	require.Equal(ht, sweepTx5.TxHash(), sweepTx6.TxHash(), "tx5 should "+
		"not be replaced: tx5=%v, tx6=%v", sweepTx5.TxHash(),
		sweepTx6.TxHash())

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Clean up the mempol.
	ht.MineBlocksAndAssertNumTxes(1, 2)
}
