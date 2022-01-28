package itest

import (
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
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
	defaultMaxHtlc := calculateMaxHtlc(funding.MaxBtcFundingAmount)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	alice, bob := ht.Alice(), ht.Bob()

	// Create a channel Alice->Bob.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	defer ht.CloseChannel(alice, chanPoint, false)

	// We add all the nodes' update channels to a slice, such that we can
	// make sure they all receive the expected updates.
	nodes := []*lntest.HarnessNode{alice, bob}

	// Alice and Bob should see each other's ChannelUpdates, advertising the
	// default routing policies.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultFeeBase,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}

	assertPolicyUpdate(
		ht, nodes, alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertPolicyUpdate(
		ht, nodes, bob.PubKeyStr, expectedPolicy, chanPoint,
	)

	// They should now know about the default policies.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		ht.AssertChannelPolicy(
			node, bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	ht.AssertChannelOpen(alice, chanPoint)
	ht.AssertChannelOpen(bob, chanPoint)

	// Create Carol with options to rate limit channel updates up to 2 per
	// day, and create a new channel Bob->Carol.
	carol := ht.NewNode(
		"Carol", []string{
			"--gossip.max-channel-update-burst=2",
			"--gossip.channel-update-interval=24h",
		},
	)
	// Clean up carol's node when the test finishes.
	defer ht.Shutdown(carol)

	nodes = append(nodes, carol)

	// Send some coins to Carol that can be used for channel funding.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

	ht.ConnectNodes(carol, bob)

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
	defer ht.CloseChannel(bob, chanPoint2, false)

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

	assertPolicyUpdate(
		ht, nodes, bob.PubKeyStr, expectedPolicyBob, chanPoint2,
	)
	assertPolicyUpdate(
		ht, nodes, carol.PubKeyStr, expectedPolicyCarol, chanPoint2,
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

	ht.AssertChannelOpen(alice, chanPoint2)
	ht.AssertChannelOpen(bob, chanPoint2)
	ht.AssertChannelOpen(carol, chanPoint2)

	// First we'll try to send a payment from Alice to Carol with an amount
	// less than the min_htlc value required by Carol. This payment should
	// fail, as the channel Bob->Carol cannot carry HTLCs this small.
	payAmt := btcutil.Amount(4)
	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, carol)

	// TODO(yy): refactor completePaymentRequests
	err := completePaymentRequests(
		alice, alice.RouterClient, []string{resp.PaymentRequest}, true,
	)

	// Alice knows about the channel policy of Carol and should therefore
	// not be able to find a path during routing.
	expErr := lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE
	require.EqualError(ht, err, expErr.String())

	// Now we try to send a payment over the channel with a value too low
	// to be accepted. First we query for a route to route a payment of
	// 5000 mSAT, as this is accepted.
	payAmt = btcutil.Amount(5)
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            int64(payAmt),
		FinalCltvDelta: defaultTimeLockDelta,
	}
	routes := ht.QueryRoutes(alice, routesReq)
	require.Len(ht, routes.Routes, 1)

	// We change the route to carry a payment of 4000 mSAT instead of 5000
	// mSAT.
	payAmt = btcutil.Amount(4)
	amtSat := int64(payAmt)
	amtMSat := int64(lnwire.NewMSatFromSatoshis(payAmt))
	routes.Routes[0].Hops[0].AmtToForward = amtSat // nolint:staticcheck
	routes.Routes[0].Hops[0].AmtToForwardMsat = amtMSat
	routes.Routes[0].Hops[1].AmtToForward = amtSat // nolint:staticcheck
	routes.Routes[0].Hops[1].AmtToForwardMsat = amtMSat

	// Send the payment with the modified value.
	alicePayStream := ht.SendToRoute(alice) // nolint:staticcheck

	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       routes.Routes[0],
	}
	err = alicePayStream.Send(sendReq)
	require.NoError(ht, err, "unable to send payment")

	// We expect this payment to fail, and that the min_htlc value is
	// communicated back to us, since the attempted HTLC value was too low.
	sendResp, err := alicePayStream.Recv()
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
	routes.Routes[0].Hops[0].AmtToForward = amtSat // nolint:staticcheck
	routes.Routes[0].Hops[0].AmtToForwardMsat = amtMSat
	routes.Routes[0].Hops[1].AmtToForward = amtSat // nolint:staticcheck
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

	sendResp, err = alicePayStream.Recv()
	require.NoError(ht, err, "unable to receive payment stream")
	require.Empty(ht, sendResp.PaymentError, "expected payment to succeed")

	// With our little cluster set up, we'll update the fees and the max
	// htlc size for the Bob side of the Alice->Bob channel, and make sure
	// all nodes learn about it.
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
	ht.UpdateChannelPolicy(bob, req)

	// Wait for all nodes to have seen the policy update done by Bob.
	assertPolicyUpdate(ht, nodes, bob.PubKeyStr, expectedPolicy, chanPoint)

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
	resp = ht.AddInvoice(invoice, carol)

	// TODO(yy): refactor
	err = completePaymentRequests(
		alice, alice.RouterClient, []string{resp.PaymentRequest}, true,
	)
	require.NoError(ht, err, "unable to complete payment")

	// We'll now open a channel from Alice directly to Carol.
	ht.ConnectNodes(alice, carol)
	chanPoint3 := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	defer ht.CloseChannel(alice, chanPoint3, false)

	ht.AssertChannelOpen(alice, chanPoint3)
	ht.AssertChannelOpen(carol, chanPoint3)

	// Make a global update, and check that both channels' new policies get
	// propagated.
	baseFee = int64(800)
	feeRate = int64(123)
	timeLockDelta = uint32(22)
	maxHtlc *= 2

	expectedPolicy.FeeBaseMsat = baseFee
	expectedPolicy.FeeRateMilliMsat = testFeeBase * feeRate
	expectedPolicy.TimeLockDelta = timeLockDelta
	expectedPolicy.MaxHtlcMsat = maxHtlc

	req = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMsat:   maxHtlc,
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_Global{}

	ht.UpdateChannelPolicy(alice, req)

	// Wait for all nodes to have seen the policy updates for both of
	// Alice's channels.
	assertPolicyUpdate(
		ht, nodes, alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertPolicyUpdate(
		ht, nodes, alice.PubKeyStr, expectedPolicy, chanPoint3,
	)

	// And finally check that all nodes remembers the policy update they
	// received.
	for _, node := range nodes {
		ht.AssertChannelPolicy(
			node, alice.PubKeyStr, expectedPolicy,
			chanPoint, chanPoint3,
		)
	}

	// Now, to test that Carol is properly rate limiting incoming updates,
	// we'll send two more update from Alice. Carol should accept the first,
	// but not the second, as she only allows two updates per day and a day
	// has yet to elapse from the previous update.
	const numUpdatesTilRateLimit = 2
	for i := 0; i < numUpdatesTilRateLimit; i++ {
		prevAlicePolicy := *expectedPolicy
		baseFee *= 2
		expectedPolicy.FeeBaseMsat = baseFee
		req.BaseFeeMsat = baseFee

		ht.UpdateChannelPolicy(alice, req)

		// Wait for all nodes to have seen the policy updates for both
		// of Alice's channels. Carol will not see the last update as
		// the limit has been reached.
		assertPolicyUpdate(
			ht, []*lntest.HarnessNode{alice, bob},
			alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertPolicyUpdate(
			ht, []*lntest.HarnessNode{alice, bob},
			alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
		// Check that all nodes remembers the policy update
		// they received.
		ht.AssertChannelPolicy(
			alice, alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)
		ht.AssertChannelPolicy(
			bob, alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)

		// Carol was added last, which is why we check the last index.
		// Since Carol didn't receive the last update, she still has
		// Alice's old policy.
		if i == numUpdatesTilRateLimit-1 {
			expectedPolicy = &prevAlicePolicy
		}
		assertPolicyUpdate(
			ht, []*lntest.HarnessNode{carol},
			alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertPolicyUpdate(
			ht, []*lntest.HarnessNode{carol},
			alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
		ht.AssertChannelPolicy(
			carol, alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)
	}
}

// testSendUpdateDisableChannel ensures that a channel update with the disable
// flag set is sent once a channel has been either unilaterally or cooperatively
// closed.
func testSendUpdateDisableChannel(ht *lntest.HarnessTest) {
	const (
		chanAmt = 100000
	)

	nodeCfg := []string{
		"--minbackoff=10s",
		"--chan-enable-timeout=1.5s",
		"--chan-disable-timeout=3s",
		"--chan-status-sample-interval=.5s",
	}
	alice, bob := ht.Alice(), ht.Bob()

	carol := ht.NewNode("Carol", nodeCfg)
	defer ht.Shutdown(carol)

	ht.ConnectNodes(alice, carol)

	// Open a channel between Alice and Bob and Alice and Carol. These will
	// be closed later on in order to trigger channel update messages
	// marking the channels as disabled.
	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	chanPointAliceCarol := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// We create a new node Eve that has an inactive channel timeout of
	// just 2 seconds (down from the default 20m). It will be used to test
	// channel updates for channels going inactive.
	eve := ht.NewNode("Eve", nodeCfg)
	defer ht.Shutdown(eve)

	// Give Eve some coins.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, eve)

	// Connect Eve to Carol and Bob, and open a channel to carol.
	ht.ConnectNodes(eve, carol)
	ht.ConnectNodes(eve, bob)

	chanPointEveCarol := ht.OpenChannel(
		eve, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Launch a node for Dave which will connect to Bob in order to receive
	// graph updates from. This will ensure that the channel updates are
	// propagated throughout the network.
	dave := ht.NewNode("Dave", nil)
	defer ht.Shutdown(dave)

	ht.ConnectNodes(bob, dave)

	// We should expect to see a channel update with the default routing
	// policy, except that it should indicate the channel is disabled.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(chainreg.DefaultBitcoinBaseFeeMSat),
		FeeRateMilliMsat: int64(chainreg.DefaultBitcoinFeeRate),
		TimeLockDelta:    chainreg.DefaultBitcoinTimeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
		Disabled:         true,
	}

	// assertPolicyUpdate checks that the required policy update has
	// happened on the given node.
	assertPolicyUpdate := func(node *lntest.HarnessNode,
		policy *lnrpc.RoutingPolicy, chanPoint *lnrpc.ChannelPoint) {

		require.NoError(ht, dave.WaitForChannelPolicyUpdate(
			node.PubKeyStr, policy, chanPoint, false,
		), "error while waiting for channel update")
	}

	// Let Carol go offline. Since Eve has an inactive timeout of 2s, we
	// expect her to send an update disabling the channel.
	restartCarol := ht.SuspendNode(carol)
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	// We restart Carol. Since the channel now becomes active again, Eve
	// should send a ChannelUpdate setting the channel no longer disabled.
	require.NoError(ht, restartCarol(), "unable to restart carol")

	expectedPolicy.Disabled = false
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	// Wait until Carol and Eve are reconnected before we disconnect them
	// again.
	ht.EnsureConnected(eve, carol)

	// Now we'll test a long disconnection. Disconnect Carol and Eve and
	// ensure they both detect each other as disabled. Their min backoffs
	// are high enough to not interfere with disabling logic.
	ht.DisconnectNodes(carol, eve)

	// Wait for a disable from both Carol and Eve to come through.
	expectedPolicy.Disabled = true
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Reconnect Carol and Eve, this should cause them to reenable the
	// channel from both ends after a short delay.
	ht.EnsureConnected(carol, eve)

	expectedPolicy.Disabled = false
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Now we'll test a short disconnection. Disconnect Carol and Eve, then
	// reconnect them after one second so that their scheduled disables are
	// aborted. One second is twice the status sample interval, so this
	// should allow for the disconnect to be detected, but still leave time
	// to cancel the announcement before the 3 second inactive timeout is
	// hit.
	ht.DisconnectNodes(carol, eve)
	time.Sleep(time.Second)
	ht.EnsureConnected(eve, carol)

	// Since the disable should have been canceled by both Carol and Eve,
	// we expect no channel updates to appear on the network, which means
	// we expect the polices stay unchanged(Disable == false).
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Close Alice's channels with Bob and Carol cooperatively and
	// unilaterally respectively. Note that the CloseChannel will mine a
	// block and check that the closing transaction can be found in both
	// the mempool and the block.
	ht.CloseChannel(alice, chanPointAliceBob, false)
	ht.CloseChannel(alice, chanPointAliceCarol, true)

	// Now that the channel close processes have been started, we should
	// receive an update marking each as disabled.
	expectedPolicy.Disabled = true
	assertPolicyUpdate(alice, expectedPolicy, chanPointAliceBob)
	assertPolicyUpdate(alice, expectedPolicy, chanPointAliceCarol)

	// Also do this check for Eve's channel with Carol.
	ht.CloseChannel(eve, chanPointEveCarol, false)
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	// And finally, clean up the force closed channel by mining the
	// sweeping transaction.
	ht.CleanupForceClose(alice, chanPointAliceCarol)
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
	// We'll create the following topology first,
	// Alice <--public:100k--> Bob <--private:100k--> Carol
	const chanAmt = btcutil.Amount(100000)

	alice, bob := ht.Alice(), ht.Bob()

	// Open a channel with 100k satoshis between Alice and Bob.
	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	defer ht.CloseChannel(alice, chanPointAliceBob, false)

	// Create a new node Carol.
	carol := ht.NewNode("Carol", nil)
	defer ht.Shutdown(carol)

	// Connect Carol to Bob.
	ht.ConnectNodes(carol, bob)

	// Open a channel with 100k satoshis between Bob and Carol.
	chanPointBobCarol := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)
	defer ht.CloseChannel(bob, chanPointBobCarol, false)

	// We should have the following topology now,
	// Alice <--public:100k--> Bob <--private:100k--> Carol
	//
	// Now we will create an invoice for Carol.
	const paymentAmt = 20000
	invoice := &lnrpc.Invoice{
		Memo:    "routing hints",
		Value:   paymentAmt,
		Private: true,
	}
	resp := ht.AddInvoice(invoice, carol)

	// Bob now updates the channel edge policy for the private channel.
	const (
		baseFeeMSat = 33000
	)
	timeLockDelta := uint32(chainreg.DefaultBitcoinTimeLockDelta)
	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFeeMSat,
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointBobCarol,
		},
	}
	ht.UpdateChannelPolicy(bob, updateFeeReq)

	// Alice pays the invoices. She will use the updated baseFeeMSat in the
	// payment
	payReqs := []string{resp.PaymentRequest}
	require.NoError(ht,
		// TODO(yy): refactor
		completePaymentRequests(
			alice, alice.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// Check that Alice did make the payment with two HTLCs, one failed and
	// one succeeded.
	paymentsResp := ht.ListPayments(alice, false)
	require.Equal(ht, 1, len(paymentsResp.Payments), "expected 1 payment")

	htlcs := paymentsResp.Payments[0].Htlcs
	require.Equal(ht, 2, len(htlcs), "expected to have 2 HTLCs")
	require.Equal(
		ht, lnrpc.HTLCAttempt_FAILED, htlcs[0].Status,
		"the first HTLC attempt should fail",
	)
	require.Equal(
		ht, lnrpc.HTLCAttempt_SUCCEEDED, htlcs[1].Status,
		"the second HTLC attempt should succeed",
	)

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
	alice, bob := ht.Alice(), ht.Bob()
	chanPoint := ht.OpenChannel(
		alice, bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	defer ht.CloseChannel(alice, chanPoint, false)

	// Nodes that we need to make sure receive the channel updates.
	nodes := []*lntest.HarnessNode{alice, bob}

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
	ht.UpdateChannelPolicy(alice, req)

	// Make sure that both Alice and Bob sees the same policy after update.
	assertPolicyUpdate(
		ht, nodes, alice.PubKeyStr, expectedPolicy, chanPoint,
	)

	// Now use the new PPM feerate field and make sure that the feerate is
	// correctly set.
	feeRatePPM := uint32(32337)
	req.FeeRate = 0 // Can't set both at the same time.
	req.FeeRatePpm = feeRatePPM
	expectedPolicy.FeeRateMilliMsat = int64(feeRatePPM)

	ht.UpdateChannelPolicy(alice, req)

	// Make sure that both Alice and Bob sees the same policy after update.
	assertPolicyUpdate(
		ht, nodes, alice.PubKeyStr, expectedPolicy, chanPoint,
	)
}

// assertPolicyUpdate checks that a given policy update has been received by a
// list of given nodes.
func assertPolicyUpdate(ht *lntest.HarnessTest, nodes []*lntest.HarnessNode,
	advertisingNode string, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint) {

	for _, node := range nodes {
		ht.AssertChannelPolicyUpdate(
			node, advertisingNode, policy, chanPoint, false,
		)
	}
}
