package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

const (
	finalCltvDelta  = routing.MinCLTVDelta // 18.
	thawHeightDelta = finalCltvDelta * 2   // 36.
)

var commitWithZeroConf = []struct {
	commitType lnrpc.CommitmentType
	zeroConf   bool
}{
	{
		commitType: lnrpc.CommitmentType_ANCHORS,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_ANCHORS,
		zeroConf:   true,
	},
	{
		commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		zeroConf:   true,
	},
	{
		commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		zeroConf:   false,
	},
	{
		commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		zeroConf:   true,
	},
}

// makeRouteHints creates a route hints that will allow Carol to be reached
// using an unadvertised channel created by Bob (Bob -> Carol). If the zeroConf
// bool is set, then the scid alias of Bob will be used in place.
func makeRouteHints(bob, carol *node.HarnessNode,
	zeroConf bool) []*lnrpc.RouteHint {

	carolChans := carol.RPC.ListChannels(
		&lnrpc.ListChannelsRequest{},
	)

	carolChan := carolChans.Channels[0]

	hopHint := &lnrpc.HopHint{
		NodeId: carolChan.RemotePubkey,
		ChanId: carolChan.ChanId,
		FeeBaseMsat: uint32(
			chainreg.DefaultBitcoinBaseFeeMSat,
		),
		FeeProportionalMillionths: uint32(
			chainreg.DefaultBitcoinFeeRate,
		),
		CltvExpiryDelta: chainreg.DefaultBitcoinTimeLockDelta,
	}

	if zeroConf {
		bobChans := bob.RPC.ListChannels(
			&lnrpc.ListChannelsRequest{},
		)

		// Now that we have Bob's channels, scan for the channel he has
		// open to Carol so we can use the proper scid.
		var found bool
		for _, bobChan := range bobChans.Channels {
			if bobChan.RemotePubkey == carol.PubKeyStr {
				hopHint.ChanId = bobChan.AliasScids[0]

				found = true

				break
			}
		}
		if !found {
			bob.Fatalf("unable to create route hint")
		}
	}

	return []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{hopHint},
		},
	}
}

// caseRunner defines a single test case runner.
type caseRunner func(ht *lntest.HarnessTest, alice, bob *node.HarnessNode,
	c lnrpc.CommitmentType, zeroConf bool)

// runMultiHopHtlcClaimTest is a helper method to build test cases based on
// different commitment types and zero-conf config and run them.
//
// TODO(yy): flatten this test.
func runMultiHopHtlcClaimTest(ht *lntest.HarnessTest, tester caseRunner) {
	for _, typeAndConf := range commitWithZeroConf {
		typeAndConf := typeAndConf
		name := fmt.Sprintf("zeroconf=%v/committype=%v",
			typeAndConf.zeroConf, typeAndConf.commitType.String())

		// Create the nodes here so that separate logs will be created
		// for Alice and Bob.
		args := lntest.NodeArgsForCommitType(typeAndConf.commitType)
		if typeAndConf.zeroConf {
			args = append(
				args, "--protocol.option-scid-alias",
				"--protocol.zero-conf",
			)
		}

		s := ht.Run(name, func(t1 *testing.T) {
			st := ht.Subtest(t1)

			alice := st.NewNode("Alice", args)
			bob := st.NewNode("Bob", args)
			st.ConnectNodes(alice, bob)

			// Start each test with the default static fee estimate.
			st.SetFeeEstimate(12500)

			// Add test name to the logs.
			alice.AddToLogf("Running test case: %s", name)
			bob.AddToLogf("Running test case: %s", name)

			tester(
				st, alice, bob,
				typeAndConf.commitType, typeAndConf.zeroConf,
			)
		})
		if !s {
			return
		}
	}
}

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopHtlcLocalChainClaim)
}

