package itest

import (
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testUpdateChannelPolicy tests that policy updates made to a channel
// gets propagated to other nodes in the network.
func testUpdateChannelPolicy(ht *lntest.HarnessTest) {
	const (
		defaultFeeBase       = 1000
		defaultFeeRate       = 1
		defaultTimeLockDelta = chainreg.DefaultBitcoinTimeLockDelta
		defaultMinHtlc       = 1000
	)
	defaultMaxHtlc := lntest.CalculateMaxHtlc(funding.MaxBtcFundingAmount)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	// Create a channel Alice->Bob.
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	alice, bob := nodes[0], nodes[1]
	chanPoint := chanPoints[0]

	// Alice and Bob should see each other's ChannelUpdates, advertising the
	// default routing policies. We do not currently set any inbound fees.
	// The inbound base and inbound fee rate are advertised with a default
	// of 0.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultFeeBase,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}

	assertNodesPolicyUpdate(ht, nodes, alice, expectedPolicy, chanPoint)
	assertNodesPolicyUpdate(ht, nodes, bob, expectedPolicy, chanPoint)

	// They should now know about the default policies.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		ht.AssertChannelPolicy(
			node, bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	// Create Carol with options to rate limit channel updates up to 2 per
	// day, and create a new channel Bob->Carol.
	carol := ht.NewNode(
		"Carol", []string{
			"--gossip.max-channel-update-burst=2",
			"--gossip.channel-update-interval=24h",
		},
	)
	ht.ConnectNodes(carol, bob)
	nodes = append(nodes, carol)

	// Send some coins to Carol that can be used for channel funding.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Open the channel Carol->Bob with a custom min_htlc value set. Since
	// Carol is opening the channel, she will require Bob to not forward
	// HTLCs smaller than this value, and hence he should advertise it as
	// part of his ChannelUpdate.
	const customMinHtlc = 5000
	chanPoint2 := ht.OpenChannel(
		carol, bob, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
			MinHtlc: customMinHtlc,
		},
	)

	expectedPolicyBob := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultFeeBase,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          customMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}
	expectedPolicyCarol := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultFeeBase,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}

	assertNodesPolicyUpdate(ht, nodes, bob, expectedPolicyBob, chanPoint2)
	assertNodesPolicyUpdate(
		ht, nodes, carol, expectedPolicyCarol, chanPoint2,
	)

	// Check that all nodes now know about the updated policies.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, bob.PubKeyStr, expectedPolicyBob, chanPoint2,
		)
		ht.AssertChannelPolicy(
			node, carol.PubKeyStr, expectedPolicyCarol, chanPoint2,
		)
	}

	// Make sure Alice and Carol have seen each other's channels.
	ht.AssertChannelInGraph(alice, chanPoint2)
	ht.AssertChannelInGraph(carol, chanPoint)

	// First we'll try to send a payment from Alice to Carol with an amount
	// less than the min_htlc value required by Carol. This payment should
	// fail, as the channel Bob->Carol cannot carry HTLCs this small.
	payAmt := btcutil.Amount(4)
	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	resp := carol.RPC.AddInvoice(invoice)

	// Alice knows about the channel policy of Carol and should therefore
	// not be able to find a path during routing.
	payReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertFail(
		alice, payReq,
		lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	)

	// Now we try to send a payment over the channel with a value too low
	// to be accepted. First we query for a route to route a payment of
	// 5000 mSAT, as this is accepted.
	payAmt = btcutil.Amount(5)
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            int64(payAmt),
		FinalCltvDelta: defaultTimeLockDelta,
	}
	routes := alice.RPC.QueryRoutes(routesReq)
	require.Len(ht, routes.Routes, 1)

	// We change the route to carry a payment of 4000 mSAT instead of 5000
	// mSAT.
	payAmt = btcutil.Amount(4)
	amtSat := int64(payAmt)
	amtMSat := int64(lnwire.NewMSatFromSatoshis(payAmt))
	routes.Routes[0].Hops[0].AmtToForward = amtSat
	routes.Routes[0].Hops[0].AmtToForwardMsat = amtMSat
	routes.Routes[0].Hops[1].AmtToForward = amtSat
	routes.Routes[0].Hops[1].AmtToForwardMsat = amtMSat

	// Send the payment with the modified value.
	alicePayStream := alice.RPC.SendToRoute()

	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       routes.Routes[0],
	}
	err := alicePayStream.Send(sendReq)
	require.NoError(ht, err, "unable to send payment")

	// We expect this payment to fail, and that the min_htlc value is
	// communicated back to us, since the attempted HTLC value was too low.
	sendResp, err := ht.ReceiveSendToRouteUpdate(alicePayStream)
	require.NoError(ht, err, "unable to receive payment stream")

	// Expected as part of the error message.
	substrs := []string{
		"AmountBelowMinimum",
		"HtlcMinimumMsat: (lnwire.MilliSatoshi) 5000 mSAT",
	}
	for _, s := range substrs {
		require.Contains(ht, sendResp.PaymentError, s)
	}

	// Make sure sending using the original value succeeds.
	payAmt = btcutil.Amount(5)
	amtSat = int64(payAmt)
	amtMSat = int64(lnwire.NewMSatFromSatoshis(payAmt))
	routes.Routes[0].Hops[0].AmtToForward = amtSat
	routes.Routes[0].Hops[0].AmtToForwardMsat = amtMSat
	routes.Routes[0].Hops[1].AmtToForward = amtSat
	routes.Routes[0].Hops[1].AmtToForwardMsat = amtMSat

	// Manually set the MPP payload a new for each payment since
	// the payment addr will change with each invoice, although we
	// can re-use the route itself.
	route := routes.Routes[0]
	route.Hops[len(route.Hops)-1].TlvPayload = true
	route.Hops[len(route.Hops)-1].MppRecord = &lnrpc.MPPRecord{
		PaymentAddr:  resp.PaymentAddr,
		TotalAmtMsat: amtMSat,
	}

	sendReq = &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       route,
	}

	err = alicePayStream.Send(sendReq)
	require.NoError(ht, err, "unable to send payment")

	sendResp, err = ht.ReceiveSendToRouteUpdate(alicePayStream)
	require.NoError(ht, err, "unable to receive payment stream")
	require.Empty(ht, sendResp.PaymentError, "expected payment to succeed")

	// With our little cluster set up, we'll update the outbound fees and
	// the max htlc size for the Bob side of the Alice->Bob channel, and
	// make sure all nodes learn about it. Inbound fees remain at 0.
	baseFee := int64(1500)
	feeRate := int64(12)
	timeLockDelta := uint32(66)
	maxHtlc := uint64(500000)

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      maxHtlc,
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMsat:   maxHtlc,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}
	updateResp := bob.RPC.UpdateChannelPolicy(req)
	require.Empty(ht, updateResp.FailedUpdates, 0)

	// Wait for all nodes to have seen the policy update done by Bob.
	assertNodesPolicyUpdate(ht, nodes, bob, expectedPolicy, chanPoint)

	// Check that all nodes now know about Bob's updated policy.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	// Now that all nodes have received the new channel update, we'll try
	// to send a payment from Alice to Carol to ensure that Alice has
	// internalized this fee update. This shouldn't affect the route that
	// Alice takes though: we updated the Alice -> Bob channel and she
	// doesn't pay for transit over that channel as it's direct.
	// Note that the payment amount is >= the min_htlc value for the
	// channel Bob->Carol, so it should successfully be forwarded.
	payAmt = btcutil.Amount(5)
	invoice = &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	resp = carol.RPC.AddInvoice(invoice)

	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// We'll now open a channel from Alice directly to Carol.
	ht.ConnectNodes(alice, carol)
	chanPoint3 := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// Make sure Bob knows this channel.
	ht.AssertChannelInGraph(bob, chanPoint3)

	// Make a global update, and check that both channels' new policies get
	// propagated.
	baseFee = int64(800)
	feeRate = int64(123)
	timeLockDelta = uint32(22)
	maxHtlc *= 2
	inboundBaseFee := int32(-400)
	inboundFeeRatePpm := int32(-60)

	expectedPolicy.FeeBaseMsat = baseFee
	expectedPolicy.FeeRateMilliMsat = testFeeBase * feeRate
	expectedPolicy.TimeLockDelta = timeLockDelta
	expectedPolicy.MaxHtlcMsat = maxHtlc
	expectedPolicy.InboundFeeBaseMsat = inboundBaseFee
	expectedPolicy.InboundFeeRateMilliMsat = inboundFeeRatePpm

	req = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMsat:   maxHtlc,
		InboundFee: &lnrpc.InboundFee{
			BaseFeeMsat: inboundBaseFee,
			FeeRatePpm:  inboundFeeRatePpm,
		},
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_Global{}
	alice.RPC.UpdateChannelPolicy(req)

	// Wait for all nodes to have seen the policy updates for both of
	// Alice's channels.
	assertNodesPolicyUpdate(ht, nodes, alice, expectedPolicy, chanPoint)
	assertNodesPolicyUpdate(ht, nodes, alice, expectedPolicy, chanPoint3)

	// And finally check that all nodes remembers the policy update they
	// received.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		ht.AssertChannelPolicy(
			node, alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
	}

	// Now, to test that Carol is properly rate limiting incoming updates,
	// we'll send two more update from Alice. Carol should accept the first,
	// but not the second, as she only allows two updates per day and a day
	// has yet to elapse from the previous update.

	// updateAndAssertAliceAndBob is a helper closure which updates Alice's
	// policy and asserts that both Alice and Bob have heard and updated the
	// policy in their graph.
	updateAndAssertAliceAndBob := func(req *lnrpc.PolicyUpdateRequest,
		expectedPolicy *lnrpc.RoutingPolicy) {

		alice.RPC.UpdateChannelPolicy(req)

		// Wait for all nodes to have seen the policy updates for both
		// of Alice's channels. Carol will not see the last update as
		// the limit has been reached.
		assertNodesPolicyUpdate(
			ht, []*node.HarnessNode{alice, bob},
			alice, expectedPolicy, chanPoint,
		)
		assertNodesPolicyUpdate(
			ht, []*node.HarnessNode{alice, bob},
			alice, expectedPolicy, chanPoint3,
		)

		// Check that all nodes remember the policy update
		// they received.
		ht.AssertChannelPolicy(
			alice, alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		ht.AssertChannelPolicy(
			alice, alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
		ht.AssertChannelPolicy(
			bob, alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		ht.AssertChannelPolicy(
			bob, alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
	}

	// Double the base fee and attach to the policy. Moreover, we set the
	// inbound fee to nil and test that it does not change the propagated
	// inbound fee.
	baseFee1 := baseFee * 2
	expectedPolicy.FeeBaseMsat = baseFee1
	req.BaseFeeMsat = baseFee1
	req.InboundFee = nil
	updateAndAssertAliceAndBob(req, expectedPolicy)

	// Check that Carol has both heard the policy and updated it in her
	// graph.
	assertNodesPolicyUpdate(
		ht, []*node.HarnessNode{carol},
		alice, expectedPolicy, chanPoint,
	)
	assertNodesPolicyUpdate(
		ht, []*node.HarnessNode{carol},
		alice, expectedPolicy, chanPoint3,
	)
	ht.AssertChannelPolicy(
		carol, alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	ht.AssertChannelPolicy(
		carol, alice.PubKeyStr, expectedPolicy, chanPoint3,
	)

	// Double the base fee and attach to the policy.
	baseFee2 := baseFee1 * 2
	expectedPolicy.FeeBaseMsat = baseFee2
	req.BaseFeeMsat = baseFee2
	updateAndAssertAliceAndBob(req, expectedPolicy)

	// Since Carol didn't receive the last update, she still has Alice's
	// old policy. We validate this by checking the base fee is the older
	// one.
	expectedPolicy.FeeBaseMsat = baseFee1
	ht.AssertChannelPolicy(
		carol, alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	ht.AssertChannelPolicy(
		carol, alice.PubKeyStr, expectedPolicy, chanPoint3,
	)
}

// testSendUpdateDisableChannel ensures that a channel update with the disable
// flag set is sent once a channel has been either unilaterally or cooperatively
// closed.
//
// NOTE: this test can be flaky as we are testing the chan-enable-timeout and
// chan-disable-timeout flags here. For instance, if some operations take more
// than 6 seconds to finish, the channel will be marked as disabled, thus a
// following operation will fail if it relies on the channel being enabled.
func testSendUpdateDisableChannel(ht *lntest.HarnessTest) {
	const chanAmt = 100000

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// Create a new node Eve, which will be restarted later with a config
	// that has an inactive channel timeout of just 6 seconds (down from
	// the default 20m). It will be used to test channel updates for
	// channels going inactive.
	//
	// NOTE: we don't create Eve with the chan-disable-timeout here because
	// the following channel openings might take longer than that timeout
	// value, which will cause the channel Eve=>Carol being marked as
	// disabled.
	eve := ht.NewNode("Eve", nil)

	// Create a new node Carol, which will later be restarted with the same
	// config as Eve's.
	carol := ht.NewNode("Carol", nil)

	// Launch a node for Dave which will connect to Bob in order to receive
	// graph updates from. This will ensure that the channel updates are
	// propagated throughout the network.
	dave := ht.NewNode("Dave", nil)

	// We will start our test by creating the following topology,
	// Alice --- Bob --- Dave
	//   |       |
	// Carol --- Eve
	ht.EnsureConnected(alice, bob)
	ht.ConnectNodes(alice, carol)
	ht.ConnectNodes(bob, dave)
	ht.ConnectNodes(eve, carol)

	// Connect Eve and Bob using a persistent connection. Later after Eve
	// is restarted, they will connect again automatically.
	ht.ConnectNodesPerm(bob, eve)

	// Give Eve some coins.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, eve)

	// We now proceed to open channels: Alice=>Bob, Alice=>Carol and
	// Eve=>Carol.
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: alice, Remote: carol, Param: p},
		{Local: eve, Remote: carol, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)

	// Extract channel points from the response.
	chanPointAliceBob := resp[0]
	chanPointAliceCarol := resp[1]
	chanPointEveCarol := resp[2]

	// We will use 10 seconds as the disable timeout.
	chanDisableTimeout := 10
	chanEnableTimeout := 5

	// waitChanDisabled is a helper closure to wait the chanDisableTimeout
	// seconds such that the channel disable logic is taking effect.
	waitChanDisabled := func() {
		time.Sleep(time.Duration(chanDisableTimeout) * time.Second)
	}

	// With the channels open, we now restart Carol and Eve to use
	// customized timeout values.
	nodeCfg := []string{
		"--minbackoff=60s",
		fmt.Sprintf("--chan-enable-timeout=%ds", chanEnableTimeout),
		fmt.Sprintf("--chan-disable-timeout=%ds", chanDisableTimeout),
		"--chan-status-sample-interval=.5s",
	}
	ht.RestartNodeWithExtraArgs(carol, nodeCfg)
	ht.RestartNodeWithExtraArgs(eve, nodeCfg)

	// Dave should know all the channels.
	ht.AssertChannelInGraph(dave, chanPointAliceBob)
	ht.AssertChannelInGraph(dave, chanPointAliceCarol)
	ht.AssertChannelInGraph(dave, chanPointEveCarol)

	// We should expect to see a channel update with the default routing
	// policy, except that it should indicate the channel is disabled.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(chainreg.DefaultBitcoinBaseFeeMSat),
		FeeRateMilliMsat: int64(chainreg.DefaultBitcoinFeeRate),
		TimeLockDelta:    chainreg.DefaultBitcoinTimeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      lntest.CalculateMaxHtlc(chanAmt),
		Disabled:         true,
	}

	// assertPolicyUpdate checks that the required policy update has
	// happened on the given node.
	assertPolicyUpdate := func(node *node.HarnessNode,
		policy *lnrpc.RoutingPolicy, chanPoint *lnrpc.ChannelPoint,
		numUpdates int) {

		ht.AssertNumPolicyUpdates(dave, chanPoint, node, numUpdates)
		ht.AssertChannelPolicyUpdate(
			dave, node, policy, chanPoint, false,
		)
	}

	// Let Carol go offline. Since Eve has an inactive timeout of 6s, we
	// expect her to send an update disabling the channel.
	restartCarol := ht.SuspendNode(carol)

	// We expect to see a total of 2 channel policy updates from the
	// channel Carol <-> Eve and advertised by Eve using the route
	// Eve->Bob->Dave.
	waitChanDisabled()
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 2)

	// We restart Carol. Since the channel now becomes active again, Eve
	// should send a ChannelUpdate setting the channel no longer disabled.
	require.NoError(ht, restartCarol(), "unable to restart carol")

	expectedPolicy.Disabled = false
	// We expect to see a total of 3 channel policy updates from the
	// channel Carol <-> Eve and advertised by Eve using the route
	// Eve->Bob->Dave.
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 3)

	// Wait until Carol and Eve are reconnected before we disconnect them
	// again.
	ht.EnsureConnected(eve, carol)

	// Now we'll test a long disconnection. Disconnect Carol and Eve and
	// ensure they both detect each other as disabled. Their min backoffs
	// are high enough to not interfere with disabling logic.
	ht.DisconnectNodes(carol, eve)

	// Wait for a disable from both Carol and Eve to come through.
	expectedPolicy.Disabled = true
	// We expect to see a total of 4 channel policy updates from the
	// channel Carol <-> Eve and advertised by Eve using the route
	// Eve->Bob->Dave.
	waitChanDisabled()
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 4)

	// Because Carol has restarted twice before, depending on how much time
	// it has taken, she might mark the channel disabled and enable it
	// multiple times.  Thus we could see a total of 2 or 4 or 6 channel
	// policy updates from the channel Carol <-> Eve and advertised by
	// Carol using the route Carol->Alice->Bob->Dave.
	//
	// Assume there are 2 channel policy updates from Carol, and update it
	// if more has found
	numCarol := 2
	op := ht.OutPointFromChannelPoint(chanPointEveCarol)
	policyMap := dave.Watcher.GetPolicyUpdates(op)
	nodePolicy, ok := policyMap[carol.PubKeyStr]
	switch {
	case !ok:
		break
	case len(nodePolicy) > 2:
		numCarol = 4
	case len(nodePolicy) > 4:
		numCarol = 6
	}
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol, numCarol)

	// Reconnect Carol and Eve, this should cause them to reenable the
	// channel from both ends after a short delay.
	ht.EnsureConnected(carol, eve)

	expectedPolicy.Disabled = false
	// We expect to see a total of 5 channel policy updates from the
	// channel Carol <-> Eve and advertised by Eve using the route
	// Eve->Bob->Dave.
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 5)
	// We expect to see a total of 3 or 5 channel policy updates from the
	// channel Carol <-> Eve and advertised by Carol using the route
	// Carol->Alice->Bob->Dave.
	numCarol++
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol, numCarol)

	// Now we'll test a short disconnection. Disconnect Carol and Eve, then
	// reconnect them after one second so that their scheduled disables are
	// aborted. One second is twice the status sample interval, so this
	// should allow for the disconnect to be detected, but still leave time
	// to cancel the announcement before the 6 second inactive timeout is
	// hit.
	ht.DisconnectNodes(carol, eve)
	time.Sleep(time.Second)
	ht.EnsureConnected(eve, carol)

	// Since the disable should have been canceled by both Carol and Eve,
	// we expect no channel updates to appear on the network, which means
	// we expect the polices stay unchanged(Disable == false).
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 5)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol, numCarol)

	// Close Alice's channels with Bob and Carol cooperatively and
	// unilaterally respectively. Note that the CloseChannel will mine a
	// block and check that the closing transaction can be found in both
	// the mempool and the block.
	ht.CloseChannel(alice, chanPointAliceBob)
	ht.ForceCloseChannel(alice, chanPointAliceCarol)

	// Now that the channel close processes have been started, we should
	// receive an update marking each as disabled.
	expectedPolicy.Disabled = true
	// We expect to see a total of 2 channel policy updates from the
	// channel Alice <-> Bob and advertised by Alice using the route
	// Alice->Bob->Dave.
	assertPolicyUpdate(alice, expectedPolicy, chanPointAliceBob, 2)
	// We expect to see a total of 2 channel policy updates from the
	// channel Alice <-> Carol and advertised by Alice using the route
	// Alice->Bob->Dave.
	assertPolicyUpdate(alice, expectedPolicy, chanPointAliceCarol, 2)

	// Also do this check for Eve's channel with Carol.
	ht.CloseChannel(eve, chanPointEveCarol)

	// We expect to see a total of 5 channel policy updates from the
	// channel Carol <-> Eve and advertised by Eve using the route
	// Eve->Bob->Dave.
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol, 6)
}

