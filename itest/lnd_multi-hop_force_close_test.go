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

const (
	chanAmt    = 1_000_000
	invoiceAmt = 100_000
	htlcAmt    = btcutil.Amount(300_000)

	incomingBroadcastDelta = lncfg.DefaultIncomingBroadcastDelta
)

var leasedType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

// multiHopForceCloseTestCases defines a set of tests that focuses on the
// behavior of the force close in a multi-hop scenario.
//
//nolint:ll
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
	{
		Name:     "multihop local force close before timeout anchor",
		TestFunc: testLocalForceCloseBeforeTimeoutAnchor,
	},
	{
		Name:     "multihop local force close before timeout simple taproot",
		TestFunc: testLocalForceCloseBeforeTimeoutSimpleTaproot,
	},
	{
		Name:     "multihop local force close before timeout leased",
		TestFunc: testLocalForceCloseBeforeTimeoutLeased,
	},
	{
		Name:     "multihop remote force close before timeout anchor",
		TestFunc: testRemoteForceCloseBeforeTimeoutAnchor,
	},
	{
		Name:     "multihop remote force close before timeout simple taproot",
		TestFunc: testRemoteForceCloseBeforeTimeoutSimpleTaproot,
	},
	{
		Name:     "multihop remote force close before timeout leased",
		TestFunc: testRemoteForceCloseBeforeTimeoutLeased,
	},
	{
		Name:     "multihop local claim incoming htlc anchor",
		TestFunc: testLocalClaimIncomingHTLCAnchor,
	},
	{
		Name:     "multihop local claim incoming htlc simple taproot",
		TestFunc: testLocalClaimIncomingHTLCSimpleTaproot,
	},
	{
		Name:     "multihop local claim incoming htlc leased",
		TestFunc: testLocalClaimIncomingHTLCLeased,
	},
	{
		Name:     "multihop local preimage claim anchor",
		TestFunc: testLocalPreimageClaimAnchor,
	},
	{
		Name:     "multihop local preimage claim simple taproot",
		TestFunc: testLocalPreimageClaimSimpleTaproot,
	},
	{
		Name:     "multihop local preimage claim leased",
		TestFunc: testLocalPreimageClaimLeased,
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

	// Bob should have enough wallet UTXOs here to sweep the HTLC in the
	// end of this test. However, due to a known issue, Bob's wallet may
	// report there's no UTXO available. For details,
	// - https://github.com/lightningnetwork/lnd/issues/8786
	//
	// TODO(yy): remove this step once the issue is resolved.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const dustHtlcAmt = btcutil.Amount(100)

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
		invoiceReq.CltvExpiry - incomingBroadcastDelta,
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

// testLocalForceCloseBeforeTimeoutAnchor tests
// `runLocalForceCloseBeforeHtlcTimeout` with anchor channel.
func testLocalForceCloseBeforeTimeoutAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// testLocalForceCloseBeforeTimeoutSimpleTaproot tests
// `runLocalForceCloseBeforeHtlcTimeout` with simple taproot channel.
func testLocalForceCloseBeforeTimeoutSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// testLocalForceCloseBeforeTimeoutLeased tests
// `runLocalForceCloseBeforeHtlcTimeout` with script enforced lease channel.
func testLocalForceCloseBeforeTimeoutLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runLocalForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// runLocalForceCloseBeforeHtlcTimeout tests that in a multi-hop HTLC scenario,
// if the node that extended the HTLC to the final node closes their commitment
// on-chain early, then it eventually recognizes this HTLC as one that's timed
// out. At this point, the node should timeout the HTLC using the HTLC timeout
// transaction, then cancel it backwards as normal.
func runLocalForceCloseBeforeHtlcTimeout(ht *lntest.HarnessTest,
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

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if params.CommitmentType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, params.ZeroConf)
	}

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := ht.Random32Bytes()
	req := &routerrpc.SendPaymentRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
		RouteHints:     routeHints,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	ht.AssertActiveHtlcs(alice, payHash)
	ht.AssertActiveHtlcs(bob, payHash)
	ht.AssertActiveHtlcs(carol, payHash)

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	stream, _ := ht.CloseChannelAssertPending(bob, bobChanPoint, true)
	ht.AssertStreamChannelForceClosed(bob, bobChanPoint, true, stream)

	// Bob's force close tx should have the following outputs,
	// 1. anchor output.
	// 2. to_local output, which is CSV locked.
	// 3. outgoing HTLC output, which hasn't expired yet.
	//
	// The channel close has anchors, we should expect to see both Bob and
	// Carol has a pending sweep request for the anchor sweep.
	ht.AssertNumPendingSweeps(carol, 1)
	anchorSweep := ht.AssertNumPendingSweeps(bob, 1)[0]

	// We expcet Bob's anchor sweep to be a non-CPFP anchor sweep now.
	// Although he has time-sensitive outputs, which means initially his
	// anchor output was used for CPFP, this anchor will be replaced by a
	// new anchor sweeping request once his force close tx is confirmed in
	// the above block. The timeline goes as follows:
	// 1. At block 447, Bob force closes his channel with Carol, which
	//    caused the channel arbitartor to create a CPFP anchor sweep.
	// 2. This force close tx was mined in AssertStreamChannelForceClosed,
	//    and we are now in block 448.
	// 3. Since the blockbeat is processed via the chain [ChainArbitrator
	//    -> chainWatcher -> channelArbitrator -> Sweeper -> TxPublisher],
	//    when it reaches `chainWatcher`, Bob will detect the confirmed
	//    force close tx and notifies `channelArbitrator`. In response,
	//    `channelArbitrator` will advance to `StateContractClosed`, in
	//    which it will prepare an anchor resolution that's non-CPFP, send
	//    it to the sweeper to replace the CPFP anchor sweep.
	// 4. By the time block 448 reaches `Sweeper`, the old CPFP anchor
	//    sweep has already been replaced with the new non-CPFP anchor
	//    sweep.
	require.EqualValues(ht, 330, anchorSweep.Budget, "expected 330 sat "+
		"budget, got %v", anchorSweep.Budget)

	// Before the HTLC times out, we'll need to assert that Bob broadcasts
	// a sweep tx for his commit output. Note that if the channel has a
	// script-enforced lease, then Bob will have to wait for an additional
	// CLTV before sweeping it.
	if params.CommitmentType != leasedType {
		// The sweeping tx is broadcast on the block CSV-1 so mine one
		// block less than defaultCSV in order to perform mempool
		// assertions.
		ht.MineBlocks(int(defaultCSV - 1))

		// Mine a block to confirm Bob's to_local sweep.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the sweeper.
	resp := ht.AssertNumPendingForceClose(bob, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)

	// Bob should have two pending sweep requests,
	// 1. the anchor sweep.
	// 2. the outgoing HTLC sweep.
	ht.AssertNumPendingSweeps(bob, 2)

	// Bob's outgoing HTLC sweep should be broadcast now. Mine a block to
	// confirm it.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// With the second layer timeout tx confirmed, Bob should have canceled
	// backwards the HTLC that Carol sent.
	ht.AssertNumActiveHtlcs(bob, 0)

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// Get the expiry height of the CSV-locked HTLC.
	resp = ht.AssertNumPendingForceClose(bob, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))
	pendingHtlc := resp.PendingHtlcs[0]
	require.Positive(ht, pendingHtlc.BlocksTilMaturity)

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	// Mine enough blocks for the HTLC to expire.
	ht.MineBlocks(int(pendingHtlc.BlocksTilMaturity))

	// Based on this is a leased channel or not, Bob may still need to
	// sweep his to_local output.
	if params.CommitmentType == leasedType {
		// Bob should have three pending sweep requests,
		// 1. the anchor sweep.
		// 2. the second-level HTLC sweep.
		// 3. the to_local output sweep, which is CSV+CLTV locked, is
		//    now mature.
		//
		// The test is setup such that the to_local and the
		// second-level HTLC sweeps share the same deadline, which
		// means they will be swept in the same tx.
		ht.AssertNumPendingSweeps(bob, 3)
	} else {
		// Bob should have two pending sweeps,
		// 1. the anchor sweep.
		// 2. the second-level HTLC sweep.
		ht.AssertNumPendingSweeps(bob, 2)
	}

	// Now that the CSV timelock has expired, mine a block to confirm the
	// sweep.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// At this point, Bob should no longer show any channels as pending
	// close.
	ht.AssertNumPendingForceClose(bob, 0)
}