func runMultiHopHtlcLocalChainClaim(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// For neutrino backend, we need to fund one more UTXO for Carol so she
	// can sweep her outputs.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined uint32

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP as there's no
	// deadline pressure for Bob on the channel Alice->Bob at the moment.
	// For Bob's local commitment tx, there's only one incoming HTLC which
	// he doesn't have the preimage yet. Thus this anchor won't be
	// force-swept.
	hasAnchorSweep := false
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchorSweep, closeStream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	var expectedTxes int
	switch c {
	// Alice will sweep her commitment and anchor output immediately. Bob
	// will also offer his anchor to his sweeper.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 1)

		// We expect to see only one sweeping tx to be published from
		// Alice, which sweeps her commit and anchor outputs in the
		// same tx. For Bob, since his anchor is not used for CPFP,
		// it'd be uneconomical to sweep so it will fail.
		expectedTxes = 1

	// Alice will offer her anchor output to her sweeper. Her commitment
	// output cannot be swept yet as it has incurred an additional CLTV due
	// to being the initiator of a script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		ht.AssertNumPendingSweeps(alice, 1)
		ht.AssertNumPendingSweeps(bob, 1)

		// We expect to see only no sweeping txns to be published,
		// neither Alice's or Bob's anchor sweep can succeed due to
		// it's uneconomical.
		expectedTxes = 0

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Mine a block to trigger the sweeps.
	ht.MineEmptyBlocks(1)
	blocksMined++

	// Assert the expected num of txns are found in the mempool.
	ht.AssertNumTxsInMempool(expectedTxes)

	// Mine a block to clean up the mempool for the rest of the test.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)
	blocksMined++

	// Suspend Bob to force Carol to go to chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We now advance the block height to the point where Carol will force
	// close her channel with Bob, broadcast the closing tx but keep it
	// unconfirmed.
	numBlocks := padCLTV(uint32(invoiceReq.CltvExpiry -
		lncfg.DefaultIncomingBroadcastDelta))
	ht.MineEmptyBlocks(int(numBlocks - blocksMined))

	// Carol's commitment transaction should now be in the mempool.
	ht.AssertNumTxsInMempool(1)

	// Look up the closing transaction. It should be spending from the
	// funding transaction,
	closingTx := ht.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Mine a block that should confirm the commit tx.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, closingTxid)

	// After the force close transaction is mined, Carol should offer her
	// second-level success HTLC tx and anchor to the sweeper.
	ht.AssertNumPendingSweeps(carol, 2)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Lower the fee rate so Bob's two anchor outputs are economical to
	// be swept in one tx.
	ht.SetFeeEstimate(chainfee.FeePerKwFloor)

	// After the force close transaction is mined, transactions will be
	// broadcast by both Bob and Carol.
	switch c {
	// Carol will broadcast her sweeping txns and Bob will sweep his
	// commitment and anchor outputs, we'd expect to see three txns,
	// - Carol's second level HTLC transaction.
	// - Carol's anchor sweeping txns since it's used for CPFP.
	// - Bob's sweep tx spending his commitment output, and two anchor
	//   outputs, one from channel Alice to Bob and the other from channel
	//   Bob to Carol.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		ht.AssertNumPendingSweeps(bob, 3)
		expectedTxes = 3

	// Carol will broadcast her sweeping txns and Bob will sweep his
	// anchor outputs. Bob can't sweep his commitment output yet as it has
	// incurred an additional CLTV due to being the initiator of a
	// script-enforced leased channel:
	// - Carol's second level HTLC transaction.
	// - Carol's anchor sweeping txns since it's used for CPFP.
	// - Bob's sweep tx spending his two anchor outputs, one from channel
	//   Alice to Bob and the other from channel Bob to Carol.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		ht.AssertNumPendingSweeps(bob, 2)
		expectedTxes = 3

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Mine a block to trigger the sweeps.
	ht.MineEmptyBlocks(1)

	// Assert transactions can be found in the mempool.
	ht.AssertNumTxsInMempool(expectedTxes)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the expected transactions (+ the coinbase).
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// For a channel of the anchor type, we will subtract one block
	// from the default CSV, as the Sweeper will handle the input, and the
	// Sweeper sweeps the input as soon as the lock expires.
	secondLevelMaturity := uint32(defaultCSV - 1)

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := secondLevelMaturity

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and broadcast a second level tx to claim
	// the HTLC in his (already closed) channel with Alice.
	ht.AssertNumPendingSweeps(bob, 1)

	// Mine a block to trigger the sweep of the second level tx.
	ht.MineEmptyBlocks(1)
	carolSecondLevelCSV--

	// Check Bob's second level tx.
	bobSecondLvlTx := ht.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobSecondLvlTx, bobForceClose)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
	ht.AssertNumHTLCsAndStage(bob, aliceChanPoint, 1, 1)

	// The channel between Bob and Carol will still be pending force close
	// if this is a leased channel. In that case, we'd also check the HTLC
	// stages are correct in that channel.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.AssertNumPendingForceClose(bob, 2)
		ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)
	} else {
		ht.AssertNumPendingForceClose(bob, 1)
	}

	// We'll now mine a block which should confirm Bob's second layer
	// transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Keep track of Bob's second level maturity, and decrement our track
	// of Carol's.
	bobSecondLevelCSV := secondLevelMaturity
	carolSecondLevelCSV--

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine 1 additional blocks, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	ht.MineEmptyBlocks(int(carolSecondLevelCSV))
	bobSecondLevelCSV -= carolSecondLevelCSV

	// Carol should have one a sweep request for her second level tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// Mine a block to trigger the sweep.
	ht.MineEmptyBlocks(1)
	bobSecondLevelCSV--

	// Carol's sweep tx should be broadcast.
	carolSweep := ht.AssertNumTxsInMempool(1)[0]

	// Bob should offer his second level tx to his sweeper.
	ht.AssertNumPendingSweeps(bob, 1)

	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output.
	block = ht.MineBlocksAndAssertNumTxes(bobSecondLevelCSV, 1)[0]
	ht.AssertTxInBlock(block, carolSweep)

	bobSweep := ht.GetNumTxsFromMempool(1)[0]
	bobSweepTxid := bobSweep.TxHash()

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, bobSweepTxid)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 1)
		}

		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)
		ht.MineEmptyBlocks(int(numBlocks))

		// Both Alice and Bob should now offer their commit outputs to
		// the sweeper. For Alice, she still has her anchor output as
		// pending sweep as it's not used for CPFP, thus it's
		// uneconomical to sweep it alone.
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 1)

		// Mine a block to trigger the sweeps.
		ht.MineEmptyBlocks(1)

		// Both Alice and Bob show broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: bobForceClose, Index: 3,
		}
		ht.AssertOutpointInMempool(
			aliceCommitOutpoint,
		).TxHash()
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		ht.AssertOutpointInMempool(
			bobCommitOutpoint,
		).TxHash()

		// Confirm their sweeps.
		ht.MineBlocksAndAssertNumTxes(1, 2)
	}

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testMultiHopHtlcRemoteChainClaim tests that in the multi-hop HTLC scenario,
// if the remote party goes to chain while we have an incoming HTLC, then when
// we found out the preimage via the witness beacon, we properly settle the
// HTLC directly on-chain using the preimage in order to ensure that we don't
// lose any funds.
func testMultiHopHtlcRemoteChainClaim(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopHtlcRemoteChainClaim)
}

