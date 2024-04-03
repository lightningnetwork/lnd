package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// testSweepAnchorCPFPLocalForceClose checks when a channel is force closed by
// a local node with a time-sensitive HTLC, the anchor output is used for
// CPFPing the force close tx.
//
// Setup:
//  1. Fund Alice with 2 UTXOs - she will need two to sweep her anchors from
//     the local and remote commitments, with one of them being invalid.
//  2. Fund Bob with no UTXOs - his sweeping txns don't need wallet utxos as he
//     doesn't need to sweep any time-sensitive outputs.
//  3. Alice opens a channel with Bob, and sends him an HTLC without being
//     settled - we achieve this by letting Bob hold the preimage, which means
//     he will consider his incoming HTLC has no preimage.
//  4. Alice force closes the channel.
//
// Test:
//  1. Alice's force close tx should be CPFPed using the anchor output.
//  2. Bob attempts to sweep his anchor output and fails due to it's
//     uneconomical.
//  3. Alice's RBF attempt is using the fee rates calculated from the deadline
//     and budget.
//  4. Wallet UTXOs requirements are met - for Alice she needs at least 2, and
//     Bob he needs none.
func testSweepAnchorCPFPLocalForceClose(ht *lntest.HarnessTest) {
	// Setup testing params for Alice.
	//
	// startFeeRate is returned by the fee estimator in sat/kw. This
	// will be used as the starting fee rate for the linear fee func used
	// by Alice.
	startFeeRate := chainfee.SatPerKWeight(2000)

	// deadline is the expected deadline for the CPFP transaction.
	deadline := uint32(10)

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	ht.SetFeeEstimateWithConf(startFeeRate, deadline)

	// Calculate the final ctlv delta based on the expected deadline.
	finalCltvDelta := int32(deadline - uint32(routing.BlockPadding) + 1)

	// toLocalCSV is the CSV delay for Alice's to_local output. This value
	// is chosen so the commit sweep happens after the anchor sweep,
	// enabling us to focus on checking the fees in CPFP here.
	toLocalCSV := deadline * 2

	// htlcAmt is the amount of the HTLC in sats. With default settings,
	// this will give us 25000 sats as the budget to sweep the CPFP anchor
	// output.
	htlcAmt := btcutil.Amount(100_000)

	// Calculate the budget. Since it's a time-sensitive HTLC, we will use
	// its value after subtracting its own budget as the CPFP budget.
	valueLeft := htlcAmt.MulF64(1 - contractcourt.DefaultBudgetRatio)
	budget := valueLeft.MulF64(1 - contractcourt.DefaultBudgetRatio)

	// We now set up testing params for Bob.
	//
	// bobBalance is the push amount when Alice opens the channel with Bob.
	// We will use zero here so we can focus on testing the CPFP logic from
	// Alice's side here.
	bobBalance := btcutil.Amount(0)

	// Make sure our assumptions and calculations are correct.
	require.EqualValues(ht, 25000, budget)

	// We now set up the force close scenario. Alice will open a channel
	// with Bob, send an HTLC, and then force close it with a
	// time-sensitive outgoing HTLC.
	//
	// Prepare node params.
	cfg := []string{
		"--hodl.exit-settle",
		"--protocol.anchors",
		// Use a very large CSV, this way to_local outputs are never
		// swept so we can focus on testing HTLCs.
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", toLocalCSV),
	}
	openChannelParams := lntest.OpenChannelParams{
		Amt:     htlcAmt * 10,
		PushAmt: bobBalance,
	}

	// Create a two hop network: Alice -> Bob.
	chanPoints, nodes := createSimpleNetwork(ht, cfg, 2, openChannelParams)

	// Unwrap the results.
	chanPoint := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	// Send one more utxo to Alice - she will need two utxos to sweep the
	// anchor output living on the local and remote commits.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Send a payment with a specified finalCTLVDelta, which will be used
	// as our deadline later on when Alice force closes the channel.
	req := &routerrpc.SendPaymentRequest{
		Dest:           bob.PubKey[:],
		Amt:            int64(htlcAmt),
		PaymentHash:    ht.Random32Bytes(),
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	ht.AssertNumActiveHtlcs(alice, 1)
	ht.AssertNumActiveHtlcs(bob, 1)

	// Alice force closes the channel.
	_, closeTxid := ht.CloseChannelAssertPending(alice, chanPoint, true)

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	ht.AssertChannelWaitingClose(alice, chanPoint)

	// Alice should have two pending sweeps,
	// - anchor sweeping from her local commitment.
	// - anchor sweeping from her remote commitment (invalid).
	//
	// TODO(yy): consider only sweeping the anchor from the local
	// commitment. Previously we would sweep up to three versions of
	// anchors because we don't know which one will be confirmed - if we
	// only broadcast the local anchor sweeping, our peer can broadcast
	// their commitment tx and replaces ours. With the new fee bumping, we
	// should be safe to only sweep our local anchor since we RBF it on
	// every new block, which destroys the remote's ability to pin us.
	ht.AssertNumPendingSweeps(alice, 2)

	// Bob should have no pending sweeps here. Although he learned about
	// the force close tx, because he doesn't have any outgoing HTLCs, he
	// doesn't need to sweep anything.
	ht.AssertNumPendingSweeps(bob, 0)

	// Mine a block so Alice's force closing tx stays in the mempool, which
	// also triggers the sweep.
	ht.MineEmptyBlocks(1)

	// TODO(yy): we should also handle the edge case where the force close
	// tx confirms here - we should cancel the fee bumping attempt for this
	// anchor sweep and let it stay in mempool? Or should we unlease the
	// wallet input and ask the sweeper to re-sweep the anchor?
	// ht.MineBlocksAndAssertNumTxes(1, 1)

	// We now check the expected fee and fee rate are used for Alice.
	//
	// We should see Alice's anchor sweeping tx triggered by the above
	// block, along with Alice's force close tx.
	txns := ht.Miner.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Get the weight for Alice's sweep tx.
	txWeight := ht.CalculateTxWeight(sweepTx)

	// Calculate the fee and fee rate of Alice's sweeping tx.
	fee := uint64(ht.CalculateTxFee(sweepTx))
	feeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// Alice should start with the initial fee rate of 2000 sat/kw.
	startFee := startFeeRate.FeeForWeight(txWeight)

	// Calculate the expected delta increased per block.
	//
	// NOTE: Assume a wallet tr output is used for fee bumping, with the tx
	// weight of 725, we expect this value to be 2355.
	feeDeltaAlice := (budget - startFee).MulF64(1 / float64(10))

	// We expect the startingFee and startingFeeRate being used. Allow some
	// deviation because weight estimates during tx generation are
	// estimates.
	//
	// TODO(yy): unify all the units and types re int vs uint!
	require.InEpsilonf(ht, uint64(startFee), fee, 0.01,
		"want %d, got %d", startFee, fee)
	require.InEpsilonf(ht, uint64(startFeeRate), feeRate,
		0.01, "want %d, got %d", startFeeRate, fee)

	// Bob has no time-sensitive outputs, so he should sweep nothing.
	ht.AssertNumPendingSweeps(bob, 0)

	// We now mine deadline-1 empty blocks. For each block mined, Alice
	// should perform an RBF on her CPFP anchor sweeping tx. By the end of
	// this iteration, we expect Alice to use start sweeping her htlc
	// output after one more block.
	for i := uint32(1); i < deadline; i++ {
		// Mine an empty block. Since the sweeping tx is not confirmed,
		// Alice's fee bumper should increase its fees.
		ht.MineEmptyBlocks(1)

		// Alice should still have two pending sweeps,
		// - anchor sweeping from her local commitment.
		// - anchor sweeping from her remote commitment (invalid).
		ht.AssertNumPendingSweeps(alice, 2)

		// Make sure Alice's old sweeping tx has been removed from the
		// mempool.
		ht.Miner.AssertTxNotInMempool(sweepTx.TxHash())

		// We expect the fees to increase by i*delta.
		expectedFee := startFee + feeDeltaAlice.MulF64(float64(i))
		expectedFeeRate := chainfee.NewSatPerKWeight(
			expectedFee, uint64(txWeight),
		)

		// We should see Alice's anchor sweeping tx being fee bumped
		// since it's not confirmed, along with her force close tx.
		txns = ht.Miner.GetNumTxsFromMempool(2)

		// Find the sweeping tx.
		sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

		// Calculate the fee rate of Alice's new sweeping tx.
		feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

		// Calculate the fee of Alice's new sweeping tx.
		fee = uint64(ht.CalculateTxFee(sweepTx))

		ht.Logf("Alice(deadline=%v): txWeight=%v, expected: [fee=%d, "+
			"feerate=%v], got: [fee=%v, feerate=%v]", deadline-i,
			txWeight, expectedFee, expectedFeeRate, fee, feeRate)

		// Assert Alice's tx has the expected fee and fee rate.
		require.InEpsilonf(ht, uint64(expectedFee), fee, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFee, fee)
		require.InEpsilonf(ht, uint64(expectedFeeRate), feeRate, 0.01,
			"deadline=%v, want %d, got %d", i, expectedFeeRate,
			feeRate)
	}

	// Once out of the above loop, we should've mined deadline-1 blocks. If
	// we mine one more block, we'd use up all the CPFP budget.
	ht.MineEmptyBlocks(1)

	// Get the last sweeping tx - we should see two txns here, Alice's
	// anchor sweeping tx and her force close tx.
	txns = ht.Miner.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	sweepTx = ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Calculate the fee and fee rate of Alice's new sweeping tx.
	fee = uint64(ht.CalculateTxFee(sweepTx))
	feeRate = uint64(ht.CalculateTxFeeRate(sweepTx))

	// Alice should still have two pending sweeps,
	// - anchor sweeping from her local commitment.
	// - anchor sweeping from her remote commitment (invalid).
	ht.AssertNumPendingSweeps(alice, 2)

	// Mine one more block. Since Alice's budget has been used up, there
	// won't be any more sweeping attempts. We now assert this by checking
	// that the sweeping tx stayed unchanged.
	ht.MineEmptyBlocks(1)

	// Get the current sweeping tx and assert it stays unchanged.
	//
	// We expect two txns here, one for the anchor sweeping, the other for
	// the HTLC sweeping.
	txns = ht.Miner.GetNumTxsFromMempool(2)

	// Find the sweeping tx.
	currentSweepTx := ht.FindSweepingTxns(txns, 1, *closeTxid)[0]

	// Calculate the fee and fee rate of Alice's current sweeping tx.
	currentFee := uint64(ht.CalculateTxFee(sweepTx))
	currentFeeRate := uint64(ht.CalculateTxFeeRate(sweepTx))

	// Assert the anchor sweep tx stays unchanged.
	require.Equal(ht, sweepTx.TxHash(), currentSweepTx.TxHash())
	require.Equal(ht, fee, currentFee)
	require.Equal(ht, feeRate, currentFeeRate)

	// Mine a block to confirm Alice's sweeping and force close txns, this
	// is needed to clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// The above mined block should confirm Alice's force close tx, and her
	// contractcourt will offer the HTLC to her sweeper. We are not testing
	// the HTLC sweeping behaviors so we just perform a simple check and
	// exit the test.
	ht.AssertNumPendingSweeps(alice, 1)
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

	// startFeeRate is returned by the fee estimator in sat/kw. This
	// will be used as the starting fee rate for the linear fee func used
	// by Alice. Since there are no time-sensitive HTLCs, Alice's sweeper
	// should start with the above default deadline, which will result in
	// the min relay fee rate being used since it's >= MaxBlockTarget.
	startFeeRate := chainfee.FeePerKwFloor

	// Set up the fee estimator to return the testing fee rate when the
	// conf target is the deadline.
	ht.SetFeeEstimateWithConf(startFeeRate, deadline)

	// toLocalCSV is the CSV delay for Alice's to_local output. We use a
	// small value to save us from mining blocks.
	//
	// NOTE: once the force close tx is confirmed, we expect anchor
	// sweeping starts. Then one more block later the commit output
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

	// TODO(yy): It looks like the funder is paying the fundee's anchor?
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

	// We should see neither Alice or Bob has any pending sweeps as there
	// are no time-sensitive HTLCs.
	ht.AssertNumPendingSweeps(alice, 0)
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

	// Mine one more empty block should trigger the sweeping. Since we use
	// a CSV of 2, this means Alice's to_local output is now mature and
	// should be offered to her sweeper.
	ht.MineEmptyBlocks(1)

	// NOTE: We must call `GetNumTxsFromMempool` before
	// `AssertNumPendingSweeps` to reduce the possibility of flakes as
	// there is a race between the two goroutines:
	// G1 - Alice's sweeper receives the commit output.
	// G2 - Alice's sweeper receives the new block mined above.
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
	//
	// We expect to see a single sweeping tx in the mempool:
	// - Alice's anchor sweeping tx must have been failed due to the fee
	//   rate chosen in this test - the anchor sweep tx has no output.
	// - Bob's sweeping tx, which sweeps both his anchor and commit outputs.
	bobSweepTx := ht.Miner.GetNumTxsFromMempool(1)[0]

	// We expect two pending sweeps for both nodes - anchor and commit
	// outputs.
	ht.AssertNumPendingSweeps(alice, 2)
	ht.AssertNumPendingSweeps(bob, 2)

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
	bobTxWeight := uint64(ht.CalculateTxWeight(bobSweepTx))
	bobEndingFeeRate := chainfee.NewSatPerKWeight(bobBudget, bobTxWeight)
	bobFeeRateDelta := (bobEndingFeeRate - bobStartFeeRate) /
		chainfee.SatPerKWeight(deadline)

	// Mine an empty block, which should trigger Alice's sweeper to publish
	// her commit sweep along with her anchor output.
	ht.MineEmptyBlocks(1)

	// We also remember the positions of fee functions used by Alice and
	// Bob. They will be used to calculate the expected fee rates later.
	//
	// Alice's sweeping tx has just been created, so she is at the starting
	// position. For Bob, due to the above mined block, his fee function is
	// now at position 1.
	alicePosition, bobPosition := uint32(0), uint32(1)

	// We should see two txns in the mempool:
	// - Alice's sweeping tx, which sweeps both her anchor and commit
	//   outputs, at the starting fee rate.
	// - Bob's previous sweeping tx, which sweeps both his anchor and
	//   commit outputs, at the starting fee rate.
	txns := ht.Miner.GetNumTxsFromMempool(2)

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
	// Alice's sweeping tx should have a shape of 2-in-1-out since it's not
	// used for CPFP, so it shouldn't take any wallet utxos.
	require.Len(ht, aliceSweepTx.TxIn, 2)
	require.Len(ht, aliceSweepTx.TxOut, 1)

	// Alice's sweeping tx should use the min relay fee rate as there's no
	// deadline pressure.
	aliceStartingFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
	require.InEpsilonf(ht, uint64(chainfee.FeePerKwFloor),
		uint64(aliceStartingFeeRate), 0.01, "want %v, got %v",
		chainfee.FeePerKwFloor, aliceStartingFeeRate)

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
		chainfee.SatPerKWeight(deadline)

	// We expect two pending sweeps for both nodes as we are mining empty
	// blocks.
	ht.AssertNumPendingSweeps(alice, 2)
	ht.AssertNumPendingSweeps(bob, 2)

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

	// We now mine 7 empty blocks. For each block mined, we'd see Alice's
	// sweeping tx being RBFed. For Bob, he performs a fee bump every
	// block, but will only publish a tx every 4 blocks mined as some of
	// the fee bumps is not sufficient to meet the fee requirements
	// enforced by RBF. Since his fee function is already at position 1,
	// mining 7 more blocks means he will RBF his sweeping tx twice.
	for i := 0; i < 7; i++ {
		// Mine an empty block to trigger the possible RBF attempts.
		ht.MineEmptyBlocks(1)

		// Increase the positions for both fee functions.
		alicePosition++
		bobPosition++

		// We expect two pending sweeps for both nodes as we are mining
		// empty blocks.
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 2)

		// Make sure Alice's old sweeping tx has been removed from the
		// mempool.
		ht.Miner.AssertTxNotInMempool(aliceSweepTx.TxHash())

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool. Since Bob's sweeping tx will only be successfully
		// RBFed every 4 blocks, his old sweeping tx only will be
		// removed when there are 4 blocks increased.
		if bobPosition%4 == 0 {
			ht.Miner.AssertTxNotInMempool(bobSweepTx.TxHash())
		}

		// We should see two txns in the mempool:
		// - Alice's sweeping tx, which sweeps both her anchor and
		//   commit outputs, using the increased fee rate.
		// - Bob's previous sweeping tx, which sweeps both his anchor
		//   and commit outputs, at the possible increased fee rate.
		txns := ht.Miner.GetNumTxsFromMempool(2)

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
		// Alice's sweeping tx should have a shape of 2-in-1-out since
		// it's not used for CPFP, so it shouldn't take any wallet
		// utxos.
		require.Len(ht, aliceSweepTx.TxIn, 2)
		require.Len(ht, aliceSweepTx.TxOut, 1)

		// Alice's sweeping tx should be increased.
		aliceFeeRate := ht.CalculateTxFeeRate(aliceSweepTx)
		expectedFeeRateAlice := aliceStartingFeeRate +
			aliceFeeRateDelta*chainfee.SatPerKWeight(alicePosition)

		ht.Logf("Alice(deadline=%v): txWeight=%v, want feerate=%v, "+
			"got feerate=%v, delta=%v", deadline-alicePosition,
			aliceTxWeight, expectedFeeRateAlice, aliceFeeRate,
			aliceFeeRateDelta)

		require.InEpsilonf(ht, uint64(expectedFeeRateAlice),
			uint64(aliceFeeRate), 0.02, "want %v, got %v",
			expectedFeeRateAlice, aliceFeeRate)

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
			"got feerate=%v, delta=%v", deadline-bobPosition,
			bobTxWeight, expectedFeeRateBob, bobFeeRate,
			bobFeeRateDelta)

		require.InEpsilonf(ht, uint64(expectedFeeRateBob),
			uint64(bobFeeRate), 0.02, "want %d, got %d",
			expectedFeeRateBob, bobFeeRate)
	}

	// Mine a block to confirm both sweeping txns, this is needed to clean
	// up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)
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
	ht.MineBlocks(numBlocks)

	// Bob force closes the channel.
	// ht.CloseChannelAssertPending(bob, bcChanPoint, true)

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
	ht.Miner.AssertNumTxsInMempool(1)

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
	outgoingSweep := ht.Miner.GetNumTxsFromMempool(1)[0]

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
		outgoingBudget, uint64(outgoingTxSize),
	)

	// Assert the initial sweeping tx is using the start fee rate.
	outgoingStartFeeRate := ht.CalculateTxFeeRate(outgoingSweep)
	require.InEpsilonf(ht, uint64(startFeeRate1),
		uint64(outgoingStartFeeRate), 0.01, "want %d, got %d",
		startFeeRate1, outgoingStartFeeRate)

	// Now the start fee rate is checked, we can calculate the fee rate
	// delta.
	outgoingFeeRateDelta := (outgoingEndFeeRate - outgoingStartFeeRate) /
		chainfee.SatPerKWeight(outgoingHTLCDeadline)

	// outgoingFuncPosition records the position of Bob's fee function used
	// for his outgoing HTLC sweeping tx.
	outgoingFuncPosition := int32(0)

	// assertSweepFeeRate is a helper closure that asserts the expected fee
	// rate is used at the given position for a sweeping tx.
	assertSweepFeeRate := func(sweepTx *wire.MsgTx,
		startFeeRate, delta chainfee.SatPerKWeight, txSize int64,
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

		// Make sure Bob's old sweeping tx has been removed from the
		// mempool.
		ht.Miner.AssertTxNotInMempool(outgoingSweep.TxHash())

		// Bob should still have the outgoing HTLC sweep.
		ht.AssertNumPendingSweeps(bob, 1)

		// We should see Bob's replacement tx in the mempool.
		outgoingSweep = ht.Miner.GetNumTxsFromMempool(1)[0]

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
	txns := ht.Miner.GetNumTxsFromMempool(2)

	// Find the force close tx - we expect it to have a single input.
	closeTx := txns[0]
	if len(closeTx.TxIn) != 1 {
		closeTx = txns[1]
	}

	// We don't care the behavior of the anchor sweep in this test, so we
	// mine the force close tx to trigger Bob's contractcourt to offer his
	// incoming HTLC to his sweeper.
	ht.Miner.MineBlockWithTx(closeTx)

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
	txns = ht.Miner.GetNumTxsFromMempool(3)

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
		incomingBudget, uint64(incomingTxSize),
	)

	// Assert the initial sweeping tx is using the start fee rate.
	incomingStartFeeRate := ht.CalculateTxFeeRate(incomingSweep)
	require.InEpsilonf(ht, uint64(startFeeRate2),
		uint64(incomingStartFeeRate), 0.01, "want %d, got %d",
		startFeeRate2, incomingStartFeeRate)

	// Now the start fee rate is checked, we can calculate the fee rate
	// delta.
	incomingFeeRateDelta := (incomingEndFeeRate - incomingStartFeeRate) /
		chainfee.SatPerKWeight(incomingHTLCDeadline)

	// incomingFuncPosition records the position of Bob's fee function used
	// for his incoming HTLC sweeping tx.
	incomingFuncPosition := int32(0)

	// Mine the anchor sweeping tx to reduce noise in this test.
	ht.Miner.MineBlockWithTxes([]*btcutil.Tx{btcutil.NewTx(anchorSweep)})

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
		txns = ht.Miner.GetNumTxsFromMempool(2)

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

	// We now mine enough blocks till we reach the end of the outgoing
	// HTLC's deadline. Along the way, we check the expected fee rates are
	// used for both incoming and outgoing HTLC sweeping txns.
	blocksLeft := outgoingHTLCDeadline - outgoingFuncPosition
	for i := int32(0); i < blocksLeft; i++ {
		// Mine an empty block.
		ht.MineEmptyBlocks(1)

		// Update Bob's fee function position.
		outgoingFuncPosition++
		incomingFuncPosition++

		// Make sure Bob's old sweeping txns have been removed from the
		// mempool.
		ht.Miner.AssertTxNotInMempool(outgoingSweep.TxHash())
		ht.Miner.AssertTxNotInMempool(incomingSweep.TxHash())

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
	ht.MineBlocks(1)

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