// testRemoteForceCloseBeforeTimeoutAnchor tests
// `runRemoteForceCloseBeforeHtlcTimeout` with anchor channel.
func testRemoteForceCloseBeforeTimeoutAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// testRemoteForceCloseBeforeTimeoutSimpleTaproot tests
// `runLocalForceCloseBeforeHtlcTimeout` with simple taproot channel.
func testRemoteForceCloseBeforeTimeoutSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// testRemoteForceCloseBeforeTimeoutLeased tests
// `runRemoteForceCloseBeforeHtlcTimeout` with script enforced lease channel.
func testRemoteForceCloseBeforeTimeoutLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgCarol := append([]string{"--hodl.exit-settle"}, cfg...)
		cfgs := [][]string{cfg, cfg, cfgCarol}

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runRemoteForceCloseBeforeHtlcTimeout(st, cfgs, params)
	})
}

// runRemoteForceCloseBeforeHtlcTimeout tests that if we extend a multi-hop
// HTLC, and the final destination of the HTLC force closes the channel, then
// we properly timeout the HTLC directly on *their* commitment transaction once
// the timeout has expired. Once we sweep the transaction, we should also
// cancel back the initial HTLC.
func runRemoteForceCloseBeforeHtlcTimeout(ht *lntest.HarnessTest,
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

	// If this is a taproot channel, then we'll need to make some manual
	// route hints so Alice can actually find a route.
	var routeHints []*lnrpc.RouteHint
	if params.CommitmentType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		routeHints = makeRouteHints(bob, carol, params.ZeroConf)
	}

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(htlcAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
		RouteHints: routeHints,
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(payHash[:])

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])
	ht.AssertActiveHtlcs(carol, payHash[:])

	// At this point, we'll now instruct Carol to force close the tx. This
	// will let us exercise that Bob is able to sweep the expired HTLC on
	// Carol's version of the commitment tx.
	closeStream, _ := ht.CloseChannelAssertPending(
		carol, bobChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP because
	// channel arbitrator thinks Carol doesn't have preimage for her
	// incoming HTLC on the commitment transaction Bob->Carol. Although
	// Carol created this invoice, because it's a hold invoice, the
	// preimage won't be generated automatically.
	ht.AssertStreamChannelForceClosed(
		carol, bobChanPoint, true, closeStream,
	)

	// At this point, Bob should have a pending force close channel as
	// Carol has gone directly to chain.
	ht.AssertNumPendingForceClose(bob, 1)

	// Carol will offer her anchor to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Bob should offered the anchor output to his sweeper.
	if params.CommitmentType == leasedType {
		// For script enforced lease channels, Bob can sweep his anchor
		// output immediately although it will be skipped due to it
		// being uneconomical. His to_local output is CLTV locked so it
		// cannot be swept yet.
		ht.AssertNumPendingSweeps(bob, 1)
	} else {
		// For non-leased channels, Bob can sweep his commit and anchor
		// outputs immediately.
		ht.AssertNumPendingSweeps(bob, 2)

		// We expect to see only one sweeping tx to be published from
		// Bob, which sweeps his to_local output. His anchor output
		// won't be swept due it being uneconomical. For Carol, since
		// her anchor is not used for CPFP, it'd be also uneconomical
		// to sweep so it will fail.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// Next, we'll mine enough blocks for the HTLC to expire. At this
	// point, Bob should hand off the output to his sweeper, which will
	// broadcast a sweep transaction.
	resp := ht.AssertNumPendingForceClose(bob, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// If we check Bob's pending channel report, it should show that he has
	// a single HTLC that's now in the second stage, as it skipped the
	// initial first stage since this is a direct HTLC.
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 2)

	// Bob should have two pending sweep requests,
	// 1. the uneconomical anchor sweep.
	// 2. the direct timeout sweep.
	ht.AssertNumPendingSweeps(bob, 2)

	// Bob's sweeping tx should now be found in the mempool.
	sweepTx := ht.AssertNumTxsInMempool(1)[0]

	// If we mine an additional block, then this should confirm Bob's tx
	// which sweeps the direct HTLC output.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, sweepTx)

	// Now that the sweeping tx has been confirmed, Bob should cancel back
	// that HTLC. As a result, Alice should not know of any active HTLC's.
	ht.AssertNumActiveHtlcs(alice, 0)

	// For script enforced lease channels, Bob still need to wait for the
	// CLTV lock to expire before he can sweep his to_local output.
	if params.CommitmentType == leasedType {
		// Get the remaining blocks to mine.
		resp = ht.AssertNumPendingForceClose(bob, 1)[0]
		ht.MineBlocks(int(resp.BlocksTilMaturity))

		// Assert the commit output has been offered to the sweeper.
		// Bob should have two pending sweep requests - one for the
		// commit output and one for the anchor output.
		ht.AssertNumPendingSweeps(bob, 2)

		// Mine the to_local sweep tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// Now we'll check Bob's pending channel report. Since this was Carol's
	// commitment, he doesn't have to wait for any CSV delays, but he may
	// still need to wait for a CLTV on his commit output to expire
	// depending on the commitment type.
	ht.AssertNumPendingForceClose(bob, 0)

	// While we're here, we assert that our expired invoice's state is
	// correctly updated, and can no longer be settled.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_CANCELED)
}

// testLocalClaimIncomingHTLCAnchor tests `runLocalClaimIncomingHTLC` with
// anchor channel.
func testLocalClaimIncomingHTLCAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalClaimIncomingHTLC(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalClaimIncomingHTLC(st, cfgs, params)
	})
}