func runMultiHopHtlcRemoteChainClaim(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	const invoiceAmt = 100000
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// Wait for carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// blocksMined records how many blocks have mined after the creation of
	// the invoice so it can be used to calculate how many more blocks need
	// to be mined to trigger a force close later on.
	var blocksMined int

	// Lower the fee rate so Bob's two anchor outputs are economical to
	// be swept in one tx.
	ht.SetFeeEstimate(chainfee.FeePerKwFloor)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, aliceChanPoint, true,
	)
	aliceForceClose := ht.AssertStreamChannelForceClosed(
		alice, aliceChanPoint, true, closeStream,
	)

	// Increase the blocks mined. At this step
	// AssertStreamChannelForceClosed mines one block.
	blocksMined++

	// Wait for the channel to be marked pending force close.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint)

	// After AssertStreamChannelForceClosed returns, it has mined a block
	// so now bob will attempt to redeem his anchor output. Check the
	// anchor is offered to the sweeper.
	ht.AssertNumPendingSweeps(bob, 1)
	ht.AssertNumPendingSweeps(alice, 1)

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel. AssertStreamChannelForceClosed() already mined a
	// block containing the commitment tx and the commit sweep tx will be
	// broadcast immediately before it can be included in a block, so mine
	// one less than defaultCSV in order to perform mempool assertions.
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.MineEmptyBlocks(defaultCSV - blocksMined)
		blocksMined = defaultCSV

		// Alice should now sweep her funds.
		ht.AssertNumPendingSweeps(alice, 2)

		// Mine a block to trigger the sweep.
		ht.MineEmptyBlocks(1)
		blocksMined++

		// Mine Alice's commit sweeping tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
		blocksMined++
	}

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineEmptyBlocks(int(numBlocks) - blocksMined)

	// Carol's commitment transaction should now be in the mempool.
	ht.AssertNumTxsInMempool(1)

	// The closing transaction should be spending from the funding
	// transaction.
	closingTx := ht.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closingTxid := closingTx.TxHash()

	// Since Carol has time-sensitive HTLCs, she will use the anchor for
	// CPFP purpose. Assert she has two pending anchor sweep requests - one
	// from local commit and the other from remote commit.
	ht.AssertNumPendingSweeps(carol, 2)

	// Mine a block, which should contain: the commitment.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, closingTxid)

	// After the force close transaction is mined, Carol should offer her
	// second level HTLC tx to the sweeper, along with her anchor output.
	ht.AssertNumPendingSweeps(carol, 2)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// After the force close transaction is mined, we should expect Bob and
	// Carol to broadcast some transactions depending on the channel
	// commitment type.
	switch c {
	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a sweeping tx to sweep his commitment output and
	// anchor outputs from the two channels.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		ht.AssertNumPendingSweeps(bob, 3)

	// Carol should broadcast her second level HTLC transaction and Bob
	// should broadcast a transaction to sweep his anchor outputs. Bob
	// can't sweep his commitment output yet as he has incurred an
	// additional CLTV due to being the channel initiator of a force closed
	// script-enforced leased channel.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		ht.AssertNumPendingSweeps(bob, 2)

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Keep track of the second level tx maturity.
	carolSecondLevelCSV := uint32(defaultCSV)

	// Mine a block to trigger the sweeps, also confirms Carol's CPFP
	// anchor sweeping.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	carolSecondLevelCSV--
	ht.AssertNumTxsInMempool(2)

	// Mine a block to confirm the expected transactions.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// When Bob notices Carol's second level transaction in the block, he
	// will extract the preimage and offer the HTLC to his sweeper.
	ht.AssertNumPendingSweeps(bob, 1)

	// NOTE: after Bob is restarted, the sweeping of the direct preimage
	// spent will happen immediately so we don't need to mine a block to
	// trigger Bob's sweeper to sweep it.
	bobHtlcSweep := ht.GetNumTxsFromMempool(1)[0]
	bobHtlcSweepTxid := bobHtlcSweep.TxHash()

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobHtlcSweep, aliceForceClose)

	// We'll now mine a block which should confirm Bob's HTLC sweep
	// transaction.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, bobHtlcSweepTxid)
	carolSecondLevelCSV--

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts for the Bob-Carol channel have been
	// fully resolved
	aliceBobPendingChansLeft := 0
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		aliceBobPendingChansLeft = 1
	}
	for _, node := range []*node.HarnessNode{alice, bob} {
		ht.AssertNumPendingForceClose(
			node, aliceBobPendingChansLeft,
		)
	}

	// If we then mine 3 additional blocks, Carol's second level tx will
	// mature, and she should pull the funds.
	ht.MineEmptyBlocks(int(carolSecondLevelCSV))
	ht.AssertNumPendingSweeps(carol, 1)

	// Mine a block to trigger the sweep of the second level tx.
	ht.MineEmptyBlocks(1)
	carolSweep := ht.AssertNumTxsInMempool(1)[0]

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, carolSweep)
	ht.AssertNumPendingForceClose(carol, 0)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to the
	// additional CLTV. We'll need to mine enough blocks for the timelock to
	// expire and prompt their sweep.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Due to the way the test is set up, Alice and Bob share the
		// same CLTV for their commit outputs even though it's enforced
		// on different channels (Alice-Bob and Bob-Carol).
		resp := alice.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)

		// Mine enough blocks for the timelock to expire.
		numBlocks := int(forceCloseChan.BlocksTilMaturity)
		ht.MineEmptyBlocks(numBlocks)

		// Both Alice and Bob should offer their commit sweeps.
		ht.AssertNumPendingSweeps(alice, 2)
		ht.AssertNumPendingSweeps(bob, 1)

		// Mine a block to trigger the sweeps.
		ht.MineEmptyBlocks(1)

		// Both Alice and Bob should broadcast their commit sweeps.
		aliceCommitOutpoint := wire.OutPoint{
			Hash: aliceForceClose, Index: 3,
		}
		ht.AssertOutpointInMempool(aliceCommitOutpoint)
		bobCommitOutpoint := wire.OutPoint{Hash: closingTxid, Index: 3}
		ht.AssertOutpointInMempool(bobCommitOutpoint)

		// Confirm their sweeps.
		ht.MineBlocksAndAssertNumTxes(1, 2)

		// Alice and Bob should not show any pending channels anymore as
		// they have been fully resolved.
		for _, node := range []*node.HarnessNode{alice, bob} {
			ht.AssertNumPendingForceClose(node, 0)
		}
	}

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	invoice := ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)
	require.Equal(ht, int64(invoiceAmt), invoice.AmtPaidSat)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testMultiHopHtlcAggregation tests that in a multi-hop HTLC scenario, if we
