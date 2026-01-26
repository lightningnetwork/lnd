package itest

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/rpc"
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
	ht.SetFeeEstimateWithConf(startFeeRateAnchor, deadlineDeltaAnchor)

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
	cfgs := [][]string{cfg, cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

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

	// Assert Bob's force closing tx has been broadcast. We should see two
	// txns in the mempool:
	// 1. Bob's force closing tx.
	// 2. Bob's anchor sweeping tx CPFPing the force close tx.
	_, sweepTx := ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Remember the force close height so we can calculate the deadline
	// height.
	forceCloseHeight := ht.CurrentHeight()

	var anchorSweep *walletrpc.PendingSweep

	// Bob should have one pending sweep,
	// - anchor sweeping from his local commitment.
	//
	// TODO(yy): consider only sweeping the anchor from the local
	// commitment. Previously we would sweep up to three versions of
	// anchors because we don't know which one will be confirmed - if we
	// only broadcast the local anchor sweeping, our peer can broadcast
	// their commitment tx and replaces ours. With the new fee bumping, we
	// should be safe to only sweep our local anchor since we RBF it on
	// every new block, which destroys the remote's ability to pin us.
	expectedNumSweeps := 1

	// For neutrino backend, Bob would have two anchor sweeps - one from
	// the local and the other from the remote.
	if ht.IsNeutrinoBackend() {
		expectedNumSweeps = 2
	}

	anchorSweep = ht.AssertNumPendingSweeps(bob, expectedNumSweeps)[0]

	// The anchor sweeping should have the expected deadline height.
	deadlineHeight := forceCloseHeight + deadlineDeltaAnchor
	require.Equal(ht, deadlineHeight, anchorSweep.DeadlineHeight)

	// Remember the deadline height for the CPFP anchor.
	anchorDeadline := anchorSweep.DeadlineHeight

	// Get the weight for Bob's anchor sweeping tx.
	txWeight := ht.CalculateTxWeight(sweepTx)

	// Bob should start with the initial fee rate of 2500 sat/kw.
	startFeeAnchor := startFeeRateAnchor.FeeForWeight(txWeight)

	// Calculate the fee and fee rate of Bob's sweeping tx.
	fee := uint64(ht.CalculateTxFee(sweepTx))
	feeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// feeFuncWidth is the width of the fee function. The fee function
	// maxes out one block before the deadline, so the width is the
	// original deadline minus 1.
	feeFuncWidth := deadlineDeltaAnchor - 1

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

		// Bob should still have the anchor sweeping from his local
		// commitment. His anchor sweeping from his remote commitment
		// is invalid and should be removed.
		ht.AssertNumPendingSweeps(bob, expectedNumSweeps)

		// We expect to see two txns in the mempool,
		// - Bob's force close tx.
		// - Bob's anchor sweep tx.
		ht.AssertNumTxsInMempool(2)

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(sweepTx.TxHash())

		// Assert the two txns are still in the mempool and grab the
		// sweeping tx.
		//
		// NOTE: must call it again after `AssertTxNotInMempool` to
		// make sure we get the replaced tx.
		_, sweepTx = ht.AssertForceCloseAndAnchorTxnsInMempool()

		// We expect the fees to increase by i*delta.
		expectedFee := startFeeAnchor + feeDelta.MulF64(float64(i))
		expectedFeeRate := chainfee.NewSatPerKWeight(
			expectedFee, txWeight,
		)

		// We should see Bob's anchor sweeping tx being fee bumped
		// since it's not confirmed, along with his force close tx.
		// Calculate the fee rate of Bob's new sweeping tx.
		feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

		// Calculate the fee of Bob's new sweeping tx.
		fee = uint64(ht.CalculateTxFee(sweepTx))

		ht.Logf("Bob(position=%v): txWeight=%v, expected: [fee=%d, "+
			"feerate=%v], got: [fee=%v, feerate=%v] in tx %v",
			feeFuncWidth-i, txWeight, expectedFee,
			expectedFeeRate, fee, feeRate, sweepTx.TxHash())

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

	// We expect to see two txns in the mempool,
	// - Bob's force close tx.
	// - Bob's anchor sweep tx.
	ht.AssertNumTxsInMempool(2)

	// Make sure Bob's old sweeping tx has been removed from the mempool.
	ht.AssertTxNotInMempool(sweepTx.TxHash())

	// Get the last sweeping tx - we should see two txns here, Bob's anchor
	// sweeping tx and his force close tx.
	//
	// NOTE: must call it again after `AssertTxNotInMempool` to make sure
	// we get the replaced tx.
	_, sweepTx = ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Bob should have the anchor sweeping from his local commitment.
	ht.AssertNumPendingSweeps(bob, expectedNumSweeps)

	// Mine one more block. Since Bob's budget has been used up, there
	// won't be any more sweeping attempts. We now assert this by checking
	// that the sweeping tx stayed unchanged.
	ht.MineEmptyBlocks(1)

	// Get the current sweeping tx and assert it stays unchanged.
	//
	// We expect two txns here, one for the anchor sweeping, the other for
	// the force close tx.
	_, currentSweepTx := ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Assert the anchor sweep tx stays unchanged.
	require.Equal(ht, sweepTx.TxHash(), currentSweepTx.TxHash())

	// Mine a block to confirm Bob's sweeping and force close txns, this is
	// needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	flakeRaceInBitcoinClientNotifications(ht)

	// The above mined block should confirm Bob's force close tx, and his
	// contractcourt will offer the HTLC to his sweeper. We are not testing
	// the HTLC sweeping behaviors so we just perform a simple check and
	// exit the test.
	ht.AssertNumPendingSweeps(bob, 2)
	ht.MineBlocksAndAssertNumTxes(1, 1)

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
	ht.SetFeeEstimateWithConf(startFeeRateAnchor, deadlineDeltaAnchor)

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
	cfgs := [][]string{cfg, cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

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
	flakeInconsistentHTLCView()
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

	// Assert Bob's force closing tx has been broadcast. We should see two
	// txns in the mempool:
	// 1. Bob's force closing tx.
	// 2. Bob's anchor sweeping tx CPFPing the force close tx.
	_, sweepTx := ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Bob should have one pending sweep,
	// - anchor sweeping from his local commitment.
	expectedNumSweeps := 1

	// For neutrino backend, Bob would have two anchor sweeps - one from
	// the local and the other from the remote.
	if ht.IsNeutrinoBackend() {
		expectedNumSweeps = 2
	}

	anchorSweep := ht.AssertNumPendingSweeps(bob, expectedNumSweeps)[0]

	// The anchor sweeping should have the expected deadline height.
	deadlineHeight := forceCloseHeight + deadlineDeltaAnchor
	require.Equal(ht, deadlineHeight, anchorSweep.DeadlineHeight)

	// Remember the deadline height for the CPFP anchor.
	anchorDeadline := anchorSweep.DeadlineHeight

	// Get the weight for Bob's anchor sweeping tx.
	txWeight := ht.CalculateTxWeight(sweepTx)

	// Bob should start with the initial fee rate of 2500 sat/kw.
	startFeeAnchor := startFeeRateAnchor.FeeForWeight(txWeight)

	// Calculate the fee and fee rate of Bob's sweeping tx.
	fee := uint64(ht.CalculateTxFee(sweepTx))
	feeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// feeFuncWidth is the width of the fee function. The fee function
	// maxes out one block before the deadline, so the width is the
	// original deadline minus 1.
	feeFuncWidth := deadlineDeltaAnchor - 1

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

		// Bob should still have the anchor sweeping from his local
		// commitment. His anchor sweeping from his remote commitment
		// is invalid and should be removed.
		ht.AssertNumPendingSweeps(bob, expectedNumSweeps)

		// We expect to see two txns in the mempool,
		// - Bob's force close tx.
		// - Bob's anchor sweep tx.
		ht.AssertNumTxsInMempool(2)

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.AssertTxNotInMempool(sweepTx.TxHash())

		// We expect to see two txns in the mempool,
		// - Bob's force close tx.
		// - Bob's anchor sweep tx.
		_, sweepTx = ht.AssertForceCloseAndAnchorTxnsInMempool()

		// We expect the fees to increase by i*delta.
		expectedFee := startFeeAnchor + feeDelta.MulF64(float64(i))
		expectedFeeRate := chainfee.NewSatPerKWeight(
			expectedFee, txWeight,
		)

		// Calculate the fee rate of Bob's new sweeping tx.
		feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

		// Calculate the fee of Bob's new sweeping tx.
		fee = uint64(ht.CalculateTxFee(sweepTx))

		ht.Logf("Bob(position=%v): txWeight=%v, expected: [fee=%d, "+
			"feerate=%v], got: [fee=%v, feerate=%v] in tx %v",
			feeFuncWidth-i, txWeight, expectedFee,
			expectedFeeRate, fee, feeRate, sweepTx.TxHash())

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

	// We expect to see two txns in the mempool,
	// - Bob's force close tx.
	// - Bob's anchor sweep tx.
	ht.AssertNumTxsInMempool(2)

	// Make sure Bob's old sweeping tx has been removed from the mempool.
	ht.AssertTxNotInMempool(sweepTx.TxHash())

	// Get the last sweeping tx - we should see two txns here, Bob's anchor
	// sweeping tx and his force close tx.
	_, sweepTx = ht.AssertForceCloseAndAnchorTxnsInMempool()

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
	_, currentSweepTx := ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Assert the anchor sweep tx stays unchanged.
	require.Equal(ht, sweepTx.TxHash(), currentSweepTx.TxHash())

	// Mine a block to confirm Bob's sweeping and force close txns, this is
	// needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// We mined a block above, which confirmed Bob's force closing tx and
	// his anchor sweeping tx. Bob should now have a new change output
	// created from that sweeping tx, which can be used as the input to
	// sweep his HTLC.
	// Also in the above mined block, the HTLC will be offered to Bob's
	// sweeper for sweeping, which requires a wallet utxo since it's a
	// zero fee HTLC.
	// There's the possible race that,
	// - btcwallet is processing this block, and marking the change output
	//   as confirmed.
	// - btcwallet notifies LND about the new block.
	// If the block notification comes first, LND's sweeper will not be
	// able to sweep this HTLC as it thinks the wallet UTXO is still
	// unconfirmed.
	// TODO(yy): To fix the above issue, we need to make sure btcwallet
	// should update its internal state first before notifying the new
	// block, which is scheduled to be fixed during the btcwallet SQLizing
	// saga.
	ht.MineEmptyBlocks(1)

	// The above mined block should confirm Bob's force close tx, and his
	// contractcourt will offer the HTLC to his sweeper. We are not testing
	// the HTLC sweeping behaviors so we just perform a simple check and
	// exit the test.
	ht.AssertNumPendingSweeps(bob, 1)
	ht.MineBlocksAndAssertNumTxes(1, 1)

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
	// to trigger the sweeper to sweep.
	outgoingHTLCDeadline := int32(cltvDelta - 1)
	incomingHTLCDeadline := int32(lncfg.DefaultIncomingBroadcastDelta - 1)

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
	cfgs := [][]string{cfg, cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 10,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

	// Unwrap the results.
	abChanPoint, bcChanPoint := chanPoints[0], chanPoints[1]
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// Bob needs two more wallet utxos:
	// - when sweeping anchors, he needs one utxo for each sweep.
	// - when sweeping HTLCs, he needs one utxo for each sweep.
	numUTXOs := 2

	// For neutrino backend, we need two more UTXOs for Bob to create his
	// sweeping txns.
	if ht.IsNeutrinoBackend() {
		numUTXOs += 2
	}

	ht.FundNumCoins(bob, numUTXOs)

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
		FeeLimitMsat:   noFeeLimitMsat,
	}
	req2 := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceHold.PaymentRequest,
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

	// Bob should have settled his outgoing HTLC with Carol.
	flakeInconsistentHTLCView()
	ht.AssertHTLCNotActive(bob, bcChanPoint, payHashSettled[:])

	// Let Carol go offline so we can focus on testing Bob's sweeping
	// behavior.
	ht.Shutdown(carol)

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
	expectedNumSweeps := 1

	// For neutrino backend, we expect the anchor output from his remote
	// commitment to be present.
	if ht.IsNeutrinoBackend() {
		expectedNumSweeps = 2
	}

	ht.AssertNumPendingSweeps(bob, expectedNumSweeps)

	// We expect to see two txns in the mempool:
	// 1. Bob's force closing tx.
	// 2. Bob's anchor CPFP sweeping tx.
	ht.AssertNumTxsInMempool(2)

	// Mine the force close tx and CPFP sweeping tx, which triggers Bob's
	// contractcourt to offer his outgoing HTLC to his sweeper.
	//
	// NOTE: HTLC outputs are only offered to sweeper when the force close
	// tx is confirmed and the CSV has reached.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Update the blocks left till Bob force closes Alice->Bob.
	blocksTillIncomingSweep--

	// Bob should have one pending sweep for the outgoing HTLC and another
	// one for his to_local output.
	ht.AssertNumPendingSweeps(bob, 2)

	// Bob should have one sweeping tx in the mempool.
	outgoingSweep := ht.GetNumTxsFromMempool(1)[0]

	// Check the shape of the sweeping tx - we expect it to be
	// 2-input-2-output as a wallet utxo is used and a required output is
	// made.
	require.Len(ht, outgoingSweep.TxIn, 2)
	require.Len(ht, outgoingSweep.TxOut, 2)

	// Calculate the ending fee rate.
	outgoingBudget := invoiceAmt.MulF64(contractcourt.DefaultBudgetRatio)
	outgoingTxSize := ht.CalculateTxWeight(outgoingSweep)
	outgoingEndFeeRate := chainfee.NewSatPerKWeight(
		outgoingBudget, outgoingTxSize,
	)

	// Assert the initial sweeping tx is using the start fee rate.
	outgoingStartFeeRate := ht.CalculateTxFeeRate(outgoingSweep)
	require.InEpsilonf(ht, uint64(startFeeRate1),
		uint64(outgoingStartFeeRate), 0.01, "want %d, got %d in tx=%v",
		startFeeRate1, outgoingStartFeeRate, outgoingSweep.TxHash())

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
			"feerate=%v, got feerate=%v, delta=%v in tx %v", desc,
			deadline-position, txSize, expectedFeeRate,
			feeRate, delta, sweepTx.TxHash())

		require.InEpsilonf(ht, uint64(expectedFeeRate), uint64(feeRate),
			0.01, "want %v, got %v", expectedFeeRate, feeRate)
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

		// Bob should still have the outgoing HTLC sweep and the
		// to_local output.
		ht.AssertNumPendingSweeps(bob, 2)

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

	// Bob should now have two pending sweeps:
	// 1. the outgoing HTLC output.
	// 2. the anchor output from his local commitment.
	// 3. the to_local output, which is not matured yet.
	expectedNumSweeps = 3

	// For neutrino backend, we expect the anchor output from his remote
	// commitment to be present.
	if ht.IsNeutrinoBackend() {
		expectedNumSweeps = 4
	}

	ht.AssertNumPendingSweeps(bob, expectedNumSweeps)

	// We should see three txns in the mempool:
	// 1. Bob's outgoing HTLC sweeping tx.
	// 2. Bob's force close tx for Alice->Bob.
	// 3. Bob's anchor CPFP sweeping tx for Alice->Bob.
	txns := ht.GetNumTxsFromMempool(3)

	// Find the force close tx - we expect it to have a single input.
	closeTx := txns[0]
	if len(closeTx.TxIn) != 1 {
		closeTx = txns[1]
	}
	if len(closeTx.TxIn) != 1 {
		closeTx = txns[2]
	}
	require.Len(ht, closeTx.TxIn, 1)

	// We don't care the behavior of the anchor sweep in this test, so we
	// mine the force close tx to trigger Bob's contractcourt to offer his
	// incoming HTLC to his sweeper.
	ht.MineBlockWithTx(closeTx)

	// Update Bob's fee function position.
	outgoingFuncPosition++

	// Bob should now have four pending sweeps:
	// 1. the outgoing HTLC output on Bob->Carol.
	// 2. the incoming HTLC output on Alice->Bob.
	// 3. the anchor sweeping on Alice-> Bob.
	// 4. the to_local output, immature.
	ht.AssertNumPendingSweeps(bob, 4)

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
		found := fn.Any(txns[0].TxIn, func(inp *wire.TxIn) bool {
			return inp.PreviousOutPoint.Hash == abCloseTxid
		})

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

	//nolint:ll
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

	ht.Logf("Bob has incoming sweep tx: %v, outgoing sweep tx: %v, "+
		"blocksLeft=%v, entering fee bumping now...",
		incomingSweep.TxHash(), outgoingSweep.TxHash(), blocksLeft)

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
		// 3. the to_local output, immature.
		ht.AssertNumPendingSweeps(bob, 3)

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
//  1. Alice's CPFP-anchor sweeping is not attempted, instead, it should be
//     swept using the no deadline path and failed due it's not economical.
//  2. Bob would also sweep his anchor and to_local outputs separately due to
//     they have different deadline heights, which means only the to_local
//     sweeping tx will succeed as the anchor sweeping is not economical.
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

	// deadlineA is the deadline used for Alice, given that,
	// - the force close tx is broadcast at height 445, her inputs are
	//   registered at the same height, so her to_local and anchor outputs
	//   have a deadline height of 1445.
	// - the force close tx is mined at 446, which means her anchor output
	//   now has a deadline delta of (1445-446) = 999 blocks.
	// - for her to_local output, with a deadline of 1000, the width of the
	//   fee func is CSV+1000-1. Given we are using a CSV of 2 here, her fee
	//   func deadline then becomes 1001.
	deadlineA := deadline + 1

	// deadlineB is the deadline used for Bob, the actual deadline used by
	// the fee function will be one block off from the deadline configured
	// as we require one block to be mined to trigger the sweep. In
	// addition, when sweeping his to_local output from Alice's commit tx,
	// because of CSV of 2, the starting height will be
	// "force_close_height+2", which means when the sweep request is
	// received by the sweeper, the actual deadline delta is "deadline+1".
	deadlineB := deadline + 1

	// startFeeRate is returned by the fee estimator in sat/kw. This
	// will be used as the starting fee rate for the linear fee func used
	// by Alice. Since there are no time-sensitive HTLCs, Alice's sweeper
	// should start with the above default deadline, which will result in
	// the min relay fee rate being used since it's >= MaxBlockTarget.
	startFeeRate := chainfee.FeePerKwFloor

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	ht.SetFeeEstimateWithConf(startFeeRate, deadlineB)

	// Set up the starting fee for Alice's anchor sweeping. With this low
	// fee rate, her anchor sweeping should be attempted and failed due to
	// dust output generated in the sweeping tx.
	ht.SetFeeEstimateWithConf(startFeeRate, deadline-1)

	// toLocalCSV is the CSV delay for Alice's to_local output. We use a
	// small value to save us from mining blocks.
	//
	// NOTE: once the force close tx is confirmed, we expect anchor
	// sweeping starts. Then two more block later the commit output
	// sweeping starts.
	toLocalCSV := 2

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
	cfgs := [][]string{cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt:     fundAmt,
		PushAmt: bobBalance,
	}

	// Create a two hop network: Alice -> Bob.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

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

	// Alice should have two pending sweeps,
	// - anchor sweeping from her local commitment.
	// - to_local output from her local commitment.
	ht.AssertNumPendingSweeps(alice, 2)

	// Bob should have two pending sweeps,
	// - anchor sweeping from the remote anchor on Alice's commit tx.
	// - commit sweeping from the to_remote on Alice's commit tx.
	ht.AssertNumPendingSweeps(bob, 2)

	// Bob's sweeper should have broadcast the commit output sweeping tx.
	// At the block which mined the force close tx, Bob's `chainWatcher`
	// will process the blockbeat first, which sends a signal to his
	// `ChainArbitrator` to launch the resolvers. Once launched, the sweep
	// requests will be sent to the sweeper. Finally, when the sweeper
	// receives this blockbeat, it will create the sweeping tx and publish
	// it.
	ht.AssertNumTxsInMempool(1)

	// Mine one more empty block should trigger Bob's sweeping. Since we
	// use a CSV of 2, this means Alice's to_local output is now mature.
	ht.MineEmptyBlocks(1)

	// We expect two pending sweeps for Bob - anchor and commit outputs.
	ht.AssertNumPendingSweeps(bob, 2)

	// We expect two pending sweeps for Alice - anchor and commit outputs.
	ht.AssertNumPendingSweeps(alice, 2)

	// We also remember the positions of fee functions used by Alice and
	// Bob. They will be used to calculate the expected fee rates later.
	alicePosition, bobPosition := uint32(0), uint32(1)

	// We should see two txns in the mempool:
	// - Alice's sweeping tx, which sweeps her commit output at the
	//   starting fee rate - Alice's anchor output won't be swept with her
	//   commit output together because they have different deadlines.
	// - Bob's previous sweeping tx, which sweeps his and commit outputs,
	//   at the starting fee rate.
	txns := ht.GetNumTxsFromMempool(2)

	// Assume the first tx is Alice's sweeping tx, if the second tx has a
	// larger output value, then that's Alice's as her to_local value is
	// much gearter.
	aliceSweepTx, bobSweepTx := txns[0], txns[1]

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

	// Alice's sweeping tx should use the min relay fee rate as there's no
	// deadline pressure.
	aliceStartingFeeRate := chainfee.FeePerKwFloor

	// With Alice's starting fee rate being validated, we now calculate her
	// ending fee rate and fee rate delta.
	//
	// Alice sweeps the to_local input, so the starting budget should come
	// from the to_local balance. However, due to the value being too large,
	// the actual ending fee rate used should be the sweeper's max fee rate
	// configured.
	aliceTxWeight := uint64(ht.CalculateTxWeight(aliceSweepTx))
	aliceEndingFeeRate := sweep.DefaultMaxFeeRate.FeePerKWeight()
	aliceFeeRateDelta := (aliceEndingFeeRate - aliceStartingFeeRate) /
		chainfee.SatPerKWeight(deadlineA)

	aliceFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
	expectedFeeRateAlice := aliceStartingFeeRate +
		aliceFeeRateDelta*chainfee.SatPerKWeight(alicePosition)
	require.InEpsilonf(ht, uint64(expectedFeeRateAlice),
		uint64(aliceFeeRate), 0.02, "want %v, got %v",
		expectedFeeRateAlice, aliceFeeRate)

	// We now check Bob's sweeping tx.
	//
	// Bob's sweeping tx should have one input, which is his commit output.
	// His anchor output won't be swept due to it being uneconomical.
	require.Len(ht, bobSweepTx.TxIn, 1, "tx=%v", bobSweepTx.TxHash())

	// Because Bob is sweeping without deadline pressure, the starting fee
	// rate should be the min relay fee rate.
	bobStartFeeRate := ht.CalculateTxFeeRate(bobSweepTx)
	require.InEpsilonf(ht, uint64(chainfee.FeePerKwFloor),
		uint64(bobStartFeeRate), 0.01, "want %v, got %v",
		chainfee.FeePerKwFloor, bobStartFeeRate)

	// With Bob's starting fee rate being validated, we now calculate his
	// ending fee rate and fee rate delta.
	//
	// Bob sweeps one input - the commit output.
	bobValue := btcutil.Amount(bobToLocal)
	bobBudget := bobValue.MulF64(contractcourt.DefaultBudgetRatio)

	// Calculate the ending fee rate and fee rate delta used in his fee
	// function.
	bobTxWeight := ht.CalculateTxWeight(bobSweepTx)
	bobEndingFeeRate := chainfee.NewSatPerKWeight(bobBudget, bobTxWeight)
	bobFeeRateDelta := (bobEndingFeeRate - bobStartFeeRate) /
		chainfee.SatPerKWeight(deadlineB-1)
	expectedFeeRateBob := bobStartFeeRate

	// We now mine 8 empty blocks. For each block mined, we'd see Alice's
	// sweeping tx being RBFed. For Bob, he performs a fee bump every
	// block, but will only publish a tx every 3 blocks mined as some of
	// the fee bumps is not sufficient to meet the fee requirements
	// enforced by RBF. Since his fee function is already at position 1,
	// mining 7 more blocks means he will RBF his sweeping tx twice.
	for i := 1; i < 9; i++ {
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
		// RBFed every 3 blocks, his old sweeping tx only will be
		// removed when there are 3 blocks increased.
		if bobPosition%3 == 0 {
			ht.AssertTxNotInMempool(bobSweepTx.TxHash())
		}

		// We should see two txns in the mempool:
		// - Alice's sweeping tx, which sweeps her commit output, using
		//   the increased fee rate.
		// - Bob's previous sweeping tx, which sweeps his commit output,
		//   at the possible increased fee rate.
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
			"got feerate=%v, delta=%v in tx %v",
			deadlineA-alicePosition, aliceTxWeight,
			expectedFeeRateAlice, aliceFeeRate,
			aliceFeeRateDelta, aliceSweepTx.TxHash())

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

		// Bob's sweeping tx will only be successfully RBFed every 3
		// blocks.
		if bobPosition%3 == 0 {
			expectedFeeRateBob = bobStartFeeRate + accumulatedDelta
		}

		ht.Logf("Bob(deadline=%v): txWeight=%v, want feerate=%v, "+
			"got feerate=%v, delta=%v in tx %v",
			deadlineB-bobPosition, bobTxWeight,
			expectedFeeRateBob, bobFeeRate,
			bobFeeRateDelta, bobSweepTx.TxHash())

		require.InEpsilonf(ht, uint64(expectedFeeRateBob),
			uint64(bobFeeRate), 0.02, "want %d, got %d in tx=%v",
			expectedFeeRateBob, bobFeeRate, bobSweepTx.TxHash())
	}

	// Mine a block to confirm both sweeping txns, this is needed to clean
	// up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Finally, assert that both Alice and Bob still have the anchor
	// outputs, which cannot be swept due to it being uneconomical.
	ht.AssertNumPendingSweeps(alice, 1)
	ht.AssertNumPendingSweeps(bob, 1)
}