// testLocalClaimIncomingHTLCSimpleTaproot tests `runLocalClaimIncomingHTLC`
// with simple taproot channel.
func testLocalClaimIncomingHTLCSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalClaimIncomingHTLC(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runLocalClaimIncomingHTLC(st, cfgs, params)
	})
}

// runLocalClaimIncomingHTLC tests that in a multi-hop HTLC scenario, if we
// force close a channel with an incoming HTLC, and later find out the preimage
// via the witness beacon, we properly settle the HTLC on-chain using the HTLC
// success transaction in order to ensure we don't lose any funds.
func runLocalClaimIncomingHTLC(ht *lntest.HarnessTest,
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
	aliceChanPoint := chanPoints[0]

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
	preimage := ht.RandomPreimage()
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

	// At this point, Bob decides that he wants to exit the channel
	// Alice=>Bob immediately, so he force closes his commitment tx.
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP as there's no
	// deadline pressure for Bob on the channel Alice->Bob at the moment.
	// For Bob's local commitment tx, there's only one incoming HTLC which
	// he doesn't have the preimage yet.
	hasAnchorSweep := false
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchorSweep, closeStream,
	)

	// Alice will offer her to_local and anchor outputs to her sweeper.
	ht.AssertNumPendingSweeps(alice, 2)

	// Bob will offer his anchor to his sweeper.
	ht.AssertNumPendingSweeps(bob, 1)

	// Assert the expected num of txns are found in the mempool.
	//
	// We expect to see only one sweeping tx to be published from Alice,
	// which sweeps her to_local output (which is to to_remote on Bob's
	// commit tx). Her anchor output won't be swept as it's uneconomical.
	// For Bob, since his anchor is not used for CPFP, it'd be uneconomical
	// to sweep so it will fail.
	ht.AssertNumTxsInMempool(1)

	// Mine a block to confirm Alice's sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

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
	numBlocks := padCLTV(
		uint32(invoiceReq.CltvExpiry - incomingBroadcastDelta),
	)

	// We've already mined 2 blocks at this point, so we only need to mine
	// CLTV-2 blocks.
	ht.MineBlocks(int(numBlocks - 2))

	// Expect two txns in the mempool,
	// - Carol's force close tx.
	// - Carol's CPFP anchor sweeping tx.
	// Mine a block to confirm them.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// After the force close tx is mined, Carol should offer her
	// second-level success HTLC tx to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Once Bob is online and sees the force close tx Bob=>Carol, he will
	// create a tx to sweep his commitment output. His anchor outputs will
	// not be swept due to uneconomical. We expect to see three sweeping
	// requests,
	// - the commitment output.
	// - the anchor output from channel Alice=>Bob.
	// - the anchor output from channel Bob=>Carol.
	ht.AssertNumPendingSweeps(bob, 3)

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

	// Assert txns can be found in the mempool.
	//
	// Carol will broadcast her sweeping tx and Bob will sweep his
	// commitment anchor output, we'd expect to see two txns,
	// - Carol's second level HTLC tx.
	// - Bob's commitment output sweeping tx.
	ht.AssertNumTxsInMempool(2)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the sweeping txns made by Bob and Carol.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// When Bob notices Carol's second level tx in the block, he will
	// extract the preimage and broadcast a second level tx to claim the
	// HTLC in his (already closed) channel with Alice, which means Bob has
	// three sweeping requests,
	// - the second level HTLC tx from channel Alice=>Bob.
	// - the anchor output from channel Alice=>Bob.
	// - the anchor output from channel Bob=>Carol.
	ht.AssertNumPendingSweeps(bob, 3)

	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched.
	//
	// TODO(yy): Expose blockbeat to the link layer.
	ht.MineEmptyBlocks(1)

	// At this point, Bob should have broadcast his second layer success
	// tx, and should have sent it to his sweeper.
	//
	// Check Bob's second level tx.
	bobSecondLvlTx := ht.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobSecondLvlTx, bobForceClose)

	// We'll now mine a block which should confirm Bob's second layer tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Bob should consider the channel Bob=>Carol closed, and channel
	// Alice=>Bob pending close.
	ht.AssertNumPendingForceClose(bob, 1)

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine 1 additional block, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	resp := ht.AssertNumPendingForceClose(carol, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Carol's timelock to_local output=%v, timelock on second "+
		"stage htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Carol should have one a sweep request for her second level tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// Carol's sweep tx should be broadcast, assert it's in the mempool and
	// mine it.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We now mine blocks till the CSV lock on Bob's success HTLC on
	// commitment Alice=>Bob expires.
	resp = ht.AssertNumPendingForceClose(bob, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Bob should have three requests in his sweeper.
	// - the second level HTLC tx.
	// - the anchor output from channel Alice=>Bob.
	// - the anchor output from channel Bob=>Carol.
	ht.AssertNumPendingSweeps(bob, 3)

	// When we mine one additional block, that will confirm Bob's sweep.
	// Now Bob should have no pending channels anymore, as this just
	// resolved it by the confirmation of the sweep transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testLocalClaimIncomingHTLCLeased tests `runLocalClaimIncomingHTLCLeased`
// with script enforced lease channel.
func testLocalClaimIncomingHTLCLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalClaimIncomingHTLCLeased(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: leasedType,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgLeased
		cfg = append(cfg, node.CfgZeroConf...)
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalClaimIncomingHTLCLeased(st, cfgs, params)
	})
}

