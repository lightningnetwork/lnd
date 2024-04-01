package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing"
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
//     doesn't need to sweep anyt time-sensitive outputs.
//  3. Alice opens a channel with Bob, and sends him an HTLC without being
//     settled - we achieve this by letting Bob hold the preimage, which means
//     he will consider his incoming HTLC has no preimage.
//  4. Alice force closes the channel.
//
// Test:
//  1. Alice's force close tx should be CPFPed using the anchor output.
//  2. Bob attempts to sweep his anchor output and fails.
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
	// the its value after subtracting its own budget as the CPFP budget.
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

	// Mine an block so Alice's force closing tx stays in the mempool.
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

	// Assume the first one is the CPFP anchor sweep tx, and switch to the
	// second one if the first doesn't has a different txid
	currentSweepTx := txns[0]
	if currentSweepTx.TxHash() != sweepTx.TxHash() {
		currentSweepTx = txns[1]
	}

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
