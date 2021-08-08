package itest

import (
	"context"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testUpdateChannelPolicy tests that policy updates made to a channel
// gets propagated to other nodes in the network.
func testUpdateChannelPolicy(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		defaultFeeBase       = 1000
		defaultFeeRate       = 1
		defaultTimeLockDelta = chainreg.DefaultBitcoinTimeLockDelta
		defaultMinHtlc       = 1000
	)
	defaultMaxHtlc := calculateMaxHtlc(funding.MaxBtcFundingAmount)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	// Create a channel Alice->Bob.
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, chanPoint, false)

	// We add all the nodes' update channels to a slice, such that we can
	// make sure they all receive the expected updates.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob}

	// assertPolicyUpdate checks that a given policy update has been
	// received by a list of given nodes.
	assertPolicyUpdate := func(nodes []*lntest.HarnessNode,
		advertisingNode string, policy *lnrpc.RoutingPolicy,
		chanPoint *lnrpc.ChannelPoint) {

		for _, node := range nodes {
			assertChannelPolicyUpdate(
				t.t, node, advertisingNode,
				policy, chanPoint, false,
			)
		}

	}

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
		nodes, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertPolicyUpdate(nodes, net.Bob.PubKeyStr, expectedPolicy, chanPoint)

	// They should now know about the default policies.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Create Carol with options to rate limit channel updates up to 2 per
	// day, and create a new channel Bob->Carol.
	carol := net.NewNode(
		t.t, "Carol", []string{
			"--gossip.max-channel-update-burst=2",
			"--gossip.channel-update-interval=24h",
		},
	)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	nodes = append(nodes, carol)

	// Send some coins to Carol that can be used for channel funding.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	net.ConnectNodes(t.t, carol, net.Bob)

	// Open the channel Carol->Bob with a custom min_htlc value set. Since
	// Carol is opening the channel, she will require Bob to not forward
	// HTLCs smaller than this value, and hence he should advertise it as
	// part of his ChannelUpdate.
	const customMinHtlc = 5000
	chanPoint2 := openChannelAndAssert(
		t, net, carol, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
			MinHtlc: customMinHtlc,
		},
	)
	defer closeChannelAndAssert(t, net, net.Bob, chanPoint2, false)

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
		nodes, net.Bob.PubKeyStr, expectedPolicyBob, chanPoint2,
	)
	assertPolicyUpdate(
		nodes, carol.PubKeyStr, expectedPolicyCarol, chanPoint2,
	)

	// Check that all nodes now know about the updated policies.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicyBob,
			chanPoint2,
		)
		assertChannelPolicy(
			t, node, carol.PubKeyStr, expectedPolicyCarol,
			chanPoint2,
		)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("carol didn't report channel: %v", err)
	}

	// First we'll try to send a payment from Alice to Carol with an amount
	// less than the min_htlc value required by Carol. This payment should
	// fail, as the channel Bob->Carol cannot carry HTLCs this small.
	payAmt := btcutil.Amount(4)
	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	err = completePaymentRequests(
		net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)

	// Alice knows about the channel policy of Carol and should therefore
	// not be able to find a path during routing.
	expErr := lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE
	if err.Error() != expErr.String() {
		t.Fatalf("expected %v, instead got %v", expErr, err)
	}

	// Now we try to send a payment over the channel with a value too low
	// to be accepted. First we query for a route to route a payment of
	// 5000 mSAT, as this is accepted.
	payAmt = btcutil.Amount(5)
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            int64(payAmt),
		FinalCltvDelta: defaultTimeLockDelta,
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	routes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	if len(routes.Routes) != 1 {
		t.Fatalf("expected to find 1 route, got %v", len(routes.Routes))
	}

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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt) // nolint:staticcheck
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       routes.Routes[0],
	}

	err = alicePayStream.Send(sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We expect this payment to fail, and that the min_htlc value is
	// communicated back to us, since the attempted HTLC value was too low.
	sendResp, err := alicePayStream.Recv()
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Expected as part of the error message.
	substrs := []string{
		"AmountBelowMinimum",
		"HtlcMinimumMsat: (lnwire.MilliSatoshi) 5000 mSAT",
	}
	for _, s := range substrs {
		if !strings.Contains(sendResp.PaymentError, s) {
			t.Fatalf("expected error to contain \"%v\", instead "+
				"got %v", s, sendResp.PaymentError)
		}
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
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	sendResp, err = alicePayStream.Recv()
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	if sendResp.PaymentError != "" {
		t.Fatalf("expected payment to succeed, instead got %v",
			sendResp.PaymentError)
	}

	// With our little cluster set up, we'll update the fees and the max htlc
	// size for the Bob side of the Alice->Bob channel, and make sure
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

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if _, err := net.Bob.UpdateChannelPolicy(ctxt, req); err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// Wait for all nodes to have seen the policy update done by Bob.
	assertPolicyUpdate(nodes, net.Bob.PubKeyStr, expectedPolicy, chanPoint)

	// Check that all nodes now know about Bob's updated policy.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err = carol.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	err = completePaymentRequests(
		net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We'll now open a channel from Alice directly to Carol.
	net.ConnectNodes(t.t, net.Alice, carol)
	chanPoint3 := openChannelAndAssert(
		t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, chanPoint3, false)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

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

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.UpdateChannelPolicy(ctxt, req)
	if err != nil {
		t.Fatalf("unable to update alice's channel policy: %v", err)
	}

	// Wait for all nodes to have seen the policy updates for both of
	// Alice's channels.
	assertPolicyUpdate(
		nodes, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertPolicyUpdate(
		nodes, net.Alice.PubKeyStr, expectedPolicy, chanPoint3,
	)

	// And finally check that all nodes remembers the policy update they
	// received.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Alice.PubKeyStr, expectedPolicy,
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

		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		_, err = net.Alice.UpdateChannelPolicy(ctxt, req)
		require.NoError(t.t, err)

		// Wait for all nodes to have seen the policy updates for both
		// of Alice's channels. Carol will not see the last update as
		// the limit has been reached.
		assertPolicyUpdate(
			[]*lntest.HarnessNode{net.Alice, net.Bob},
			net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertPolicyUpdate(
			[]*lntest.HarnessNode{net.Alice, net.Bob},
			net.Alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
		// Check that all nodes remembers the policy update
		// they received.
		assertChannelPolicy(
			t, net.Alice, net.Alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)
		assertChannelPolicy(
			t, net.Bob, net.Alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)

		// Carol was added last, which is why we check the last index.
		// Since Carol didn't receive the last update, she still has
		// Alice's old policy.
		if i == numUpdatesTilRateLimit-1 {
			expectedPolicy = &prevAlicePolicy
		}
		assertPolicyUpdate(
			[]*lntest.HarnessNode{carol},
			net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertPolicyUpdate(
			[]*lntest.HarnessNode{carol},
			net.Alice.PubKeyStr, expectedPolicy, chanPoint3,
		)
		assertChannelPolicy(
			t, carol, net.Alice.PubKeyStr,
			expectedPolicy, chanPoint, chanPoint3,
		)
	}
}

// testSendUpdateDisableChannel ensures that a channel update with the disable
// flag set is sent once a channel has been either unilaterally or cooperatively
// closed.
func testSendUpdateDisableChannel(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt = 100000
	)

	// Open a channel between Alice and Bob and Alice and Carol. These will
	// be closed later on in order to trigger channel update messages
	// marking the channels as disabled.
	chanPointAliceBob := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	carol := net.NewNode(
		t.t, "Carol", []string{
			"--minbackoff=10s",
			"--chan-enable-timeout=1.5s",
			"--chan-disable-timeout=3s",
			"--chan-status-sample-interval=.5s",
		})
	defer shutdownAndAssert(net, t, carol)

	net.ConnectNodes(t.t, net.Alice, carol)
	chanPointAliceCarol := openChannelAndAssert(
		t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// We create a new node Eve that has an inactive channel timeout of
	// just 2 seconds (down from the default 20m). It will be used to test
	// channel updates for channels going inactive.
	eve := net.NewNode(
		t.t, "Eve", []string{
			"--minbackoff=10s",
			"--chan-enable-timeout=1.5s",
			"--chan-disable-timeout=3s",
			"--chan-status-sample-interval=.5s",
		})
	defer shutdownAndAssert(net, t, eve)

	// Give Eve some coins.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, eve)

	// Connect Eve to Carol and Bob, and open a channel to carol.
	net.ConnectNodes(t.t, eve, carol)
	net.ConnectNodes(t.t, eve, net.Bob)

	chanPointEveCarol := openChannelAndAssert(
		t, net, eve, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Launch a node for Dave which will connect to Bob in order to receive
	// graph updates from. This will ensure that the channel updates are
	// propagated throughout the network.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	net.ConnectNodes(t.t, net.Bob, dave)

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

		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()

		require.NoError(
			t.t, dave.WaitForChannelPolicyUpdate(
				ctxt, node.PubKeyStr, policy, chanPoint, false,
			), "error while waiting for channel update",
		)
	}

	// Let Carol go offline. Since Eve has an inactive timeout of 2s, we
	// expect her to send an update disabling the channel.
	restartCarol, err := net.SuspendNode(carol)
	if err != nil {
		t.Fatalf("unable to suspend carol: %v", err)
	}

	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	// We restart Carol. Since the channel now becomes active again, Eve
	// should send a ChannelUpdate setting the channel no longer disabled.
	if err := restartCarol(); err != nil {
		t.Fatalf("unable to restart carol: %v", err)
	}

	expectedPolicy.Disabled = false
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	// Now we'll test a long disconnection. Disconnect Carol and Eve and
	// ensure they both detect each other as disabled. Their min backoffs
	// are high enough to not interfere with disabling logic.
	if err := net.DisconnectNodes(carol, eve); err != nil {
		t.Fatalf("unable to disconnect Carol from Eve: %v", err)
	}

	// Wait for a disable from both Carol and Eve to come through.
	expectedPolicy.Disabled = true
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Reconnect Carol and Eve, this should cause them to reenable the
	// channel from both ends after a short delay.
	net.EnsureConnected(t.t, carol, eve)

	expectedPolicy.Disabled = false
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Now we'll test a short disconnection. Disconnect Carol and Eve, then
	// reconnect them after one second so that their scheduled disables are
	// aborted. One second is twice the status sample interval, so this
	// should allow for the disconnect to be detected, but still leave time
	// to cancel the announcement before the 3 second inactive timeout is
	// hit.
	if err := net.DisconnectNodes(carol, eve); err != nil {
		t.Fatalf("unable to disconnect Carol from Eve: %v", err)
	}
	time.Sleep(time.Second)
	net.EnsureConnected(t.t, eve, carol)

	// Since the disable should have been canceled by both Carol and Eve, we
	// expect no channel updates to appear on the network, which means we
	// expect the polices stay unchanged(Disable == false).
	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)
	assertPolicyUpdate(carol, expectedPolicy, chanPointEveCarol)

	// Close Alice's channels with Bob and Carol cooperatively and
	// unilaterally respectively.
	_, _, err = net.CloseChannel(net.Alice, chanPointAliceBob, false)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	_, _, err = net.CloseChannel(net.Alice, chanPointAliceCarol, true)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Now that the channel close processes have been started, we should
	// receive an update marking each as disabled.
	expectedPolicy.Disabled = true
	assertPolicyUpdate(net.Alice, expectedPolicy, chanPointAliceBob)
	assertPolicyUpdate(net.Alice, expectedPolicy, chanPointAliceCarol)

	// Finally, close the channels by mining the closing transactions.
	mineBlocks(t, net, 1, 2)

	// Also do this check for Eve's channel with Carol.
	_, _, err = net.CloseChannel(eve, chanPointEveCarol, false)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	assertPolicyUpdate(eve, expectedPolicy, chanPointEveCarol)

	mineBlocks(t, net, 1, 1)

	// And finally, clean up the force closed channel by mining the
	// sweeping transaction.
	cleanupForceClose(t, net, net.Alice, chanPointAliceCarol)
}

// testUpdateChannelPolicyForPrivateChannel tests when a private channel
// updates its channel edge policy, we will use the updated policy to send our
// payment.
// The topology is created as: Alice -> Bob -> Carol, where Alice -> Bob is
// public and Bob -> Carol is private. After an invoice is created by Carol,
// Bob will update the base fee via UpdateChannelPolicy, we will test that
// Alice will not fail the payment and send it using the updated channel
// policy.
func testUpdateChannelPolicyForPrivateChannel(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	defer ctxb.Done()

	// We'll create the following topology first,
	// Alice <--public:100k--> Bob <--private:100k--> Carol
	const chanAmt = btcutil.Amount(100000)

	// Open a channel with 100k satoshis between Alice and Bob.
	chanPointAliceBob := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, chanPointAliceBob, false)

	// Get Alice's funding point.
	aliceChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointAliceBob)
	require.NoError(t.t, err, "unable to get txid")
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAliceBob.OutputIndex,
	}

	// Create a new node Carol.
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	// Connect Carol to Bob.
	net.ConnectNodes(t.t, carol, net.Bob)

	// Open a channel with 100k satoshis between Bob and Carol.
	chanPointBobCarol := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)
	defer closeChannelAndAssert(t, net, net.Bob, chanPointBobCarol, false)

	// Get Bob's funding point.
	bobChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointBobCarol)
	require.NoError(t.t, err, "unable to get txid")
	bobFundPoint := wire.OutPoint{
		Hash:  *bobChanTXID,
		Index: chanPointBobCarol.OutputIndex,
	}

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
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err, "unable to create invoice for carol")

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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Bob.UpdateChannelPolicy(ctxt, updateFeeReq)
	require.NoError(t.t, err, "unable to update chan policy")

	// Alice pays the invoices. She will use the updated baseFeeMSat in the
	// payment
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	payReqs := []string{resp.PaymentRequest}
	require.NoError(t.t,
		completePaymentRequests(
			net.Alice, net.Alice.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// Check that Alice did make the payment with two HTLCs, one failed and
	// one succeeded.
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(
		ctxt, &lnrpc.ListPaymentsRequest{},
	)
	require.NoError(t.t, err, "failed to obtain payments for Alice")
	require.Equal(t.t, 1, len(paymentsResp.Payments), "expected 1 payment")

	htlcs := paymentsResp.Payments[0].Htlcs
	require.Equal(t.t, 2, len(htlcs), "expected to have 2 HTLCs")
	require.Equal(
		t.t, lnrpc.HTLCAttempt_FAILED, htlcs[0].Status,
		"the first HTLC attempt should fail",
	)
	require.Equal(
		t.t, lnrpc.HTLCAttempt_SUCCEEDED, htlcs[1].Status,
		"the second HTLC attempt should succeed",
	)

	// Carol should have received 20k satoshis from Bob.
	assertAmountPaid(t, "Carol(remote) [<=private] Bob(local)",
		carol, bobFundPoint, 0, paymentAmt)

	// Bob should have sent 20k satoshis to Carol.
	assertAmountPaid(t, "Bob(local) [private=>] Carol(remote)",
		net.Bob, bobFundPoint, paymentAmt, 0)

	// Calcuate the amount in satoshis.
	amtExpected := int64(paymentAmt + baseFeeMSat/1000)

	// Bob should have received 20k satoshis + fee from Alice.
	assertAmountPaid(t, "Bob(remote) <= Alice(local)",
		net.Bob, aliceFundPoint, 0, amtExpected)

	// Alice should have sent 20k satoshis + fee to Bob.
	assertAmountPaid(t, "Alice(local) => Bob(remote)",
		net.Alice, aliceFundPoint, amtExpected, 0)
}