// testUpdateChannelPolicyForPrivateChannel tests when a private channel
// updates its channel edge policy, we will use the updated policy to send our
// payment.
// The topology is created as: Alice -> Bob -> Carol, where Alice -> Bob is
// public and Bob -> Carol is private. After an invoice is created by Carol,
// Bob will update the base fee via UpdateChannelPolicy, we will test that
// Alice will not fail the payment and send it using the updated channel
// policy.
func testUpdateChannelPolicyForPrivateChannel(ht *lntest.HarnessTest) {
	const (
		chanAmt     = btcutil.Amount(100000)
		paymentAmt  = 20000
		baseFeeMSat = 33000
	)

	// We'll create the following topology first,
	// Alice <--public:100k--> Bob <--private:100k--> Carol
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	// Open a channel with 100k satoshis between Alice and Bob.
	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Create a new node Carol.
	carol := ht.NewNode("Carol", nil)

	// Connect Carol to Bob.
	ht.ConnectNodes(carol, bob)

	// Open a channel with 100k satoshis between Bob and Carol.
	chanPointBobCarol := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Carol should be aware of the channel between Alice and Bob.
	ht.AssertChannelInGraph(carol, chanPointAliceBob)

	// We should have the following topology now,
	// Alice <--public:100k--> Bob <--private:100k--> Carol
	//
	// Now we will create an invoice for Carol.
	invoice := &lnrpc.Invoice{
		Memo:    "routing hints",
		Value:   paymentAmt,
		Private: true,
	}
	resp := carol.RPC.AddInvoice(invoice)

	// Bob now updates the channel edge policy for the private channel.
	timeLockDelta := uint32(chainreg.DefaultBitcoinTimeLockDelta)
	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFeeMSat,
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointBobCarol,
		},
	}
	bob.RPC.UpdateChannelPolicy(updateFeeReq)

	// Alice pays the invoices. She will use the updated baseFeeMSat in the
	// payment
	//
	// TODO(yy): we may get a flake saying the timeout checking the
	// payment's state, which is due to slow round of HTLC settlement. An
	// example log is shown below, where Alice sent RevokeAndAck to Bob,
	// but it took Bob 7 seconds to reply back the final UpdateFulfillHTLC.
	//
	// 2022-11-14 06:23:59.774 PEER: Peer(Bob): Sending UpdateAddHTLC
	// 2022-11-14 06:24:00.635 PEER: Peer(Bob): Sending CommitSig
	// 2022-11-14 06:24:01.784 PEER: Peer(Bob): Sending RevokeAndAck
	// 2022-11-14 06:24:08.464 PEER: Peer(Bob): Received UpdateFulfillHTLC
	//
	// 7 seconds is too long for a local test and this needs more
	// investigation.
	payReqs := []string{resp.PaymentRequest}
	ht.CompletePaymentRequests(alice, payReqs)

	// Check that Alice did make the payment with two HTLCs, one failed and
	// one succeeded.
	payment := ht.AssertNumPayments(alice, 1)[0]

	htlcs := payment.Htlcs
	require.Equal(ht, 2, len(htlcs), "expected to have 2 HTLCs")
	require.Equal(ht, lnrpc.HTLCAttempt_FAILED, htlcs[0].Status,
		"the first HTLC attempt should fail")
	require.Equal(ht, lnrpc.HTLCAttempt_SUCCEEDED, htlcs[1].Status,
		"the second HTLC attempt should succeed")

	// Carol should have received 20k satoshis from Bob.
	ht.AssertAmountPaid("Carol(remote) [<=private] Bob(local)",
		carol, chanPointBobCarol, 0, paymentAmt)

	// Bob should have sent 20k satoshis to Carol.
	ht.AssertAmountPaid("Bob(local) [private=>] Carol(remote)",
		bob, chanPointBobCarol, paymentAmt, 0)

	// Calculate the amount in satoshis.
	amtExpected := int64(paymentAmt + baseFeeMSat/1000)

	// Bob should have received 20k satoshis + fee from Alice.
	ht.AssertAmountPaid("Bob(remote) <= Alice(local)",
		bob, chanPointAliceBob, 0, amtExpected)

	// Alice should have sent 20k satoshis + fee to Bob.
	ht.AssertAmountPaid("Alice(local) => Bob(remote)",
		alice, chanPointAliceBob, amtExpected, 0)
}