// force close a channel with both incoming and outgoing HTLCs, we can properly
// resolve them using the second level timeout and success transactions. In
// case of anchor channels, the second-level spends can also be aggregated and
// properly feebumped, so we'll check that as well.
func testMultiHopHtlcAggregation(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runMultiHopHtlcAggregation)
}

func runMultiHopHtlcAggregation(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// We need one additional UTXO to create the sweeping tx for the
	// second-level success txes.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)

	// First, we'll create a three hop network: Alice -> Bob -> Carol.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice+Carol can actually find a route.
	var (
		carolRouteHints []*lnrpc.RouteHint
		aliceRouteHints []*lnrpc.RouteHint
	)
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		carolRouteHints = makeRouteHints(bob, carol, zeroConf)
		aliceRouteHints = makeRouteHints(bob, alice, zeroConf)
	}

	// To ensure we have capacity in both directions of the route, we'll
	// make a fairly large payment Alice->Carol and settle it.
	const reBalanceAmt = 500_000
	invoice := &lnrpc.Invoice{
		Value:      reBalanceAmt,
		RouteHints: carolRouteHints,
	}
	resp := carol.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// Make sure Carol has settled the invoice.
	ht.AssertInvoiceSettled(carol, resp.PaymentAddr)

	// With the network active, we'll now add a new hodl invoices at both
	// Alice's and Carol's end. Make sure the cltv expiry delta is large
	// enough, otherwise Bob won't send out the outgoing htlc.
	const numInvoices = 5
	const invoiceAmt = 50_000

	var (
		carolInvoices       []*invoicesrpc.AddHoldInvoiceResp
		aliceInvoices       []*invoicesrpc.AddHoldInvoiceResp
		alicePreimages      []lntypes.Preimage
		payHashes           [][]byte
		invoiceStreamsCarol []rpc.SingleInvoiceClient
		invoiceStreamsAlice []rpc.SingleInvoiceClient
	)

	// Add Carol invoices.
	for i := 0; i < numInvoices; i++ {
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: finalCltvDelta,
			Hash:       payHash[:],
			RouteHints: carolRouteHints,
		}
		carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

		carolInvoices = append(carolInvoices, carolInvoice)
		payHashes = append(payHashes, payHash[:])

		// Subscribe the invoice.
		stream := carol.RPC.SubscribeSingleInvoice(payHash[:])
		invoiceStreamsCarol = append(invoiceStreamsCarol, stream)
	}

	// We'll give Alice's invoices a longer CLTV expiry, to ensure the
	// channel Bob<->Carol will be closed first.
	for i := 0; i < numInvoices; i++ {
		var preimage lntypes.Preimage
		copy(preimage[:], ht.Random32Bytes())
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Value:      invoiceAmt,
			CltvExpiry: thawHeightDelta - 4,
			Hash:       payHash[:],
			RouteHints: aliceRouteHints,
		}
		aliceInvoice := alice.RPC.AddHoldInvoice(invoiceReq)

		aliceInvoices = append(aliceInvoices, aliceInvoice)
		alicePreimages = append(alicePreimages, preimage)
		payHashes = append(payHashes, payHash[:])

		// Subscribe the invoice.
		stream := alice.RPC.SubscribeSingleInvoice(payHash[:])
		invoiceStreamsAlice = append(invoiceStreamsAlice, stream)
	}

	// Now that we've created the invoices, we'll pay them all from
	// Alice<->Carol, going through Bob. We won't wait for the response
	// however, as neither will immediately settle the payment.

	// Alice will pay all of Carol's invoices.
	for _, carolInvoice := range carolInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: carolInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		alice.RPC.SendPayment(req)
	}

	// And Carol will pay Alice's.
	for _, aliceInvoice := range aliceInvoices {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: aliceInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		carol.RPC.SendPayment(req)
	}

	// At this point, all 3 nodes should now the HTLCs active on their
	// channels.
	ht.AssertActiveHtlcs(alice, payHashes...)
	ht.AssertActiveHtlcs(bob, payHashes...)
	ht.AssertActiveHtlcs(carol, payHashes...)

	// Wait for Alice and Carol to mark the invoices as accepted. There is
	// a small gap to bridge between adding the htlc to the channel and
	// executing the exit hop logic.
	for _, stream := range invoiceStreamsCarol {
		ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)
	}

	for _, stream := range invoiceStreamsAlice {
		ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)
	}

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We want Carol's htlcs to expire off-chain to demonstrate bob's force
	// close. However, Carol will cancel her invoices to prevent force
	// closes, so we shut her down for now.
	restartCarol := ht.SuspendNode(carol)

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the Carol's HTLCs are
	// about to timeout. With the default outgoing broadcast delta of zero,
	// this will be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineEmptyBlocks(int(numBlocks))

	// Bob's force close transaction should now be found in the mempool. If
	// there are anchors, we expect it to be offered to Bob's sweeper.
	ht.AssertNumTxsInMempool(1)

	// Bob has two anchor sweep requests, one for remote (invalid) and the
	// other for local.
	ht.AssertNumPendingSweeps(bob, 2)

	closeTx := ht.AssertOutpointInMempool(
		ht.OutPointFromChannelPoint(bobChanPoint),
	)
	closeTxid := closeTx.TxHash()

	// Go through the closing transaction outputs, and make an index for
	// the HTLC outputs.
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

		// If the HTLC has direction towards Alice, Bob will claim it
		// with the success TX when he learns the preimage. In this
		// case one extra sat will be on the output, because of the
		// routing fee.
		case invoiceAmt + 1:
			successOuts[op] = struct{}{}
		}
	}

	// Once bob has force closed, we can restart carol.
	require.NoError(ht, restartCarol())

	// Mine a block to confirm the closing transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// The above mined block will trigger Bob to sweep his anchor output.
	ht.AssertNumTxsInMempool(1)

	// Let Alice settle her invoices. When Bob now gets the preimages, he
	// has no other option than to broadcast his second-level transactions
	// to claim the money.
	for _, preimage := range alicePreimages {
		alice.RPC.SettleInvoice(preimage[:])
	}

	expectedTxes := 0
	switch c {
	// In case of anchors, all success transactions will be aggregated into
	// one, the same is the case for the timeout transactions. In this case
	// Carol will also sweep her commitment and anchor output in a single
	// tx.
	case lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
		lnrpc.CommitmentType_SIMPLE_TAPROOT:

		// Bob should have `numInvoices` for both HTLC success and
		// timeout txns, plus one anchor sweep.
		ht.AssertNumPendingSweeps(bob, numInvoices*2+1)

		// Carol should have commit and anchor outputs.
		ht.AssertNumPendingSweeps(carol, 2)

		// We expect to see three sweeping txns:
		// 1. Bob's sweeping tx for all timeout HTLCs.
		// 2. Bob's sweeping tx for all success HTLCs.
		// 3. Carol's sweeping tx for her commit and anchor outputs.
		expectedTxes = 3

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Mine a block to confirm Bob's anchor sweeping, which will also
	// trigger his sweeper to sweep HTLCs.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Assert the sweeping txns are found in the mempool.
	txes := ht.GetNumTxsFromMempool(expectedTxes)

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
	if lntest.CommitTypeHasAnchors(c) {
		require.Len(ht, timeoutTxs, 1)
		require.Len(ht, successTxs, 1)
	} else {
		require.Len(ht, timeoutTxs, numInvoices)
		require.Len(ht, successTxs, numInvoices)
	}

	// All mempool transactions should be spending from the commitment
	// transaction.
	ht.AssertAllTxesSpendFrom(txes, closeTxid)

	// Mine a block to confirm the all the transactions, including Carol's
	// commitment tx, anchor tx(optional), and Bob's second-level timeout
	// and success txes.
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation,
	// or to the sweeper for sweeping.
	forceCloseChan := ht.AssertNumPendingForceClose(bob, 1)[0]
	ht.Logf("Bob's timelock on commit=%v, timelock on htlc=%v",
		forceCloseChan.BlocksTilMaturity,
		forceCloseChan.PendingHtlcs[0].BlocksTilMaturity)

	// For this channel, we also check the number of HTLCs and the stage
	// are correct.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, numInvoices*2, 2)

	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// If we then mine additional blocks, Bob can sweep his
		// commitment output.
		ht.MineEmptyBlocks(1)

		// Assert the tx has been offered to the sweeper.
		ht.AssertNumPendingSweeps(bob, 1)

		// Mine one block to trigger the sweep.
		ht.MineEmptyBlocks(1)

		// Find the commitment sweep.
		bobCommitSweep := ht.GetNumTxsFromMempool(1)[0]
		ht.AssertTxSpendFrom(bobCommitSweep, closeTxid)

		// Also ensure it is not spending from any of the HTLC output.
		for _, txin := range bobCommitSweep.TxIn {
			for _, timeoutTx := range timeoutTxs {
				require.NotEqual(ht, *timeoutTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of timeout tx")
			}

			for _, successTx := range successTxs {
				require.NotEqual(ht, *successTx,
					txin.PreviousOutPoint.Hash,
					"found unexpected spend of success tx")
			}
		}
	}

	switch c {
	// Mining one additional block, Bob's second level tx is mature, and he
	// can sweep the output. Before the blocks are mined, we should expect
	// to see Bob's commit sweep in the mempool.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		ht.MineBlocksAndAssertNumTxes(1, 1)

	// Since Bob is the initiator of the Bob-Carol script-enforced leased
	// channel, he incurs an additional CLTV when sweeping outputs back to
	// his wallet. We'll need to mine enough blocks for the timelock to
	// expire to prompt his broadcast.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		resp := bob.RPC.PendingChannels()
		require.Len(ht, resp.PendingForceClosingChannels, 1)
		forceCloseChan := resp.PendingForceClosingChannels[0]
		require.Positive(ht, forceCloseChan.BlocksTilMaturity)
		numBlocks := uint32(forceCloseChan.BlocksTilMaturity)

		// Add debug log.
		height := ht.CurrentHeight()
		bob.AddToLogf("itest: now mine %d blocks at height %d",
			numBlocks, height)
		ht.MineEmptyBlocks(int(numBlocks) - 1)

	default:
		ht.Fatalf("unhandled commitment type %v", c)
	}

	// Make sure Bob's sweeper has received all the sweeping requests.
	ht.AssertNumPendingSweeps(bob, numInvoices*2)

	// Mine one block to trigger the sweeps.
	ht.MineEmptyBlocks(1)

	// For leased channels, Bob's commit output will mature after the above
	// block.
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		ht.AssertNumPendingSweeps(bob, numInvoices*2+1)
	}

	// We now wait for 30 seconds to overcome the flake - there's a block
	// race between contractcourt and sweeper, causing the sweep to be
	// broadcast earlier.
	//
	// TODO(yy): remove this once `blockbeat` is in place.
	numExpected := 1
	err := wait.NoError(func() error {
		mem := ht.GetRawMempool()
		if len(mem) == numExpected {
			return nil
		}

		if len(mem) > 0 {
			numExpected = len(mem)
		}

		return fmt.Errorf("want %d, got %v in mempool: %v", numExpected,
			len(mem), mem)
	}, wait.DefaultTimeout)
	ht.Logf("Checking mempool got: %v", err)

	// Make sure it spends from the second level tx.
	secondLevelSweep := ht.GetNumTxsFromMempool(numExpected)[0]
	bobSweep := secondLevelSweep.TxHash()

	// It should be sweeping all the second-level outputs.
	var secondLvlSpends int
	for _, txin := range secondLevelSweep.TxIn {
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

	// TODO(yy): bring the following check back when `blockbeat` is in
	// place - atm we may have two sweeping transactions in the mempool.
	// require.Equal(ht, 2*numInvoices, secondLvlSpends)

	// When we mine one additional block, that will confirm Bob's second
	// level sweep. Now Bob should have no pending channels anymore, as
	// this just resolved it by the confirmation of the sweep transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, numExpected)[0]
	ht.AssertTxInBlock(block, bobSweep)

	// For leased channels, we need to mine one more block to confirm Bob's
	// commit output sweep.
	//
	// NOTE: we mine this block conditionally, as the commit output may
	// have already been swept one block earlier due to the race in block
	// consumption among subsystems.
	pendingChanResp := bob.RPC.PendingChannels()
	if len(pendingChanResp.PendingForceClosingChannels) != 0 {
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}
	ht.AssertNumPendingForceClose(bob, 0)

	// THe channel with Alice is still open.
	ht.AssertNodeNumChannels(bob, 1)

	// Carol should have no channels left (open nor pending).
	ht.AssertNumPendingForceClose(carol, 0)
	ht.AssertNodeNumChannels(carol, 0)

	// Coop close, no anchors.
	ht.CloseChannel(alice, aliceChanPoint)
}