// runLocalClaimIncomingHTLCLeased tests that in a multi-hop HTLC scenario, if
// we force close a channel with an incoming HTLC, and later find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain using
// the HTLC success transaction in order to ensure we don't lose any funds.
//
// TODO(yy): simplify or remove this test as it's too complicated.
func runLocalClaimIncomingHTLCLeased(ht *lntest.HarnessTest,
	cfgs [][]string, params lntest.OpenChannelParams) {

	// Set the min relay feerate to be 5 sat/vbyte so the non-CPFP anchor
	// is never swept.
	//
	// TODO(yy): delete this line once the normal anchor sweeping is
	// removed.
	ht.SetMinRelayFeerate(5000)

	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	aliceChanPoint, bobChanPoint := chanPoints[0], chanPoints[1]

	// Fund Carol one UTXO so she can sweep outputs.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Carol should have enough wallet UTXOs here to sweep the HTLC in the
	// end of this test. However, due to a known issue, Carol's wallet may
	// report there's no UTXO available. For details,
	// - https://github.com/lightningnetwork/lnd/issues/8786
	//
	// TODO(yy): remove this step once the issue is resolved.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()

	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
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

	// At this point, Bob decides that he wants to exit the channel
	// Alice=>Bob immediately, so he force closes his commitment tx.
	closeStream, _ := ht.CloseChannelAssertPending(
		bob, aliceChanPoint, true,
	)

	// For anchor channels, the anchor won't be used for CPFP as there's no
	// deadline pressure for Bob on the channel Alice->Bob at the moment.
	// For Bob's local commitment tx, there's only one incoming HTLC which
	// he doesn't have the preimage yet.
	hasAnchorSweep := false
	bobForceClose := ht.AssertStreamChannelForceClosed(
		bob, aliceChanPoint, hasAnchorSweep, closeStream,
	)

	// Alice will offer her anchor output to her sweeper. Her commitment
	// output cannot be swept yet as it has incurred an additional CLTV due
	// to being the initiator of a script-enforced leased channel.
	//
	// This anchor output cannot be swept due to it being uneconomical.
	ht.AssertNumPendingSweeps(alice, 1)

	// Bob will offer his anchor to his sweeper.
	//
	// This anchor output cannot be swept due to it being uneconomical.
	ht.AssertNumPendingSweeps(bob, 1)

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
	numBlocks := padCLTV(
		uint32(invoiceReq.CltvExpiry - incomingBroadcastDelta),
	)
	ht.MineBlocks(int(numBlocks) - 1)

	// Expect two txns in the mempool,
	// - Carol's force close tx.
	// - Carol's CPFP anchor sweeping tx.
	// Mine a block to confirm them.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// After the force close tx is mined, Carol should offer her
	// second-level success HTLC tx to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Once Bob is online and sees the force close tx Bob=>Carol, he will
	// offer his commitment output to his sweeper, which will be skipped
	// due to it being timelocked. His anchor outputs will not be swept due
	// to uneconomical. We expect to see two sweeping requests,
	// - the anchor output from channel Alice=>Bob.
	// - the anchor output from channel Bob=>Carol.
	ht.AssertNumPendingSweeps(bob, 2)

	// Assert txns can be found in the mempool.
	//
	// Carol will broadcast her second-level HTLC sweeping txns. Bob canoot
	// sweep his commitment anchor output yet due to it being CLTV locked.
	ht.AssertNumTxsInMempool(1)

	// At this point we suspend Alice to make sure she'll handle the
	// on-chain settle after a restart.
	restartAlice := ht.SuspendNode(alice)

	// Mine a block to confirm the sweeping tx from Carol.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// When Bob notices Carol's second level tx in the block, he will
	// extract the preimage and broadcast a second level tx to claim the
	// HTLC in his (already closed) channel with Alice, which means Bob has
	// three sweeping requests,
	// - the second level HTLC tx from channel Alice=>Bob.
	// - the anchor output from channel Alice=>Bob.
	// - the anchor output from channel Bob=>Carol.
	ht.AssertNumPendingSweeps(bob, 3)

	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched.
	//
	// TODO(yy): Expose blockbeat to the link layer.
	ht.MineEmptyBlocks(1)

	// At this point, Bob should have broadcast his second layer success
	// tx, and should have sent it to his sweeper.
	//
	// Check Bob's second level tx.
	bobSecondLvlTx := ht.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobSecondLvlTx, bobForceClose)

	// The channel between Bob and Carol will still be pending force close
	// if this is a leased channel. We'd also check the HTLC stages are
	// correct in both channels.
	ht.AssertNumPendingForceClose(bob, 2)
	ht.AssertNumHTLCsAndStage(bob, aliceChanPoint, 1, 1)
	ht.AssertNumHTLCsAndStage(bob, bobChanPoint, 1, 1)

	// We'll now mine a block which should confirm Bob's second layer tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Now that the preimage from Bob has hit the chain, restart Alice to
	// ensure she'll pick it up.
	require.NoError(ht, restartAlice())

	// If we then mine 1 additional block, Carol's second level tx should
	// mature, and she can pull the funds from it with a sweep tx.
	resp := ht.AssertNumPendingForceClose(carol, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Carol's timelock to_local output=%v, timelock on second "+
		"stage htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Carol should have one a sweep request for her second level tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// Carol's sweep tx should be broadcast, assert it's in the mempool and
	// mine it.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We now mine blocks till the CSV lock on Bob's success HTLC on
	// commitment Alice=>Bob expires.
	resp = ht.AssertChannelPendingForceClose(bob, aliceChanPoint)
	require.Equal(ht, 1, len(resp.PendingHtlcs))
	htlcExpiry := resp.PendingHtlcs[0].BlocksTilMaturity

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity, htlcExpiry)
	ht.MineBlocks(int(htlcExpiry))

	// When we mine one additional block, that will confirm Bob's second
	// level HTLC sweep on channel Alice=>Bob.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We now mine blocks till the CLTV lock on Bob's to_local output HTLC
	// on commitment Bob=>Carol expires.
	resp = ht.AssertChannelPendingForceClose(bob, bobChanPoint)
	require.Equal(ht, 1, len(resp.PendingHtlcs))
	htlcExpiry = resp.PendingHtlcs[0].BlocksTilMaturity

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity, htlcExpiry)
	ht.MineBlocks(int(resp.BlocksTilMaturity))

	// Bob should have three requests in his sweeper.
	// - to_local output from channel Bob=>Carol.
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	ht.AssertNumPendingSweeps(bob, 3)

	// Alice should have two requests in her sweeper,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - her commitment output, now mature.
	ht.AssertNumPendingSweeps(alice, 2)

	// Mine a block to confirm Bob's to_local output sweep.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// All nodes should show zero pending and open channels.
	for _, node := range []*node.HarnessNode{alice, bob, carol} {
		ht.AssertNumPendingForceClose(node, 0)
		ht.AssertNodeNumChannels(node, 0)
	}

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testLocalPreimageClaimAnchor tests `runLocalPreimageClaim` with anchor
// channel.
func testLocalPreimageClaimAnchor(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// anchor channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{Amt: chanAmt}

		cfg := node.CfgAnchor
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalPreimageClaim(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: lnrpc.CommitmentType_ANCHORS,
		}

		// Prepare Carol's node config to enable zero-conf and anchor.
		cfg := node.CfgZeroConf
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalPreimageClaim(st, cfgs, params)
	})
}