// testBumpForceCloseFee tests that when a force close transaction, in
// particular a commitment which has no HTLCs at stake, can be bumped via the
// rpc endpoint `BumpForceCloseFee`.
//
// NOTE: This test does not check for a specific fee rate because channel force
// closures should be bumped taking a budget into account not a specific
// fee rate.
func testBumpForceCloseFee(ht *lntest.HarnessTest) {
	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping BumpForceCloseFee test for neutrino backend")
	}

	// fundAmt is the funding amount.
	fundAmt := btcutil.Amount(1_000_000)

	// We add a push amount because otherwise no anchor for the counter
	// party will be created which influences the commitment fee
	// calculation.
	pushAmt := btcutil.Amount(50_000)

	openChannelParams := lntest.OpenChannelParams{
		Amt:     fundAmt,
		PushAmt: pushAmt,
	}

	// Bumping the close fee rate is only possible for anchor channels.
	cfg := []string{
		"--protocol.anchors",
	}
	cfgs := [][]string{cfg, cfg}

	// Create a two hop network: Alice -> Bob.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

	// Unwrap the results.
	chanPoint := chanPoints[0]
	alice := nodes[0]
	bob := nodes[1]

	// We need to fund alice with 2 wallet inputs so that we can test to
	// increase the fee rate of the anchor cpfp via two subsequent calls of
	// the`BumpForceCloseFee` rpc cmd.
	//
	// TODO (ziggie): Make sure we use enough wallet inputs so that both
	// anchor transactions (local, remote commitment tx) can be created and
	// broadcasted. Not sure if we really need this, because we can be sure
	// as soon as one anchor transactions makes it into the mempool that the
	// others will fail anyways?
	ht.FundCoinsP2TR(btcutil.SatoshiPerBitcoin, alice)

	// Alice force closes the channel which has no HTLCs at stake.
	_, closeUpdates := ht.CloseChannelAssertPending(alice, chanPoint, true)
	require.NotNil(ht, closeUpdates)

	// Alice should see one waiting close channel.
	ht.AssertNumWaitingClose(alice, 1)

	// Alice should have 2 registered sweep inputs. The anchor of the local
	// commitment tx and the anchor of the remote commitment tx.
	ht.AssertNumPendingSweeps(alice, 2)

	// Calculate the commitment tx fee rate.
	pendingClose := closeUpdates.GetClosePending()
	closeTxid, err := chainhash.NewHash(pendingClose.Txid)
	require.NoError(ht, err)
	closingTx := ht.AssertTxInMempool(*closeTxid)
	require.NotNil(ht, closingTx)

	// The default commitment fee for anchor channels is capped at 2500
	// sat/kw but there might be some inaccuracies because of the witness
	// signature length therefore we calculate the exact value here.
	closingFeeRate := ht.CalculateTxFeeRate(closingTx)

	// We increase the fee rate of the fee function by 100% to make sure
	// we trigger a cpfp-transaction.
	newFeeRate := closingFeeRate * 2

	// We need to make sure that the budget can cover the fees for bumping.
	// However we also want to make sure that the budget is not too large
	// so that the delta of the fee function does not increase the feerate
	// by a single sat hence NOT rbfing the anchor sweep every time a new
	// block is found and a new sweep broadcast is triggered.
	//
	// NOTE:
	// We expect an anchor sweep with 2 inputs (anchor input + a wallet
	// input) and 1 p2tr output. This transaction has a weight of approx.
	// 725 wu. This info helps us to calculate the delta of the fee
	// function.
	// EndFeeRate: 100_000 sats/725 wu * 1000 = 137931 sat/kw
	// StartingFeeRate: 5000 sat/kw
	// delta = (137931-5000)/1008 = 132 sat/kw (which is lower than
	// 250 sat/kw) => hence we are violating BIP 125 Rule 4, which is
	// exactly what we want here to test the subsequent calling of the
	// bumpclosefee rpc.
	cpfpBudget := 100_000

	bumpFeeReq := &walletrpc.BumpForceCloseFeeRequest{
		ChanPoint:       chanPoint,
		StartingFeerate: uint64(newFeeRate.FeePerVByte()),
		Budget:          uint64(cpfpBudget),
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpForceCloseFee(bumpFeeReq)

	// We expect the initial closing transaction and the local anchor cpfp
	// transaction because alice force closed the channel.
	//
	// NOTE: We don't compare a feerate but only make sure that a cpfp
	// transaction was triggered. The sweeper increases the fee rate
	// periodically with every new incoming block and the selected fee
	// function.
	ht.AssertNumTxsInMempool(2)

	// Identify the cpfp anchor sweep.
	txns := ht.GetNumTxsFromMempool(2)
	cpfpSweep1 := ht.FindSweepingTxns(txns, 1, closingTx.TxHash())[0]

	// Mine an empty block and make sure the anchor cpfp is still in the
	// mempool hence the new block did not let the sweeper subsystem rbf
	// this anchor sweep transaction (because of the small fee delta).
	ht.MineEmptyBlocks(1)
	cpfpHash1 := cpfpSweep1.TxHash()
	ht.AssertTxInMempool(cpfpHash1)

	// Now Bump the fee rate again with a bigger starting fee rate of the
	// fee function.
	newFeeRate = closingFeeRate * 3

	bumpFeeReq = &walletrpc.BumpForceCloseFeeRequest{
		ChanPoint:       chanPoint,
		StartingFeerate: uint64(newFeeRate.FeePerVByte()),
		// The budget needs to be high enough to pay for the fee because
		// the anchor does not have an output value high enough to pay
		// for itself.
		Budget: uint64(cpfpBudget),
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpForceCloseFee(bumpFeeReq)

	// Make sure the old sweep is not in the mempool anymore, which proofs
	// that a new cpfp transaction replaced the old one paying higher fees.
	ht.AssertTxNotInMempool(cpfpHash1)

	// Identify the new cpfp transaction.
	// Both anchor sweeps result from the same closing tx (the local
	// commitment) hence proofing that the remote commitment transaction
	// and its cpfp transaction is invalid and not accepted into the
	// mempool.
	txns = ht.GetNumTxsFromMempool(2)
	ht.FindSweepingTxns(txns, 1, closingTx.TxHash())

	// Shut down Bob, otherwise he will create a sweeping tx to collect the
	// to_remote output once Alice's force closing tx is confirmed below.
	ht.Shutdown(bob)

	// Mine both transactions, the closing tx and the anchor cpfp tx.
	// This is needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)
}

// testFeeReplacement tests that when a sweeping txns aggregates multiple
// outgoing HTLCs, and one of the outgoing HTLCs has been spent via the direct
// preimage path by the remote peer, the remaining HTLCs will be grouped again
// and swept immediately.
//
// Setup:
//  1. Fund Alice with 1 UTXOs - she only needs one for the funding process,
//  2. Fund Bob with 3 UTXOs - he needs one for the funding process, one for
//     his CPFP anchor sweeping, and one for sweeping his outgoing HTLCs.
//  3. Create a linear network from Alice -> Bob -> Carol.
//  4. Alice pays two invoices to Carol, with Carol holding the settlement.
//  5. Bob goes offline.
//  6. Carol settles one of the invoices, so she can later spend Bob's outgoing
//     HTLC via the direct preimage path.
//  7. Carol goes offline and Bob comes online.
//  8. Mine enough blocks so Bob will force close Bob=>Carol to claim his
//     outgoing HTLCs.
//  9. Carol comes online, sweeps one of Bob's outgoing HTLCs and it confirms.
//  10. Bob creates a new sweeping tx to sweep his remaining HTLC with a
//     previous fee rate.
//
// Test:
//  1. Bob will immediately sweeps his remaining outgoing HTLC given that the
//     other one has been spent by Carol.
//  2. Bob's new sweeping tx will use the previous fee rate instead of
//     initializing a new starting fee rate.
func testFeeReplacement(ht *lntest.HarnessTest) {
	// Set the min relay feerate to be 10 sat/vbyte so the non-CPFP anchor
	// is never swept.
	//
	// TODO(yy): delete this line once the normal anchor sweeping is
	// removed.
	ht.SetMinRelayFeerate(10_000)

	// Setup testing params.
	//
	// Invoice is 100k sats.
	invoiceAmt := btcutil.Amount(100_000)

	// Alice will send two payments.
	numPayments := 2

	// Use the smallest CLTV so we can mine fewer blocks.
	cltvDelta := routing.MinCLTVDelta

	// Prepare params.
	cfg := []string{
		"--protocol.anchors",
		// Use a small CLTV to mine less blocks.
		fmt.Sprintf("--bitcoin.timelockdelta=%d", cltvDelta),
		// Use a very large CSV, this way to_local outputs are never
		// swept so we can focus on testing HTLCs.
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", cltvDelta*10),
	}
	cfgs := [][]string{cfg, cfg, cfg}

	openChannelParams := lntest.OpenChannelParams{
		Amt: invoiceAmt * 100,
	}

	// Create a three hop network: Alice -> Bob -> Carol.
	_, nodes := ht.CreateSimpleNetwork(cfgs, openChannelParams)

	// Unwrap the results.
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// Bob needs two more wallet utxos:
	// - when sweeping anchors, he needs one utxo for each sweep.
	// - when sweeping HTLCs, he needs one utxo for each sweep.
	numUTXOs := 2

	// For neutrino backend, we need two more UTXOs for Bob to create his
	// sweeping txns.
	if ht.IsNeutrinoBackend() {
		numUTXOs += 2
	}

	ht.FundNumCoins(bob, numUTXOs)

	// We also give Carol 2 coins to create her sweeping txns.
	ht.FundNumCoins(carol, 2)

	// Create numPayments HTLCs on Bob's incoming and outgoing channels.
	preimages := make([][]byte, 0, numPayments)
	streams := make([]rpc.SingleInvoiceClient, 0, numPayments)
	for i := 0; i < numPayments; i++ {
		// Create the preimage.
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		payHashHold := preimage.Hash()
		preimages = append(preimages, preimage[:])

		// Subscribe the invoices.
		stream := carol.RPC.SubscribeSingleInvoice(payHashHold[:])
		streams = append(streams, stream)

		// Carol create the hold invoice.
		invoiceReqHold := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      int64(invoiceAmt),
			CltvExpiry: finalCltvDelta,
			Hash:       payHashHold[:],
		}
		invoiceHold := carol.RPC.AddHoldInvoice(invoiceReqHold)

		// Let Alice pay the invoices.
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceHold.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}

		// Assert the payments are inflight.
		ht.SendPaymentAndAssertStatus(
			alice, req, lnrpc.Payment_IN_FLIGHT,
		)

		// Wait for Carol to mark invoice as accepted. There is a small
		// gap to bridge between adding the htlc to the channel and
		// executing the exit hop logic.
		ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)
	}

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLCs pending on all of them.
	//
	// Alice should have numPayments outgoing HTLCs on channel Alice -> Bob.
	ht.AssertNumActiveHtlcs(alice, numPayments)

	// Bob should have 2 * numPayments HTLCs,
	// - numPayments incoming HTLCs on channel Alice -> Bob.
	// - numPayments outgoing HTLCs on channel Bob -> Carol.
	ht.AssertNumActiveHtlcs(bob, numPayments*2)

	// Carol should have numPayments incoming HTLCs on channel Bob -> Carol.
	ht.AssertNumActiveHtlcs(carol, numPayments)

	// Suspend Bob so he won't get the preimage from Carol.
	restartBob := ht.SuspendNode(bob)

	// Carol settles the first invoice.
	carol.RPC.SettleInvoice(preimages[0])
	ht.AssertInvoiceState(streams[0], lnrpc.Invoice_SETTLED)

	// Carol goes offline so the preimage won't be sent to Bob.
	restartCarol := ht.SuspendNode(carol)

	// Bob comes online.
	require.NoError(ht, restartBob())

	// We'll now mine enough blocks to trigger Bob to force close channel
	// Bob->Carol due to his outgoing HTLC is about to timeout. With the
	// default outgoing broadcast delta of zero, this will be the same
	// height as the outgoing htlc's expiry height.
	numBlocks := padCLTV(uint32(
		finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta,
	))
	ht.MineEmptyBlocks(int(numBlocks))

	// Assert Bob's force closing tx has been broadcast. We should see two
	// txns in the mempool:
	// 1. Bob's force closing tx.
	// 2. Bob's anchor sweeping tx CPFPing the force close tx.
	ht.AssertForceCloseAndAnchorTxnsInMempool()

	// Mine a block to confirm Bob's force close tx and anchor sweeping tx
	// so we can focus on testing his outgoing HTLCs.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Bob should have numPayments pending sweep for the outgoing HTLCs. In
	// addition, he should see his immature to_local output sweep.
	ht.AssertNumPendingSweeps(bob, numPayments+1)

	// Bob should have one sweeping tx in the mempool, which sweeps all his
	// outgoing HTLCs.
	outgoingSweep0 := ht.GetNumTxsFromMempool(1)[0]

	// We now mine one empty block so Bob will perform one fee bump, after
	// which his sweeping tx should be updated with a new fee rate. We do
	// this so we can test later when Bob sweeps his remaining HTLC, the new
	// sweeping tx will start with the current fee rate.
	//
	// Calculate Bob's initial sweeping fee rate.
	initialFeeRate := ht.CalculateTxFeeRate(outgoingSweep0)

	// Mine one block to trigger Bob's RBF.
	ht.MineEmptyBlocks(1)

	// Make sure Bob's old sweeping tx has been removed from the mempool.
	ht.AssertTxNotInMempool(outgoingSweep0.TxHash())

	// Get the feerate of Bob's current sweeping tx.
	outgoingSweep1 := ht.GetNumTxsFromMempool(1)[0]
	currentFeeRate := ht.CalculateTxFeeRate(outgoingSweep1)

	// Assert the Bob has updated the fee rate.
	require.Greater(ht, currentFeeRate, initialFeeRate)

	delta := currentFeeRate - initialFeeRate

	// Check the shape of the sweeping tx - we expect it to be
	// 3-input-3-output as a wallet utxo is used and a required output is
	// made.
	require.Len(ht, outgoingSweep1.TxIn, numPayments+1)
	require.Len(ht, outgoingSweep1.TxOut, numPayments+1)

	// Restart Carol, once she is online, she will try to settle the HTLCs
	// via the direct preimage spend.
	require.NoError(ht, restartCarol())

	// Carol should have 1 incoming HTLC and 1 anchor output to sweep.
	ht.AssertNumPendingSweeps(carol, 2)

	// Assert Bob's sweeping tx has been replaced by Carol's.
	ht.AssertTxNotInMempool(outgoingSweep1.TxHash())
	carolSweepTx := ht.GetNumTxsFromMempool(1)[0]

	// Assume the miner is now happy with Carol's fee, and it gets included
	// in the next block.
	ht.MineBlockWithTx(carolSweepTx)

	// Upon receiving the above block, Bob should immediately create a
	// sweeping tx and broadcast it using the remaining outgoing HTLC.
	//
	// Bob should have numPayments-1 pending sweep for the outgoing HTLCs.
	// In addition, he should have his to_local output sweep which is
	// immature.
	ht.AssertNumPendingSweeps(bob, numPayments)

	// Assert Bob immediately sweeps his remaining HTLC with the previous
	// fee rate.
	outgoingSweep2 := ht.GetNumTxsFromMempool(1)[0]

	// Calculate the fee rate.
	feeRate := ht.CalculateTxFeeRate(outgoingSweep2)

	// We expect the current fee rate to be equal to the last fee rate he
	// used plus the delta, as we expect the fee rate to stay on the initial
	// line given by his fee function.
	expectedFeeRate := currentFeeRate + delta
	require.InEpsilonf(ht, uint64(expectedFeeRate),
		uint64(feeRate), 0.02, "want %d, got %d in tx=%v",
		currentFeeRate, feeRate, outgoingSweep2.TxHash())

	// Finally, clean the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}
