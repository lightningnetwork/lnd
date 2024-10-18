package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

const chanAmt = 1000000

var leasedType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

// multiHopForceCloseTestCases defines a set of tests that focuses on the
// behavior of the force close in a multi-hop scenario.
var multiHopForceCloseTestCases = []*lntest.TestCase{
	{
		Name:     "multihop local claim outgoing htlc anchor",
		TestFunc: testLocalClaimOutgoingHTLCAnchor,
	},
	{
		Name:     "multihop local claim outgoing htlc simple taproot",
		TestFunc: testLocalClaimOutgoingHTLCSimpleTaproot,
	},
	{
		Name:     "multihop local claim outgoing htlc leased",
		TestFunc: testLocalClaimOutgoingHTLCLeased,
	},
	{
		Name:     "multihop receiver preimage claim anchor",
		TestFunc: testMultiHopReceiverPreimageClaimAnchor,
	},
	{
		Name:     "multihop receiver preimage claim simple taproot",
		TestFunc: testMultiHopReceiverPreimageClaimSimpleTaproot,
	},
	{
		Name:     "multihop receiver preimage claim leased",
		TestFunc: testMultiHopReceiverPreimageClaimLeased,
	},
}

// testLocalClaimOutgoingHTLCAnchor tests `runLocalClaimOutgoingHTLC` with
// anchor channel.
func testLocalClaimOutgoingHTLCAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
}

// testLocalClaimOutgoingHTLCSimpleTaproot tests `runLocalClaimOutgoingHTLC`
// with simple taproot channel.
func testLocalClaimOutgoingHTLCSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf simple taproot channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: c,
			Private:        true,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgSimpleTaproot
		cfg = append(cfg, node.CfgZeroConf...)
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
}

// testLocalClaimOutgoingHTLCLeased tests `runLocalClaimOutgoingHTLC` with
// script enforced lease channel.
func testLocalClaimOutgoingHTLCLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: leasedType,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgLeased
		cfg = append(cfg, node.CfgZeroConf...)
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalClaimOutgoingHTLC(st, cfgs, openChannelParams)
	})
}

// runLocalClaimOutgoingHTLC tests that in a multi-hop scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it using the HTLC timeout transaction. Any dust HTLC's should be immediately
// canceled backwards. Once the timeout has been reached, then we should sweep
// it on-chain, and cancel the HTLC backwards.
func runLocalClaimOutgoingHTLC(ht *lntest.HarnessTest,
	cfgs [][]string, params lntest.OpenChannelParams) {

	// Create a three hop network: Alice -> Bob -> Carol.
	_, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// For neutrino backend, we need to fund one more UTXO for Bob so he
	// can sweep his outputs.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt = btcutil.Amount(100)
		htlcAmt     = btcutil.Amount(300_000)
	)

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := ht.Random32Bytes()
	payHash := ht.Random32Bytes()

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if params.CommitmentType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, params.ZeroConf)
	}

	alice.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(dustHtlcAmt),
		PaymentHash:    dustPayHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	})

	alice.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	})

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	ht.AssertActiveHtlcs(alice, dustPayHash, payHash)
	ht.AssertActiveHtlcs(bob, dustPayHash, payHash)
	ht.AssertActiveHtlcs(carol, dustPayHash, payHash)

	// We'll now mine enough blocks to trigger Bob's force close the
	// channel Bob=>Carol due to the fact that the HTLC is about to
	// timeout.  With the default outgoing broadcast delta of zero, this
	// will be the same height as the htlc expiry height.
	numBlocks := padCLTV(
		uint32(finalCltvDelta - lncfg.DefaultOutgoingBroadcastDelta),
	)
	ht.MineBlocks(int(numBlocks))

	// Bob's force close tx should have the following outputs,
	// 1. anchor output.
	// 2. to_local output, which is CSV locked.
	// 3. outgoing HTLC output, which has expired.
	//
	// Bob's anchor output should be offered to his sweeper since Bob has
	// time-sensitive HTLCs - we expect both anchors to be offered, while
	// the sweeping of the remote anchor will be marked as failed due to
	// `testmempoolaccept` check.
	//
	// For neutrino backend, there's no way to know the sweeping of the
	// remote anchor is failed, so Bob still sees two pending sweeps.
	if ht.IsNeutrinoBackend() {
		ht.AssertNumPendingSweeps(bob, 2)
	} else {
		ht.AssertNumPendingSweeps(bob, 1)
	}

	// We expect to see tow txns in the mempool,
	// 1. Bob's force close tx.
	// 2. Bob's anchor sweep tx.
	ht.AssertNumTxsInMempool(2)

	// Mine a block to confirm the closing tx and the anchor sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// At this point, Bob should have canceled backwards the dust HTLC that
	// we sent earlier. This means Alice should now only have a single HTLC
	// on her channel.
	ht.AssertActiveHtlcs(alice, payHash)

	// With the closing transaction confirmed, we should expect Bob's HTLC
	// timeout transaction to be offered to the sweeper due to the expiry
	// being reached. we also expect Carol's anchor sweeps.
	ht.AssertNumPendingSweeps(bob, 1)
	ht.AssertNumPendingSweeps(carol, 1)

	// Bob's sweeper should sweep his outgoing HTLC immediately since it's
	// expired. His to_local output cannot be swept due to the CSV lock.
	// Carol's anchor sweep should be failed due to output being dust.
	ht.AssertNumTxsInMempool(1)

	// Mine a block to confirm Bob's outgoing HTLC sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// With Bob's HTLC timeout transaction confirmed, there should be no
	// active HTLC's on the commitment transaction from Alice -> Bob.
	ht.AssertNumActiveHtlcs(alice, 0)

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is ready to be swept once the timelock is up.
	resp := ht.AssertNumPendingForceClose(bob, 1)[0]
	require.NotZero(ht, resp.LimboBalance)
	require.Positive(ht, resp.BlocksTilMaturity)
	require.Equal(ht, 1, len(resp.PendingHtlcs))
	require.Equal(ht, uint32(2), resp.PendingHtlcs[0].Stage)

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	if params.CommitmentType == leasedType {
		// Since Bob is the initiator of the script-enforced leased
		// channel between him and Carol, he will incur an additional
		// CLTV on top of the usual CSV delay on any outputs that he
		// can sweep back to his wallet.
		//
		// We now mine enough blocks so the CLTV lock expires, which
		// will trigger the sweep of the to_local and outgoing HTLC
		// outputs.
		ht.MineBlocks(int(resp.BlocksTilMaturity))

		// Check that Bob has a pending sweeping tx which sweeps his
		// to_local and outgoing HTLC outputs.
		ht.AssertNumPendingSweeps(bob, 2)

		// Mine a block to confirm the sweeping tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	} else {
		// Since Bob force closed the channel between him and Carol, he
		// will incur the usual CSV delay on any outputs that he can
		// sweep back to his wallet. We'll subtract one block from our
		// current maturity period to assert on the mempool.
		ht.MineBlocks(int(resp.BlocksTilMaturity - 1))

		// Check that Bob has a pending sweeping tx which sweeps his
		// to_local output.
		ht.AssertNumPendingSweeps(bob, 1)

		// Mine a block to confirm the to_local sweeping tx, which also
		// triggers the sweeping of the second stage HTLC output.
		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Bob's sweeper should now broadcast his second layer sweep
		// due to the CSV on the HTLC timeout output.
		ht.AssertNumTxsInMempool(1)

		// Next, we'll mine a final block that should confirm the
		// sweeping transactions left.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	ht.AssertNumPendingForceClose(bob, 0)
}