// createThreeHopNetwork creates a topology of `Alice -> Bob -> Carol`.
func createThreeHopNetwork(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, carolHodl bool, c lnrpc.CommitmentType,
	zeroConf bool) (*lnrpc.ChannelPoint,
	*lnrpc.ChannelPoint, *node.HarnessNode) {

	ht.EnsureConnected(alice, bob)

	// We'll create a new node "carol" and have Bob connect to her.
	// If the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := lntest.NodeArgsForCommitType(c)
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}

	if zeroConf {
		carolFlags = append(
			carolFlags, "--protocol.option-scid-alias",
			"--protocol.zero-conf",
		)
	}
	carol := ht.NewNode("Carol", carolFlags)

	ht.ConnectNodes(bob, carol)

	// Make sure there are enough utxos for anchoring. Because the anchor
	// by itself often doesn't meet the dust limit, a utxo from the wallet
	// needs to be attached as an additional input. This can still lead to
	// a positively-yielding transaction.
	for i := 0; i < 2; i++ {
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, alice)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, bob)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)

		// Mine 1 block to get the above coins confirmed.
		ht.MineBlocksAndAssertNumTxes(1, 3)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	var aliceFundingShim *lnrpc.FundingShim
	var thawHeight uint32
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		minerHeight := ht.CurrentHeight()
		thawHeight = minerHeight + thawHeightDelta
		aliceFundingShim, _ = deriveFundingShim(
			ht, alice, bob, chanAmt, thawHeight, true, c,
		)
	}

	var privateChan bool
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		privateChan = true
	}

	aliceParams := lntest.OpenChannelParams{
		Private:        privateChan,
		Amt:            chanAmt,
		CommitmentType: c,
		FundingShim:    aliceFundingShim,
		ZeroConf:       zeroConf,
	}

	// If the channel type is taproot, then use an explicit channel type to
	// open it.
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		aliceParams.CommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	// We'll create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	var bobFundingShim *lnrpc.FundingShim
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		bobFundingShim, _ = deriveFundingShim(
			ht, bob, carol, chanAmt, thawHeight, true, c,
		)
	}

	// Prepare params for Bob.
	bobParams := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        privateChan,
		CommitmentType: c,
		FundingShim:    bobFundingShim,
		ZeroConf:       zeroConf,
	}

	// If the channel type is taproot, then use an explicit channel type to
	// open it.
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		bobParams.CommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	var (
		acceptStreamBob   rpc.AcceptorClient
		acceptStreamCarol rpc.AcceptorClient
		cancelBob         context.CancelFunc
		cancelCarol       context.CancelFunc
	)

	// If a zero-conf channel is being opened, the nodes are signalling the
	// zero-conf feature bit. Setup a ChannelAcceptor for the fundee.
	if zeroConf {
		acceptStreamBob, cancelBob = bob.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStreamBob)

		acceptStreamCarol, cancelCarol = carol.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStreamCarol)
	}

	// Open channels in batch to save blocks mined.
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: aliceParams},
		{Local: bob, Remote: carol, Param: bobParams},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	aliceChanPoint := resp[0]
	bobChanPoint := resp[1]

	// Make sure alice and carol know each other's channels.
	//
	// We'll only do this though if it wasn't a private channel we opened
	// earlier.
	if !privateChan {
		ht.AssertChannelInGraph(alice, bobChanPoint)
		ht.AssertChannelInGraph(carol, aliceChanPoint)
	} else {
		// Otherwise, we want to wait for all the channels to be shown
		// as active before we proceed.
		ht.AssertChannelExists(alice, aliceChanPoint)
		ht.AssertChannelExists(carol, bobChanPoint)
	}

	// Remove the ChannelAcceptor for Bob and Carol.
	if zeroConf {
		cancelBob()
		cancelCarol()
	}

	return aliceChanPoint, bobChanPoint, carol
}