// testUpdateChannelPolicyFeeRateAccuracy tests that updating the channel policy
// rounds fee rate values correctly as well as setting fee rate with ppm works
// as expected.
func testUpdateChannelPolicyFeeRateAccuracy(ht *lntest.HarnessTest) {
	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	// Create a channel Alice -> Bob.
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	alice := nodes[0]
	chanPoint := chanPoints[0]

	baseFee := int64(1500)
	timeLockDelta := uint32(66)
	maxHtlc := uint64(500000)
	defaultMinHtlc := int64(1000)

	// Originally LND did not properly round up fee rates which caused
	// inaccuracy where fee rates were simply rounded down due to the
	// integer conversion.
	//
	// We'll use a fee rate of 0.031337 which without rounding up would
	// have resulted in a fee rate ppm of 31336.
	feeRate := 0.031337

	// Expected fee rate will be rounded up.
	expectedFeeRateMilliMsat := int64(math.Round(testFeeBase * feeRate))

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: expectedFeeRateMilliMsat,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      maxHtlc,
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       feeRate,
		TimeLockDelta: timeLockDelta,
		MaxHtlcMsat:   maxHtlc,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}
	alice.RPC.UpdateChannelPolicy(req)

	// Make sure that both Alice and Bob sees the same policy after update.
	assertNodesPolicyUpdate(ht, nodes, alice, expectedPolicy, chanPoint)

	// Now use the new PPM feerate field and make sure that the feerate is
	// correctly set.
	feeRatePPM := uint32(32337)
	req.FeeRate = 0 // Can't set both at the same time.
	req.FeeRatePpm = feeRatePPM
	expectedPolicy.FeeRateMilliMsat = int64(feeRatePPM)

	alice.RPC.UpdateChannelPolicy(req)

	// Make sure that both Alice and Bob sees the same policy after update.
	assertNodesPolicyUpdate(ht, nodes, alice, expectedPolicy, chanPoint)
}

// assertNodesPolicyUpdate checks that a given policy update has been received
// by a list of given nodes.
func assertNodesPolicyUpdate(ht *lntest.HarnessTest, nodes []*node.HarnessNode,
	advertisingNode *node.HarnessNode, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint) {

	for _, node := range nodes {
		ht.AssertChannelPolicyUpdate(
			node, advertisingNode, policy, chanPoint, false,
		)
	}
}