// testLocalPreimageClaimSimpleTaproot tests `runLocalClaimIncomingHTLC` with
// simple taproot channel.
func testLocalPreimageClaimSimpleTaproot(ht *lntest.HarnessTest) {
	c := lnrpc.CommitmentType_SIMPLE_TAPROOT

	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// simple taproot channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			Private:        true,
		}

		cfg := node.CfgSimpleTaproot
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalPreimageClaim(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
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

		runLocalPreimageClaim(st, cfgs, params)
	})
}

// runLocalPreimageClaim tests that in the multi-hop HTLC scenario, if the
// remote party goes to chain while we have an incoming HTLC, then when we
// found out the preimage via the witness beacon, we properly settle the HTLC
// directly on-chain using the preimage in order to ensure that we don't lose
// any funds.
func runLocalPreimageClaim(ht *lntest.HarnessTest,
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
	aliceChanPoint := chanPoints[0]

	// Fund Carol one UTXO so she can sweep outputs.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Carol should have enough wallet UTXOs here to sweep the HTLC in the
	// end of this test. However, due to a known issue, Carol's wallet may
	// report there's no UTXO available. For details,
	// - https://github.com/lightningnetwork/lnd/issues/8786
	//
	// TODO(yy): remove this step once the issue is resolved.
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
	preimage := ht.RandomPreimage()
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

	// Record the height which the invoice will expire.
	invoiceExpiry := ht.CurrentHeight() + uint32(invoiceReq.CltvExpiry)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, aliceChanPoint, true,
	)
	aliceForceClose := ht.AssertStreamChannelForceClosed(
		alice, aliceChanPoint, true, closeStream,
	)

	// Wait for the channel to be marked pending force close.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint)

	// Once the force closing tx is mined, Alice should offer the anchor
	// output to her sweeper.
	ht.AssertNumPendingSweeps(alice, 1)

	// Bob should offer his anchor output to his sweeper.
	ht.AssertNumPendingSweeps(bob, 1)

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel. AssertStreamChannelForceClosed() already mined a
	// block, so mine one less than defaultCSV in order to perform mempool
	// assertions.
	ht.MineBlocks(defaultCSV - 1)

	// Mine Alice's commit sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	ht.Logf("Invoice expire height: %d, current: %d", invoiceExpiry,
		ht.CurrentHeight())

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(
		invoiceExpiry - ht.CurrentHeight() - incomingBroadcastDelta,
	)
	ht.MineBlocks(int(numBlocks))

	// Since Carol has time-sensitive HTLCs, she will use the anchor for
	// CPFP purpose. Assert the anchor output is offered to the sweeper.
	//
	// For neutrino backend, Carol still have the two anchors - one from
	// local commitment and the other from the remote.
	if ht.IsNeutrinoBackend() {
		ht.AssertNumPendingSweeps(carol, 2)
	} else {
		ht.AssertNumPendingSweeps(carol, 1)
	}

	// We should see two txns in the mempool, we now a block to confirm,
	// - Carol's force close tx.
	// - Carol's anchor sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Once the force close tx is confirmed, Carol should offer her
	// incoming HTLC to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Bob should have three sweeping requests,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	// - the commit output sweep from the channel with Carol, no timelock.
	ht.AssertNumPendingSweeps(bob, 3)

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

	// We mine one block to confirm,
	// - Carol's sweeping tx of the incoming HTLC.
	// - Bob's sweeping tx of his commit output.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// When Bob notices Carol's second level tx in the block, he will
	// extract the preimage and offer the HTLC to his sweeper. So he has,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	// - the htlc sweeping tx.
	ht.AssertNumPendingSweeps(bob, 3)

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

	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched.
	//
	// TODO(yy): Expose blockbeat to the link layer.
	ht.MineEmptyBlocks(1)

	// Bob should broadcast the sweeping of the direct preimage spent now.
	bobHtlcSweep := ht.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobHtlcSweep, aliceForceClose)

	// We'll now mine a block which should confirm Bob's HTLC sweep tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Now that the sweeping tx has been confirmed, Bob should recognize
	// that all contracts for the Bob-Carol channel have been fully
	// resolved.
	ht.AssertNumPendingForceClose(bob, 0)

	// Mine blocks till Carol's second level tx matures.
	resp := ht.AssertNumPendingForceClose(carol, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Carol's timelock to_local output=%v, timelock on second "+
		"stage htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Carol should offer the htlc output to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Mine a block to confirm Carol's sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	ht.AssertNumPendingForceClose(carol, 0)

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
}

