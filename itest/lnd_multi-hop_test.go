package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
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
// the sweep tx found in mempool. The direct spend tx is broadcast by Carol and
// spends the outpoint on Bob's commit tx.
func testHtlcTimeoutResolverExtractPreimageLocal(ht *lntest.HarnessTest) {
	// For neutrino backend there's no mempool source so we skip it. The
	// test of extracting preimage from blocks has already been covered in
	// other tests.
	if ht.IsNeutrinoBackend() {
		ht.Skip("skipping neutrino")
	}

	// Set the min relay feerate to be 10 sat/vbyte so the non-CPFP anchor
	// is never swept.
	//
	// TODO(yy): delete this line once the normal anchor sweeping is
	// removed.
	ht.SetMinRelayFeerate(10_000)

	// Create a three hop network: Alice -> Bob -> Carol, using
	// anchor channels.
	//
	// Prepare params.
	params := lntest.OpenChannelParams{Amt: chanAmt}
	cfg := node.CfgAnchor
	cfgs := [][]string{cfg, cfg, cfg}

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	// Create a three hop network: Alice -> Bob -> Carol.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	aliceChanPoint, bobChanPoint := chanPoints[0], chanPoints[1]

	// With the network active, we'll now add a new hodl invoice at Carol's
	// end. Make sure the cltv expiry delta is large enough, otherwise Bob
	// won't send out the outgoing htlc.
	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      100_000,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	carolInvoice := carol.RPC.AddHoldInvoice(invoiceReq)

	// Record the height which the invoice will expire.
	invoiceExpiry := ht.CurrentHeight() + uint32(invoiceReq.CltvExpiry)

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
	ht.AssertOutgoingHTLCActive(bob, bobChanPoint, payHash[:])

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

	// Mine Bob's force close tx.
	ht.MineClosingTx(bobChanPoint)

	// Once Bob's force closing tx is confirmed, he will re-offer the
	// anchor output to his sweeper, which won't be swept due to it being
	// uneconomical.
	ht.AssertNumPendingSweeps(bob, 1)

	// Mine 3 blocks so the output will be offered to the sweeper.
	ht.MineBlocks(defaultCSV - 1)

	// Bob should have two pending sweeps now,
	// - the commit output.
	// - the anchor output, uneconomical.
	ht.AssertNumPendingSweeps(bob, 2)

	// Mine a block to confirm Bob's sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	ht.Logf("Invoice expire height: %d, current: %d", invoiceExpiry,
		ht.CurrentHeight())

	// We'll now mine enough blocks to trigger Carol's sweeping of the htlc
	// via the direct spend.
	numBlocks := padCLTV(
		invoiceExpiry - ht.CurrentHeight() - incomingBroadcastDelta,
	)
	ht.MineBlocks(int(numBlocks))

	// Restart Carol to sweep the htlc output.
	require.NoError(ht, restartCarol())

	// With the above blocks mined, we should expect Carol's to offer the
	// htlc output on Bob's commitment to the sweeper.
	//
	// Carol should two pending sweeps,
	// - htlc output.
	// - anchor output, uneconomical.
	ht.AssertNumPendingSweeps(carol, 2)

	// Check the current mempool state and we should see,
	// - Carol's direct spend tx, which contains the preimage.
	// - Carol's anchor sweep tx cannot be broadcast as it's uneconomical.
	ht.AssertNumTxsInMempool(1)

	// We'll now mine enough blocks to trigger Bob's htlc timeout resolver
	// to act. Once his timeout resolver starts, it will extract the
	// preimage from Carol's direct spend tx found in the mempool.
	resp := ht.AssertNumPendingForceClose(bob, 1)[0]
	require.Equal(ht, 1, len(resp.PendingHtlcs))

	ht.Logf("Bob's timelock to_local output=%v, timelock on second stage "+
		"htlc=%v", resp.BlocksTilMaturity,
		resp.PendingHtlcs[0].BlocksTilMaturity)

	// Mine empty blocks so Carol's direct spend tx stays in mempool. Once
	// the height is reached, Bob's timeout resolver will resolve the htlc
	// by extracing the preimage from the mempool.
	//
	// TODO(yy): there's no need to wait till the HTLC's CLTV is reached,
	// Bob's outgoing contest resolver can also monitor the mempool and
	// resolve the payment even earlier.
	ht.MineEmptyBlocks(int(resp.PendingHtlcs[0].BlocksTilMaturity))

	// Finally, check that the Alice's payment is marked as succeeded as
	// Bob has settled the htlc using the preimage extracted from Carol's
	// direct spend tx.
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)

	// NOTE: for non-standby nodes there's no need to clean up the force
	// close as long as the mempool is cleaned.
	ht.CleanShutDown()
}