// testHtlcTimeoutResolverExtractPreimageRemote tests that in the multi-hop
// setting, Alice->Bob->Carol, when Bob's outgoing HTLC is swept by Carol using
// the 2nd level success tx2nd level success tx, Bob's timeout resolver will
// extract the preimage from the sweep tx found in mempool or blocks(for
// neutrino). The 2nd level success tx is broadcast by Carol and spends the
// outpoint on her commit tx.
func testHtlcTimeoutResolverExtractPreimageRemote(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runExtraPreimageFromRemoteCommit)
}

// runExtraPreimageFromRemoteCommit checks that Bob's htlc timeout resolver
// will extract the preimage from the 2nd level success tx broadcast by Carol
// which spends the htlc output on her commitment tx.
func runExtraPreimageFromRemoteCommit(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      100_000,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	eveInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: eveInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the payment sent, Alice should have one outgoing HTLC active.
	ht.AssertOutgoingHTLCActive(alice, aliceChanPoint, payHash[:])

	// Bob should have two HTLCs active. One incoming HTLC from Alice, and
	// one outgoing to Carol.
	ht.AssertIncomingHTLCActive(bob, aliceChanPoint, payHash[:])
	htlc := ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])

	// Carol should have one incoming HTLC from Bob.
	ht.AssertIncomingHTLCActive(carol, bobChanPoint, payHash[:])

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Bob now goes offline so the link between Bob and Carol is broken.
	restartBob := ht.SuspendNode(bob)

	// Carol now settles the invoice, since her link with Bob is broken,
	// Bob won't know the preimage.
	carol.RPC.SettleInvoice(preimage[:])

	// We'll now mine enough blocks to trigger Carol's broadcast of her
	// commitment transaction due to the fact that the HTLC is about to
	// timeout. With the default incoming broadcast delta of 10, this
	// will be the htlc expiry height minus 10.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))
	ht.MineEmptyBlocks(int(numBlocks))

	// Carol's force close transaction should now be found in the mempool.
	// If there are anchors, we also expect Carol's contractcourt to offer
	// the anchors to her sweeper - one from the local commitment and the
	// other from the remote.
	ht.AssertNumPendingSweeps(carol, 2)

	// We now mine a block to confirm Carol's closing transaction, which
	// will trigger her sweeper to sweep her CPFP anchor sweeping.
	ht.MineClosingTx(bobChanPoint)

	// With the closing transaction confirmed, we should expect Carol's
	// HTLC success transaction to be offered to the sweeper along with her
	// anchor output.
	ht.AssertNumPendingSweeps(carol, 2)

	// Mine a block to trigger the sweep, and clean up the anchor sweeping
	// tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertNumTxsInMempool(1)

	// Restart Bob. Once he finishes syncing the channel state, he should
	// notice the force close from Carol.
	require.NoError(ht, restartBob())

	// Get the current height to compute number of blocks to mine to
	// trigger the htlc timeout resolver from Bob.
	height := ht.CurrentHeight()

	// We'll now mine enough blocks to trigger Bob's timeout resolver.
	numBlocks = htlc.ExpirationHeight - height -
		lncfg.DefaultOutgoingBroadcastDelta

	// We should now have Carol's htlc success tx in the mempool.
	numTxesMempool := 1
	ht.AssertNumTxsInMempool(numTxesMempool)

	// For neutrino backend, the timeout resolver needs to extract the
	// preimage from the blocks.
	if ht.IsNeutrinoBackend() {
		// Mine a block to confirm Carol's 2nd level success tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
		numBlocks--
	}

	// Mine empty blocks so Carol's htlc success tx stays in mempool. Once
	// the height is reached, Bob's timeout resolver will resolve the htlc
	// by extracing the preimage from the mempool.
	ht.MineEmptyBlocks(int(numBlocks))

	// Finally, check that the Alice's payment is marked as succeeded as
	// Bob has settled the htlc using the preimage extracted from Carol's
	// 2nd level success tx.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	switch c {
	// For anchor channel type, we should expect to see Bob's commit output
	// and his anchor output be swept in a single tx in the mempool.
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		numTxesMempool++

	// For script-enforced leased channel, Bob's anchor sweep tx won't
	// happen as it's not used for CPFP, hence no wallet utxo is used so
	// it'll be uneconomical.
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
	}

	// For neutrino backend, Carol's second-stage sweep should be offered
	// to her sweeper.
	if ht.IsNeutrinoBackend() {
		ht.AssertNumPendingSweeps(carol, 1)

		// Mine a block to trigger the sweep.
		ht.MineEmptyBlocks(1)
	}

	// Mine a block to clean the mempool.
	ht.MineBlocksAndAssertNumTxes(1, numTxesMempool)

	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}