// testMultiHopReceiverPreimageClaimAnchor tests
// `runMultiHopReceiverPreimageClaim` with anchor channels.
func testMultiHopReceiverPreimageClaimAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
}

// testMultiHopReceiverPreimageClaimSimpleTaproot tests
// `runMultiHopReceiverPreimageClaim` with simple taproot channels.
func testMultiHopReceiverPreimageClaimSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf simple taproot channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: c,
			Private:        true,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgSimpleTaproot
		cfg = append(cfg, node.CfgZeroConf...)
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
}

// testMultiHopReceiverPreimageClaimLeased tests
// `runMultiHopReceiverPreimageClaim` with script enforce lease channels.
func testMultiHopReceiverPreimageClaimLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
	if !success {
		return
	}

	ht.Run("zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// zero-conf anchor channels.
		//
		// Prepare params.
		openChannelParams := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: leasedType,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgLeased
		cfg = append(cfg, node.CfgZeroConf...)
		cfgs := [][]string{cfg, cfg, cfg}

		runMultiHopReceiverPreimageClaim(st, cfgs, openChannelParams)
	})
}

// runMultiHopReceiverClaim tests that in the multi-hop setting, if the
// receiver of an HTLC knows the preimage, but wasn't able to settle the HTLC
// off-chain, then it goes on chain to claim the HTLC uing the HTLC success
// transaction. In this scenario, the node that sent the outgoing HTLC should
// extract the preimage from the sweep transaction, and finish settling the
// HTLC backwards into the route.
func runMultiHopReceiverPreimageClaim(ht *lntest.HarnessTest,
	cfgs [][]string, params lntest.OpenChannelParams) {

	// Set the min relay feerate to be 10 sat/vbyte so the non-CPFP anchor
	// is never swept.
	//
	// TODO(yy): delete this line once the normal anchor sweeping is
	// removed.
	ht.SetMinRelayFeerate(10_000)

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	bobChanPoint := chanPoints[1]

	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// For neutrino backend, we need to one more UTXO for Carol so she can
	// sweep her outputs.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// Fund Carol one UTXO so she can sweep outputs.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if params.CommitmentType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, params.ZeroConf)
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

	// Wait for Carol to mark invoice as accepted. There is a small gap to
	// bridge between adding the htlc to the channel and executing the exit
	// hop logic.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Stop Bob so he won't be able to settle the incoming htlc.
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
	numBlocks := padCLTV(uint32(
		invoiceReq.CltvExpiry - lncfg.DefaultIncomingBroadcastDelta,
	))

	// Now we'll mine enough blocks to prompt Carol to actually go to the
	// chain in order to sweep her HTLC since the value is high enough.
	ht.MineBlocks(int(numBlocks))

	// Carol's force close tx should have the following outputs,
	// 1. anchor output.
	// 2. to_local output, which is CSV locked.
	// 3. incoming HTLC output, which she has the preimage to settle.
	//
	// Carol's anchor output should be offered to her sweeper since she has
	// time-sensitive HTLCs - we expect both anchors to be offered, while
	// the sweeping of the remote anchor will be marked as failed due to
	// `testmempoolaccept` check.
	//
	// For neutrino backend, there's no way to know the sweeping of the
	// remote anchor is failed, so Carol still sees two pending sweeps.
	if ht.IsNeutrinoBackend() {
		ht.AssertNumPendingSweeps(carol, 2)
	} else {
		ht.AssertNumPendingSweeps(carol, 1)
	}

	// We expect to see tow txns in the mempool,
	// 1. Carol's force close tx.
	// 2. Carol's anchor sweep tx.
	ht.AssertNumTxsInMempool(2)

	// Mine a block to confirm the closing tx and the anchor sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	ht.Log("Current height", ht.CurrentHeight())

	// After the force close tx is mined, Carol should offer her second
	// level HTLC tx to the sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Once Bob is online, he should notice Carol's second level tx in the
	// mempool, he will extract the preimage and settle the HTLC back
	// off-chain. He will also try to sweep his anchor and to_local
	// outputs, with the anchor output being skipped due to it being
	// uneconomical.
	if params.CommitmentType == leasedType {
		// For leased channels, Bob cannot sweep his to_local output
		// yet since it's timelocked, so we only see his anchor input.
		ht.AssertNumPendingSweeps(bob, 1)
	} else {
		// For non-leased channels, Bob should have two pending sweeps,
		// 1. to_local output.
		// 2. anchor output, tho it won't be swept due to it being
		//    uneconomical.
		ht.AssertNumPendingSweeps(bob, 2)
	}

	// Mine an empty block the for neutrino backend. We need this step to
	// trigger Bob's chain watcher to detect the force close tx. Deep down,
	// this happens because the notification system for neutrino is very
	// different from others. Specifically, when a block contains the force
	// close tx is notified, these two calls,
	// - RegisterBlockEpochNtfn, will notify the block first.
	// - RegisterSpendNtfn, will wait for the neutrino notifier to sync to
	//   the block, then perform a GetUtxo, which, by the time the spend
	//   details are sent, the blockbeat is considered processed in Bob's
	//   chain watcher.
	//
	// TODO(yy): refactor txNotifier to fix the above issue.
	if ht.IsNeutrinoBackend() {
		ht.MineEmptyBlocks(1)
	}

	if params.CommitmentType == leasedType {
		// We expect to see 1 txns in the mempool,
		// - Carol's second level HTLC sweep tx.
		// We now mine a block to confirm it.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	} else {
		// We expect to see 2 txns in the mempool,
		// - Bob's to_local sweep tx.
		// - Carol's second level HTLC sweep tx.
		// We now mine a block to confirm the sweeping txns.
		ht.MineBlocksAndAssertNumTxes(1, 2)
	}

	// Once the second-level transaction confirmed, Bob should have
	// extracted the preimage from the chain, and sent it back to Alice,
	// clearing the HTLC off-chain.
	ht.AssertNumActiveHtlcs(alice, 0)

	// Check that the Alice's payment is correctly marked succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// Carol's pending channel report should now show two outputs under
	// limbo: her commitment output, as well as the second-layer claim
	// output, and the pending HTLC should also now be in stage 2.
	ht.AssertNumHTLCsAndStage(carol, bobChanPoint, 1, 2)

	// If we mine 4 additional blocks, then Carol can sweep the second
	// level HTLC output once the CSV expires.
	ht.MineBlocks(defaultCSV - 1)

	// Assert Carol has the pending HTLC sweep.
	ht.AssertNumPendingSweeps(carol, 1)

	// We should have a new transaction in the mempool.
	ht.AssertNumTxsInMempool(1)

	// Finally, if we mine an additional block to confirm Carol's second
	// level success transaction. Carol should not show a pending channel
	// in her report afterwards.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertNumPendingForceClose(carol, 0)

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	ht.AssertInvoiceSettled(carol, carolInvoice.PaymentAddr)

	// For leased channels, Bob still has his commit output to sweep to
	// since he incurred an additional CLTV from being the channel
	// initiator.
	if params.CommitmentType == leasedType {
		resp := ht.AssertNumPendingForceClose(bob, 1)[0]
		require.Positive(ht, resp.LimboBalance)
		require.Positive(ht, resp.BlocksTilMaturity)

		// Mine enough blocks for Bob's commit output's CLTV to expire
		// and sweep it.
		ht.MineBlocks(int(resp.BlocksTilMaturity))

		// Bob should have two pending inputs to be swept, the commit
		// output and the anchor output.
		ht.AssertNumPendingSweeps(bob, 2)

		// Mine a block to confirm the commit output sweep.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// Assert Bob also sees the channel as closed.
	ht.AssertNumPendingForceClose(bob, 0)
}