// testLocalPreimageClaimLeased tests `runLocalPreimageClaim` with script
// enforced lease channel.
func testLocalPreimageClaimLeased(ht *lntest.HarnessTest) {
	success := ht.Run("no zero conf", func(t *testing.T) {
		st := ht.Subtest(t)

		// Create a three hop network: Alice -> Bob -> Carol, using
		// leased channels.
		//
		// Prepare params.
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: leasedType,
		}

		cfg := node.CfgLeased
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalPreimageClaimLeased(st, cfgs, params)
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
		params := lntest.OpenChannelParams{
			Amt:            chanAmt,
			ZeroConf:       true,
			CommitmentType: leasedType,
		}

		// Prepare Carol's node config to enable zero-conf and leased
		// channel.
		cfg := node.CfgLeased
		cfg = append(cfg, node.CfgZeroConf...)
		cfgs := [][]string{cfg, cfg, cfg}

		runLocalPreimageClaimLeased(st, cfgs, params)
	})
}

// runLocalPreimageClaimLeased tests that in the multi-hop HTLC scenario, if
// the remote party goes to chain while we have an incoming HTLC, then when we
// found out the preimage via the witness beacon, we properly settle the HTLC
// directly on-chain using the preimage in order to ensure that we don't lose
// any funds.
func runLocalPreimageClaimLeased(ht *lntest.HarnessTest,
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
	aliceChanPoint, bobChanPoint := chanPoints[0], chanPoints[1]

	// Fund Carol one UTXO so she can sweep outputs.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()

	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      invoiceAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
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

	// Record the height which the invoice will expire.
	invoiceExpiry := ht.CurrentHeight() + uint32(invoiceReq.CltvExpiry)

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, aliceChanPoint, true,
	)
	aliceForceClose := ht.AssertStreamChannelForceClosed(
		alice, aliceChanPoint, true, closeStream,
	)

	// Wait for the channel to be marked pending force close.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint)

	// Once the force closing tx is mined, Alice should offer the anchor
	// output to her sweeper.
	ht.AssertNumPendingSweeps(alice, 1)

	// Bob should offer his anchor output to his sweeper.
	ht.AssertNumPendingSweeps(bob, 1)

	// Suspend bob, so Carol is forced to go on chain.
	restartBob := ht.SuspendNode(bob)

	// Settle invoice. This will just mark the invoice as settled, as there
	// is no link anymore to remove the htlc from the commitment tx. For
	// this test, it is important to actually settle and not leave the
	// invoice in the accepted state, because without a known preimage, the
	// channel arbitrator won't go to chain.
	carol.RPC.SettleInvoice(preimage[:])

	ht.Logf("Invoice expire height: %d, current: %d", invoiceExpiry,
		ht.CurrentHeight())

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := padCLTV(
		invoiceExpiry - ht.CurrentHeight() - incomingBroadcastDelta - 1,
	)
	ht.MineBlocks(int(numBlocks))

	// Since Carol has time-sensitive HTLCs, she will use the anchor for
	// CPFP purpose. Assert the anchor output is offered to the sweeper.
	//
	// For neutrino backend, there's no way to know the sweeping of the
	// remote anchor is failed, so Carol still sees two pending sweeps.
	if ht.IsNeutrinoBackend() {
		ht.AssertNumPendingSweeps(carol, 2)
	} else {
		ht.AssertNumPendingSweeps(carol, 1)
	}

	// We should see two txns in the mempool, we now a block to confirm,
	// - Carol's force close tx.
	// - Carol's anchor sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Once the force close tx is confirmed, Carol should offer her
	// incoming HTLC to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Restart bob again.
	require.NoError(ht, restartBob())

	// Bob should have two sweeping requests,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	// - the commit output sweep from the channel with Carol, which is CLTV
	//   locked so it won't show up the pending sweeps.
	ht.AssertNumPendingSweeps(bob, 2)

	// We mine one block to confirm,
	// - Carol's sweeping tx of the incoming HTLC.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// When Bob notices Carol's second level tx in the block, he will
	// extract the preimage and offer the HTLC to his sweeper. So he has,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	// - the htlc sweeping tx.
	ht.AssertNumPendingSweeps(bob, 3)

	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched.
	//
	// TODO(yy): Expose blockbeat to the link layer.
	ht.MineEmptyBlocks(1)

	// Bob should broadcast the sweeping of the direct preimage spent now.
	bobHtlcSweep := ht.GetNumTxsFromMempool(1)[0]

	// It should spend from the commitment in the channel with Alice.
	ht.AssertTxSpendFrom(bobHtlcSweep, aliceForceClose)

	// We'll now mine a block which should confirm Bob's HTLC sweep tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Now that the sweeping tx has been confirmed, Bob should recognize
	// that all contracts for the Bob-Carol channel have been fully
	// resolved.
	ht.AssertNumPendingForceClose(bob, 1)
	ht.AssertChannelPendingForceClose(bob, bobChanPoint)

	// Mine blocks till Carol's second level tx matures.
	resp := ht.AssertNumPendingForceClose(carol, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Carol's timelock to_local output=%v, timelock on second "+
		"stage htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Carol should offer the htlc output to her sweeper.
	ht.AssertNumPendingSweeps(carol, 1)

	// Mine a block to confirm Carol's sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// When Carol's sweep gets confirmed, she should have no more pending
	// channels.
	ht.AssertNumPendingForceClose(carol, 0)

	// The invoice should show as settled for Carol, indicating that it was
	// swept on-chain.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)

	// Check that the Alice's payment is correctly marked succeeded.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// With the script-enforced lease commitment type, Alice and Bob still
	// haven't been able to sweep their respective commit outputs due to
	// the additional CLTV. We'll need to mine enough blocks for the
	// timelock to expire and prompt their sweep.
	//
	// Get num of blocks to mine.
	resp = ht.AssertNumPendingForceClose(alice, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Alice's timelock to_local output=%v, timelock on second "+
		"stage htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	ht.MineBlocks(int(resp.BlocksTilMaturity))

	// Alice should two sweeping requests,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the commit output sweep from the channel with Bob.
	ht.AssertNumPendingSweeps(alice, 2)

	// Bob should have three sweeping requests,
	// - the anchor output from channel Alice=>Bob, uneconomical.
	// - the anchor output from channel Bob=>Carol, uneconomical.
	// - the commit output sweep from the channel with Carol.
	ht.AssertNumPendingSweeps(bob, 3)

	// Confirm their sweeps.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Both nodes should consider the channel fully closed.
	ht.AssertNumPendingForceClose(alice, 0)
	ht.AssertNumPendingForceClose(bob, 0)
}