// testHtlcTimeoutResolverExtractPreimage tests that in the multi-hop setting,
// Alice->Bob->Carol, when Bob's outgoing HTLC is swept by Carol using the
// direct preimage spend, Bob's timeout resolver will extract the preimage from
// the sweep tx found in mempool or blocks(for neutrino). The direct spend tx
// is broadcast by Carol and spends the outpoint on Bob's commit tx.
func testHtlcTimeoutResolverExtractPreimageLocal(ht *lntest.HarnessTest) {
	runMultiHopHtlcClaimTest(ht, runExtraPreimageFromLocalCommit)
}

// runExtraPreimageFromLocalCommit checks that Bob's htlc timeout resolver will
// extract the preimage from the direct spend broadcast by Carol which spends
// the htlc output on Bob's commitment tx.
func runExtraPreimageFromLocalCommit(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, c lnrpc.CommitmentType, zeroConf bool) {

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopNetwork(
		ht, alice, bob, false, c, zeroConf,
	)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if c == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, zeroConf)
	}

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      100_000,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the payment sent, Alice should have one outgoing HTLC active.
	ht.AssertOutgoingHTLCActive(alice, aliceChanPoint, payHash[:])

	// Bob should have two HTLCs active. One incoming HTLC from Alice, and
	// one outgoing to Carol.
	ht.AssertIncomingHTLCActive(bob, aliceChanPoint, payHash[:])
	htlc := ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])

	// Carol should have one incoming HTLC from Bob.
	ht.AssertIncomingHTLCActive(carol, bobChanPoint, payHash[:])

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Bob now goes offline so the link between Bob and Carol is broken.
	restartBob := ht.SuspendNode(bob)

	// Carol now settles the invoice, since her link with Bob is broken,
	// Bob won't know the preimage.
	carol.RPC.SettleInvoice(preimage[:])

	// Stop Carol so it's easier to check the mempool's state since she
	// will broadcast the anchor sweeping once Bob force closes.
	restartCarol := ht.SuspendNode(carol)

	// Restart Bob to force close the channel.
	require.NoError(ht, restartBob())

	// Bob force closes the channel, which gets his commitment tx into the
	// mempool.
	ht.CloseChannelAssertPending(bob, bobChanPoint, true)

	// Bob should now has offered his anchors to his sweeper - both local
	// and remote versions.
	ht.AssertNumPendingSweeps(bob, 2)

	// Mine Bob's force close tx.
	closeTx := ht.MineClosingTx(bobChanPoint)

	// Mine Bob's anchor sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	blocksMined := 1

	// We'll now mine enough blocks to trigger Carol's sweeping of the htlc
	// via the direct spend. With the default incoming broadcast delta of
	// 10, this will be the htlc expiry height minus 10.
	//
	// NOTE: we need to mine 1 fewer block as we've already mined one to
	// confirm Bob's force close tx.
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta - 1,
	))

	// If this is a nont script-enforced channel, Bob will be able to sweep
	// his commit output after 4 blocks.
	if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Mine 3 blocks so the output will be offered to the sweeper.
		ht.MineEmptyBlocks(defaultCSV - blocksMined - 1)

		// Assert the commit output has been offered to the sweeper.
		ht.AssertNumPendingSweeps(bob, 1)

		// Mine a block to trigger the sweep.
		ht.MineEmptyBlocks(1)
		blocksMined = defaultCSV
	}

	// Mine empty blocks so it's easier to check Bob's sweeping txes below.
	ht.MineEmptyBlocks(int(numBlocks) - blocksMined)

	// With the above blocks mined, we should expect Carol's to offer the
	// htlc output on Bob's commitment to the sweeper.
	//
	// TODO(yy): it's not offered to the sweeper yet, instead, the utxo
	// nursery is creating and broadcasting the sweep tx - we should unify
	// this behavior and offer it to the sweeper.
	// ht.AssertNumPendingSweeps(carol, 1)

	// Increase the fee rate used by the sweeper so Carol's direct spend tx
	// won't be replaced by Bob's timeout tx.
	ht.SetFeeEstimate(30000)

	// Restart Carol to sweep the htlc output.
	require.NoError(ht, restartCarol())

	ht.AssertNumPendingSweeps(carol, 2)
	ht.MineEmptyBlocks(1)

	// Construct the htlc output on Bob's commitment tx, and decide its
	// index based on the commit type below.
	htlcOutpoint := wire.OutPoint{Hash: closeTx.TxHash()}

	// Check the current mempool state and we should see,
	// - Carol's direct spend tx.
	// - Bob's local output sweep tx, if this is NOT script enforced lease.
	// - Carol's anchor sweep tx cannot be broadcast as it's uneconomical.
	switch c {
	case lnrpc.CommitmentType_ANCHORS, lnrpc.CommitmentType_SIMPLE_TAPROOT:
		htlcOutpoint.Index = 2
		ht.AssertNumTxsInMempool(2)

	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		htlcOutpoint.Index = 2
		ht.AssertNumTxsInMempool(1)
	}

	// Get the current height to compute number of blocks to mine to
	// trigger the timeout resolver from Bob.
	height := ht.CurrentHeight()

	// We'll now mine enough blocks to trigger Bob's htlc timeout resolver
	// to act. Once his timeout resolver starts, it will extract the
	// preimage from Carol's direct spend tx found in the mempool.
	numBlocks = htlc.ExpirationHeight - height -
		lncfg.DefaultOutgoingBroadcastDelta

	// Decrease the fee rate used by the sweeper so Bob's timeout tx will
	// not replace Carol's direct spend tx.
	ht.SetFeeEstimate(1000)

	// Mine empty blocks so Carol's direct spend tx stays in mempool. Once
	// the height is reached, Bob's timeout resolver will resolve the htlc
	// by extracing the preimage from the mempool.
	ht.MineEmptyBlocks(int(numBlocks))

	// For neutrino backend, the timeout resolver needs to extract the
	// preimage from the blocks.
	if ht.IsNeutrinoBackend() {
		// Make sure the direct spend tx is still in the mempool.
		ht.AssertOutpointInMempool(htlcOutpoint)

		// Mine a block to confirm two txns,
		// - Carol's direct spend tx.
		// - Bob's to_local output sweep tx.
		if c != lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
			ht.MineBlocksAndAssertNumTxes(1, 2)
		} else {
			ht.MineBlocksAndAssertNumTxes(1, 1)
		}
	}

	// Finally, check that the Alice's payment is marked as succeeded as
	// Bob has settled the htlc using the preimage extracted from Carol's
	// direct spend tx.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}
