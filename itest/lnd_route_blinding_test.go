package itest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testQueryBlindedRoutes tests querying routes to blinded routes. To do this,
// it sets up a nework of Alice - Bob - Carol and creates a mock blinded route
// that uses Carol as the introduction node (plus dummy hops to cover multiple
// hops). The test simply asserts that the structure of the route is as
// expected. It also includes the edge case of a single-hop blinded route,
// which indicates that the introduction node is the recipient.
func testQueryBlindedRoutes(ht *lntest.HarnessTest) {
	var (
		// Convenience aliases.
		alice = ht.Alice
		bob   = ht.Bob
	)

	// Setup a two hop channel network: Alice -- Bob -- Carol.
	// We set our proportional fee for these channels to zero, so that
	// our calculations are easier. This is okay, because we're not testing
	// the basic mechanics of pathfinding in this test.
	chanAmt := btcutil.Amount(100000)
	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:        chanAmt,
			BaseFee:    10000,
			FeeRate:    0,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	)

	carol := ht.NewNode("Carol", nil)
	ht.EnsureConnected(bob, carol)

	var bobCarolBase uint64 = 2000
	chanPointBobCarol := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt:        chanAmt,
			BaseFee:    bobCarolBase,
			FeeRate:    0,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	)

	// Wait for Alice to see Bob/Carol's channel because she'll need it for
	// pathfinding.
	ht.AssertTopologyChannelOpen(alice, chanPointBobCarol)

	// Lookup full channel info so that we have channel ids for our route.
	aliceBobChan := ht.GetChannelByChanPoint(alice, chanPointAliceBob)
	bobCarolChan := ht.GetChannelByChanPoint(bob, chanPointBobCarol)

	// Sanity check that bob's fee is as expected.
	chanInfoReq := &lnrpc.ChanInfoRequest{
		ChanId: bobCarolChan.ChanId,
	}

	bobCarolInfo := bob.RPC.GetChanInfo(chanInfoReq)

	// Our test relies on knowing the fee rate for bob - carol to set the
	// fees we expect for our route. Perform a quick sanity check that our
	// policy is as expected.
	var policy *lnrpc.RoutingPolicy
	if bobCarolInfo.Node1Pub == bob.PubKeyStr {
		policy = bobCarolInfo.Node1Policy
	} else {
		policy = bobCarolInfo.Node2Policy
	}
	require.Equal(ht, bobCarolBase, uint64(policy.FeeBaseMsat), "base fee")
	require.EqualValues(ht, 0, policy.FeeRateMilliMsat, "fee rate")

	// We'll also need the current block height to calculate our locktimes.
	info := alice.RPC.GetInfo()

	// Since we created channels with default parameters, we can assume
	// that all of our channels have the default cltv delta.
	bobCarolDelta := uint32(chainreg.DefaultBitcoinTimeLockDelta)

	// Create arbitrary pubkeys for use in our blinded route. They're not
	// actually used functionally in this test, so we can just make them up.
	var (
		_, blindingPoint = btcec.PrivKeyFromBytes([]byte{1})
		_, carolBlinded  = btcec.PrivKeyFromBytes([]byte{2})
		_, blindedHop1   = btcec.PrivKeyFromBytes([]byte{3})
		_, blindedHop2   = btcec.PrivKeyFromBytes([]byte{4})

		encryptedDataCarol = []byte{1, 2, 3}
		encryptedData1     = []byte{4, 5, 6}
		encryptedData2     = []byte{7, 8, 9}

		blindingBytes     = blindingPoint.SerializeCompressed()
		carolBlindedBytes = carolBlinded.SerializeCompressed()
		blinded1Bytes     = blindedHop1.SerializeCompressed()
		blinded2Bytes     = blindedHop2.SerializeCompressed()
	)

	// Now we create a blinded route which uses carol as an introduction
	// node followed by two dummy hops (the arbitrary pubkeys in our
	// blinded route above:
	// Carol --- B1 --- B2
	route := &lnrpc.BlindedPath{
		IntroductionNode: carol.PubKey[:],
		BlindingPoint:    blindingBytes,
		BlindedHops: []*lnrpc.BlindedHop{
			{
				// The first hop in the blinded route is
				// expected to be the introduction node.
				BlindedNode:   carolBlindedBytes,
				EncryptedData: encryptedDataCarol,
			},
			{
				BlindedNode:   blinded1Bytes,
				EncryptedData: encryptedData1,
			},
			{
				BlindedNode:   blinded2Bytes,
				EncryptedData: encryptedData2,
			},
		},
	}

	// Create a blinded payment that has aggregate cltv and fee params
	// for our route.
	var (
		blindedBaseFee   uint64 = 1500
		blindedCltvDelta uint32 = 125
	)

	blindedPayment := &lnrpc.BlindedPaymentPath{
		BlindedPath:    route,
		BaseFeeMsat:    blindedBaseFee,
		TotalCltvDelta: blindedCltvDelta,
	}

	// Query for a route to the blinded path constructed above.
	var paymentAmt int64 = 100_000

	req := &lnrpc.QueryRoutesRequest{
		AmtMsat: paymentAmt,
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{
			blindedPayment,
		},
	}

	resp := alice.RPC.QueryRoutes(req)
	require.Len(ht, resp.Routes, 1)

	// Payment amount and cltv will be included for the bob/carol edge
	// (because we apply on the outgoing hop), and the blinded portion of
	// the route.
	totalFee := bobCarolBase + blindedBaseFee
	totalAmt := uint64(paymentAmt) + totalFee
	totalCltv := info.BlockHeight + bobCarolDelta + blindedCltvDelta

	// Alice -> Bob
	//   Forward: total - bob carol fees
	//   Expiry: total - bob carol delta
	//
	// Bob -> Carol
	//  Forward: 101500 (total + blinded fees)
	//  Expiry: Height + blinded cltv delta
	//  Encrypted Data: enc_carol
	//
	// Carol -> Blinded 1
	//  Forward/ Expiry: 0
	//  Encrypted Data: enc_1
	//
	// Blinded 1 -> Blinded 2
	//  Forward/ Expiry: Height
	//  Encrypted Data: enc_2
	hop0Amount := int64(totalAmt - bobCarolBase)
	hop0Expiry := totalCltv - bobCarolDelta
	finalHopExpiry := totalCltv - bobCarolDelta - blindedCltvDelta

	expectedRoute := &lnrpc.Route{
		TotalTimeLock: totalCltv,
		TotalAmtMsat:  int64(totalAmt),
		TotalFeesMsat: int64(totalFee),
		Hops: []*lnrpc.Hop{
			{
				ChanId:           aliceBobChan.ChanId,
				Expiry:           hop0Expiry,
				AmtToForwardMsat: hop0Amount,
				FeeMsat:          int64(bobCarolBase),
				PubKey:           bob.PubKeyStr,
			},
			{
				ChanId:        bobCarolChan.ChanId,
				PubKey:        carol.PubKeyStr,
				BlindingPoint: blindingBytes,
				FeeMsat:       int64(blindedBaseFee),
				EncryptedData: encryptedDataCarol,
			},
			{
				PubKey: hex.EncodeToString(
					blinded1Bytes,
				),
				EncryptedData: encryptedData1,
			},
			{
				PubKey: hex.EncodeToString(
					blinded2Bytes,
				),
				AmtToForwardMsat: paymentAmt,
				Expiry:           finalHopExpiry,
				EncryptedData:    encryptedData2,
				TotalAmtMsat:     uint64(paymentAmt),
			},
		},
	}

	r := resp.Routes[0]
	assert.Equal(ht, expectedRoute.TotalTimeLock, r.TotalTimeLock)
	assert.Equal(ht, expectedRoute.TotalAmtMsat, r.TotalAmtMsat)
	assert.Equal(ht, expectedRoute.TotalFeesMsat, r.TotalFeesMsat)

	assert.Equal(ht, len(expectedRoute.Hops), len(r.Hops))
	for i, hop := range expectedRoute.Hops {
		assert.Equal(ht, hop.PubKey, r.Hops[i].PubKey,
			"hop: %v pubkey", i)

		assert.Equal(ht, hop.ChanId, r.Hops[i].ChanId,
			"hop: %v chan id", i)

		assert.Equal(ht, hop.Expiry, r.Hops[i].Expiry,
			"hop: %v expiry", i)

		assert.Equal(ht, hop.AmtToForwardMsat,
			r.Hops[i].AmtToForwardMsat, "hop: %v forward", i)

		assert.Equal(ht, hop.FeeMsat, r.Hops[i].FeeMsat,
			"hop: %v fee", i)

		assert.Equal(ht, hop.BlindingPoint, r.Hops[i].BlindingPoint,
			"hop: %v blinding point", i)

		assert.Equal(ht, hop.EncryptedData, r.Hops[i].EncryptedData,
			"hop: %v encrypted data", i)
	}

	// Dispatch a payment to our blinded route.
	preimage := [33]byte{1, 2, 3}
	hash := sha256.Sum256(preimage[:])

	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       r,
	}

	htlcAttempt := alice.RPC.SendToRouteV2(sendReq)

	// Since Carol won't be able to decrypt the dummy encrypted data
	// containing the forwarding information, we expect her to fail the
	// payment.
	require.NotNil(ht, htlcAttempt.Failure)
	require.Equal(ht, uint32(2), htlcAttempt.Failure.FailureSourceIndex)

	// Next, we test an edge case where just an introduction node is
	// included as a "single hop blinded route".
	sendToIntroCLTVFinal := uint32(15)
	sendToIntroTimelock := info.BlockHeight + bobCarolDelta +
		sendToIntroCLTVFinal

	introNodeBlinded := &lnrpc.BlindedPaymentPath{
		BlindedPath: &lnrpc.BlindedPath{
			IntroductionNode: carol.PubKey[:],
			BlindingPoint:    blindingBytes,
			BlindedHops: []*lnrpc.BlindedHop{
				{
					// The first hop in the blinded route is
					// expected to be the introduction node.
					BlindedNode:   carolBlindedBytes,
					EncryptedData: encryptedDataCarol,
				},
			},
		},
		// Fees should be zero for a single hop blinded path, and the
		// total cltv expiry is just expected to cover the final cltv
		// delta of the receiving node (ie, the introduction node).
		BaseFeeMsat:    0,
		TotalCltvDelta: sendToIntroCLTVFinal,
	}
	req = &lnrpc.QueryRoutesRequest{
		AmtMsat: paymentAmt,
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{
			introNodeBlinded,
		},
	}

	// Assert that we have one route, and two hops: Alice/Bob and Bob/Carol.
	resp = alice.RPC.QueryRoutes(req)
	require.Len(ht, resp.Routes, 1)
	require.Len(ht, resp.Routes[0].Hops, 2)
	require.Equal(ht, resp.Routes[0].TotalTimeLock, sendToIntroTimelock)

	ht.CloseChannel(alice, chanPointAliceBob)
	ht.CloseChannel(bob, chanPointBobCarol)
}

type blindedForwardTest struct {
	ht       *lntest.HarnessTest
	carol    *node.HarnessNode
	dave     *node.HarnessNode
	channels []*lnrpc.ChannelPoint

	carolInterceptor routerrpc.Router_HtlcInterceptorClient

	preimage [32]byte

	// cancel will cancel the test's top level context.
	cancel func()
}

func newBlindedForwardTest(ht *lntest.HarnessTest) (context.Context,
	*blindedForwardTest) {

	ctx, cancel := context.WithCancel(context.Background())

	return ctx, &blindedForwardTest{
		ht:       ht,
		cancel:   cancel,
		preimage: [32]byte{1, 2, 3},
	}
}

// setupNetwork spins up additional nodes needed for our test and creates a four
// hop network for testing blinded path logic and an optional interceptor on
// Carol's node for those tests where we want to perhaps prevent the final hop
// from settling.
func (b *blindedForwardTest) setupNetwork(ctx context.Context,
	withInterceptor bool) {

	carolArgs := []string{"--bitcoin.timelockdelta=18"}
	if withInterceptor {
		carolArgs = append(carolArgs, "--requireinterceptor")
	}
	b.carol = b.ht.NewNode("Carol", carolArgs)

	if withInterceptor {
		var err error
		b.carolInterceptor, err = b.carol.RPC.Router.HtlcInterceptor(
			ctx,
		)
		require.NoError(b.ht, err, "interceptor")
	}

	// Restrict Dave so that he only ever creates a single blinded path from
	// Bob to himself.
	b.dave = b.ht.NewNode("Dave", []string{
		"--bitcoin.timelockdelta=18",
		"--routing.blinding.min-num-real-hops=2",
		"--routing.blinding.num-hops=2",
	})

	b.channels = setupFourHopNetwork(b.ht, b.carol, b.dave)
}

// buildBlindedPath returns a blinded route from Bob -> Carol -> Dave, with Bob
// acting as the introduction point.
func (b *blindedForwardTest) buildBlindedPath() *lnrpc.BlindedPaymentPath {
	// Let Dave add a blinded invoice.
	invoice := b.dave.RPC.AddInvoice(&lnrpc.Invoice{
		RPreimage: b.preimage[:],
		Memo:      "test",
		ValueMsat: 10_000_000,
		Blind:     true,
	})

	// Assert that only one blinded path is selected and that it contains
	// a 3 hop path starting at Bob.
	payReq := b.dave.RPC.DecodePayReq(invoice.PaymentRequest)
	require.Len(b.ht, payReq.BlindedPaths, 1)
	path := payReq.BlindedPaths[0].BlindedPath
	require.Len(b.ht, path.BlindedHops, 3)
	require.EqualValues(b.ht, path.IntroductionNode, b.ht.Bob.PubKey[:])

	return payReq.BlindedPaths[0]
}

// cleanup tears down all channels created by the test and cancels the top
// level context used in the test.
func (b *blindedForwardTest) cleanup() {
	b.ht.CloseChannel(b.ht.Alice, b.channels[0])
	b.ht.CloseChannel(b.ht.Bob, b.channels[1])
	b.ht.CloseChannel(b.carol, b.channels[2])

	b.cancel()
}

// createRouteToBlinded queries for a route from alice to the blinded path
// provided.
//
//nolint:gomnd
func (b *blindedForwardTest) createRouteToBlinded(paymentAmt int64,
	blindedPath *lnrpc.BlindedPaymentPath) *lnrpc.Route {

	req := &lnrpc.QueryRoutesRequest{
		AmtMsat: paymentAmt,
		// Our fee limit doesn't really matter, we just want to be able
		// to make the payment.
		FeeLimit: &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Percent{
				Percent: 50,
			},
		},
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{
			blindedPath,
		},
	}

	resp := b.ht.Alice.RPC.QueryRoutes(req)
	require.Greater(b.ht, len(resp.Routes), 0, "no routes")
	require.Len(b.ht, resp.Routes[0].Hops, 3, "unexpected route length")

	return resp.Routes[0]
}

// sendBlindedPayment dispatches a payment to the route provided, returning a
// cancel function for the payment. Timeout is set for very long to allow
// time for on-chain resolution.
func (b *blindedForwardTest) sendBlindedPayment(ctx context.Context,
	route *lnrpc.Route) func() {

	hash := sha256.Sum256(b.preimage[:])
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       route,
	}

	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	go func() {
		_, err := b.ht.Alice.RPC.Router.SendToRouteV2(ctx, sendReq)

		// We may get a context canceled error when the test is
		// finished.
		if errors.Is(err, context.Canceled) {
			b.ht.Logf("sendBlindedPayment: parent context canceled")
			return
		}

		require.NoError(b.ht, err)
	}()

	return cancel
}

// sendToRoute synchronously lets Alice attempt to send to the given route
// using the SendToRouteV2 endpoint and asserts that the payment either
// succeeds or fails.
func (b *blindedForwardTest) sendToRoute(route *lnrpc.Route,
	assertSuccess bool) {

	hash := sha256.Sum256(b.preimage[:])
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       route,
	}

	// Let Alice send to the blinded payment path and assert that it
	// succeeds/fails.
	htlcAttempt := b.ht.Alice.RPC.SendToRouteV2(sendReq)
	if assertSuccess {
		require.Nil(b.ht, htlcAttempt.Failure)
		require.Equal(b.ht, htlcAttempt.Status,
			lnrpc.HTLCAttempt_SUCCEEDED)

		return
	}

	require.NotNil(b.ht, htlcAttempt.Failure)
	require.Equal(b.ht, htlcAttempt.Status, lnrpc.HTLCAttempt_FAILED)

	// Wait for the HTLC to reflect as failed for Alice.
	preimage, err := lntypes.MakePreimage(b.preimage[:])
	require.NoError(b.ht, err)

	pmt := b.ht.AssertPaymentStatus(
		b.ht.Alice, preimage, lnrpc.Payment_FAILED,
	)
	require.Len(b.ht, pmt.Htlcs, 1)

	// Assert that the failure appears to originate from the introduction
	// node hop.
	require.EqualValues(b.ht, 1, pmt.Htlcs[0].Failure.FailureSourceIndex)
	require.Equal(
		b.ht, lnrpc.Failure_INVALID_ONION_BLINDING,
		pmt.Htlcs[0].Failure.Code,
	)
}

// drainCarolLiquidity will drain all of the liquidity in Carol's channel in
// the direction requested:
// - incoming: Carol has no incoming liquidity from Bob
// - outgoing: Carol has no outgoing liquidity to Dave.
func (b *blindedForwardTest) drainCarolLiquidity(incoming bool) {
	sendingNode := b.carol
	receivingNode := b.dave

	if incoming {
		sendingNode = b.ht.Bob
		receivingNode = b.carol
	}

	resp := sendingNode.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		Peer: receivingNode.PubKey[:],
	})
	require.Len(b.ht, resp.Channels, 1)

	// We can't send our channel reserve, and leave some buffer for fees.
	paymentAmt := resp.Channels[0].LocalBalance -
		int64(resp.Channels[0].RemoteConstraints.ChanReserveSat) - 25000

	invoice := receivingNode.RPC.AddInvoice(&lnrpc.Invoice{
		// Leave some leeway for fees for the HTLC.
		Value: paymentAmt,
	})

	pmtClient := sendingNode.RPC.SendPayment(
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
		},
	)

	b.ht.AssertPaymentStatusFromStream(pmtClient, lnrpc.Payment_SUCCEEDED)
}

// setupFourHopNetwork creates a network with the following topology and
// liquidity:
// Alice (100k)----- Bob (100k) ----- Carol (100k) ----- Dave
//
// The funding outpoint for AB / BC / CD are returned in-order.
func setupFourHopNetwork(ht *lntest.HarnessTest,
	carol, dave *node.HarnessNode) []*lnrpc.ChannelPoint {

	const chanAmt = btcutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := ht.OpenChannel(
		ht.Alice, ht.Bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	// Create a channel between bob and carol.
	ht.EnsureConnected(ht.Bob, carol)
	chanPointBob := ht.OpenChannel(
		ht.Bob, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointBob)

	// Fund carol and connect her and dave so that she can create a channel
	// between them.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	ht.EnsureConnected(carol, dave)

	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	// Wait for all nodes to have seen all channels.
	nodes := []*node.HarnessNode{ht.Alice, ht.Bob, carol, dave}
	for _, chanPoint := range networkChans {
		for _, node := range nodes {
			ht.AssertTopologyChannelOpen(node, chanPoint)
		}
	}

	return []*lnrpc.ChannelPoint{
		chanPointAlice,
		chanPointBob,
		chanPointCarol,
	}
}

// testBlindedRouteInvoices tests lnd's ability to create a blinded payment path
// which it then inserts into an invoice, sending to an invoice with a blinded
// path and forward payments in a blinded route and finally, receiving the
// payment.
func testBlindedRouteInvoices(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()

	// Set up the 4 hop network and let Dave create an invoice with a
	// blinded path that uses Bob as an introduction node.
	testCase.setupNetwork(ctx, false)

	// Let Dave add a blinded invoice.
	invoice := testCase.dave.RPC.AddInvoice(&lnrpc.Invoice{
		Memo:      "test",
		ValueMsat: 10_000_000,
		Blind:     true,
	})

	// Now let Alice pay the invoice.
	ht.CompletePaymentRequests(ht.Alice, []string{invoice.PaymentRequest})

	// Restart Dave with blinded path restrictions that will result in him
	// creating a blinded path that uses himself as the introduction node.
	ht.RestartNodeWithExtraArgs(testCase.dave, []string{
		"--routing.blinding.min-num-real-hops=0",
		"--routing.blinding.num-hops=0",
	})
	ht.EnsureConnected(testCase.dave, testCase.carol)

	// Let Dave add a blinded invoice.
	// Once again let Dave create a blinded invoice.
	invoice = testCase.dave.RPC.AddInvoice(&lnrpc.Invoice{
		Memo:      "test",
		ValueMsat: 10_000_000,
		Blind:     true,
	})

	// Assert that it contains a single blinded path with only an
	// introduction node hop where the introduction node is Dave.
	payReq := testCase.dave.RPC.DecodePayReq(invoice.PaymentRequest)
	require.Len(ht, payReq.BlindedPaths, 1)
	path := payReq.BlindedPaths[0].BlindedPath
	require.Len(ht, path.BlindedHops, 1)
	require.EqualValues(ht, path.IntroductionNode, testCase.dave.PubKey[:])

	// Now let Alice pay the invoice.
	ht.CompletePaymentRequests(ht.Alice, []string{invoice.PaymentRequest})
}

// testReceiverBlindedError tests handling of errors from the receiving node in
// a blinded route, testing a payment over: Alice -- Bob -- Carol -- Dave, where
// Bob is the introduction node.
func testReceiverBlindedError(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()

	testCase.setupNetwork(ctx, false)
	blindedPaymentPath := testCase.buildBlindedPath()
	route := testCase.createRouteToBlinded(10_000_000, blindedPaymentPath)

	// Replace the encrypted recipient data payload for Dave (the recipient)
	// with an invalid payload which Dave will then fail to parse when he
	// receives the incoming HTLC for this payment.
	route.Hops[len(route.Hops)-1].EncryptedData = []byte{1, 2, 3}

	// Subscribe to Dave's HTLC events so that we can observe the payment
	// coming in.
	daveEvents := testCase.dave.RPC.SubscribeHtlcEvents()

	// Once subscribed, the first event will be UNKNOWN.
	ht.AssertHtlcEventType(daveEvents, routerrpc.HtlcEvent_UNKNOWN)

	// Let Alice send to the constructed route and assert that the payment
	// fails.
	testCase.sendToRoute(route, false)

	// Make sure that the HTLC did in fact reach Dave and fail there.
	ht.AssertHtlcEvents(daveEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_FORWARD)
}

// testRelayingBlindedError tests handling of errors from relaying nodes in a
// blinded route, testing a failure over on Carol's outgoing link in the
// following topology: Alice -- Bob -- Carol -- Dave, where Bob is the
// introduction node.
func testRelayingBlindedError(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()

	testCase.setupNetwork(ctx, false)
	blindedPaymentPath := testCase.buildBlindedPath()
	route := testCase.createRouteToBlinded(10_000_000, blindedPaymentPath)

	// Before we send our payment, drain all of Carol's liquidity
	// so that she can't forward the payment to Dave.
	testCase.drainCarolLiquidity(false)

	// Subscribe to Carol's HTLC events so that we can observe the payment
	// coming in.
	carolEvents := testCase.carol.RPC.SubscribeHtlcEvents()

	// Once subscribed, the first event will be UNKNOWN.
	ht.AssertHtlcEventType(carolEvents, routerrpc.HtlcEvent_UNKNOWN)

	// Let Alice send to the constructed route and assert that the payment
	// fails.
	testCase.sendToRoute(route, false)

	// Make sure that the HTLC did in fact reach Carol and fail there.
	ht.AssertHtlcEvents(
		carolEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_FORWARD,
	)
}

// testIntroductionNodeError tests handling of errors in a blinded route when
// the introduction node is the source of the error. This test sends a payment
// over Alice -- Bob -- Carol -- Dave, where Bob is the introduction node and
// has insufficient outgoing liquidity to forward on to carol.
func testIntroductionNodeError(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()
	testCase.setupNetwork(ctx, false)
	blindedPaymentPath := testCase.buildBlindedPath()
	route := testCase.createRouteToBlinded(10_000_000, blindedPaymentPath)

	// Before we send our payment, drain all of Carol's incoming liquidity
	// so that she can't receive the forward from Bob, causing a failure
	// at the introduction node.
	testCase.drainCarolLiquidity(true)

	// Subscribe to Bob's HTLC events so that we can observe the payment
	// coming in.
	bobEvents := ht.Bob.RPC.SubscribeHtlcEvents()

	// Once subscribed, the first event will be UNKNOWN.
	ht.AssertHtlcEventType(bobEvents, routerrpc.HtlcEvent_UNKNOWN)

	// Let Alice send to the constructed route and assert that the payment
	// fails.
	testCase.sendToRoute(route, false)

	// Make sure that the HTLC did in fact reach Bob and fail there.
	ht.AssertHtlcEvents(
		bobEvents, 0, 0, 0, 1, routerrpc.HtlcEvent_FORWARD,
	)
}

// testDisableIntroductionNode tests disabling of blinded forwards for the
// introduction node.
func testDisableIntroductionNode(ht *lntest.HarnessTest) {
	// First construct a blinded route while Bob is still advertising the
	// route blinding feature bit to ensure that Bob is included in the
	// blinded path that Dave selects.
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()
	testCase.setupNetwork(ctx, false)
	blindedPaymentPath := testCase.buildBlindedPath()
	route := testCase.createRouteToBlinded(10_000_000, blindedPaymentPath)

	// Now, disable route blinding for Bob, then re-connect to Alice.
	ht.RestartNodeWithExtraArgs(ht.Bob, []string{
		"--protocol.no-route-blinding",
	})
	ht.EnsureConnected(ht.Alice, ht.Bob)

	// Assert that this fails.
	testCase.sendToRoute(route, false)
}

// testErrorHandlingOnChainFailure tests handling of blinded errors when we're
// resolving from an on-chain resolution. This test also tests that we're able
// to resolve blinded HTLCs on chain between restarts, as we've got all the
// infrastructure in place already for error testing.
func testErrorHandlingOnChainFailure(ht *lntest.HarnessTest) {
	// Setup a test case, note that we don't use its built in clean up
	// because we're going to close a channel, so we'll close out the
	// rest manually.
	ctx, testCase := newBlindedForwardTest(ht)

	// Note that we send a larger amount here, so it'll be worthwhile for
	// the sweeper to claim.
	testCase.setupNetwork(ctx, true)
	blindedPaymentPath := testCase.buildBlindedPath()
	blindedRoute := testCase.createRouteToBlinded(
		50_000_000, blindedPaymentPath,
	)

	// Once our interceptor is set up, we can send the blinded payment.
	cancelPmt := testCase.sendBlindedPayment(ctx, blindedRoute)
	defer cancelPmt()

	// Wait for the HTLC to be active on Alice and Bob's channels.
	hash := sha256.Sum256(testCase.preimage[:])
	ht.AssertOutgoingHTLCActive(ht.Alice, testCase.channels[0], hash[:])
	ht.AssertOutgoingHTLCActive(ht.Bob, testCase.channels[1], hash[:])

	// Intercept the forward on Carol's link, but do not take any action
	// so that we have the chance to force close with this HTLC in flight.
	carolHTLC, err := testCase.carolInterceptor.Recv()
	require.NoError(ht, err)

	// Force close Bob <-> Carol.
	closeStream, _ := ht.CloseChannelAssertPending(
		ht.Bob, testCase.channels[1], true,
	)

	ht.AssertStreamChannelForceClosed(
		ht.Bob, testCase.channels[1], false, closeStream,
	)

	// SuspendCarol so that she can't interfere with the resolution of the
	// HTLC from now on.
	restartCarol := ht.SuspendNode(testCase.carol)

	// Mine blocks so that Bob will claim his CSV delayed local commitment,
	// we've already mined 1 block so we need one less than our CSV.
	ht.MineBlocks(node.DefaultCSV - 1)
	ht.AssertNumPendingSweeps(ht.Bob, 1)
	ht.MineEmptyBlocks(1)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Restart bob so that we can test that he's able to recover everything
	// he needs to claim a blinded HTLC.
	ht.RestartNode(ht.Bob)

	// Mine enough blocks for Bob to trigger timeout of his outgoing HTLC.
	// Carol's incoming expiry height is Bob's outgoing so we can use this
	// value.
	info := ht.Bob.RPC.GetInfo()
	target := carolHTLC.IncomingExpiry - info.BlockHeight
	ht.MineBlocks(int(target))

	// Wait for Bob's timeout transaction in the mempool, since we've
	// suspended Carol we don't need to account for her commitment output
	// claim.
	ht.AssertNumPendingSweeps(ht.Bob, 0)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Assert that the HTLC has cleared.
	ht.WaitForBlockchainSync(ht.Bob)
	ht.WaitForBlockchainSync(ht.Alice)

	ht.AssertHTLCNotActive(ht.Bob, testCase.channels[0], hash[:])
	ht.AssertHTLCNotActive(ht.Alice, testCase.channels[0], hash[:])

	// Wait for the HTLC to reflect as failed for Alice.
	paymentStream := ht.Alice.RPC.TrackPaymentV2(hash[:])
	htlcs := ht.ReceiveTrackPayment(paymentStream).Htlcs
	require.Len(ht, htlcs, 1)
	require.NotNil(ht, htlcs[0].Failure)
	require.Equal(
		ht, htlcs[0].Failure.Code,
		lnrpc.Failure_INVALID_ONION_BLINDING,
	)

	// Clean up the rest of our force close: mine blocks so that Bob's CSV
	// expires plus one block to trigger his sweep and then mine it.
	ht.MineBlocks(node.DefaultCSV + 1)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Bring carol back up so that we can close out the rest of our
	// channels cooperatively. She requires an interceptor to start up
	// so we just re-register our interceptor.
	require.NoError(ht, restartCarol())
	_, err = testCase.carol.RPC.Router.HtlcInterceptor(ctx)
	require.NoError(ht, err, "interceptor")

	// Assert that Carol has started up and reconnected to dave so that
	// we can close out channels cooperatively.
	ht.EnsureConnected(testCase.carol, testCase.dave)

	// Manually close out the rest of our channels and cancel (don't use
	// built in cleanup which will try close the already-force-closed
	// channel).
	ht.CloseChannel(ht.Alice, testCase.channels[0])
	ht.CloseChannel(testCase.carol, testCase.channels[2])
	testCase.cancel()
}

// testMPPToSingleBlindedPath tests that a two-shard MPP payment can be sent
// over a single blinded path.
// The following graph is created where Dave is the destination node, and he
// will choose Carol as the introduction node. The channel capacities are set in
// such a way that Alice will have to split the payment to dave over both the
// A->B->C-D and A->E->C->D routes. The Carol-Dave channel will also be made
// a private channel so that we can test that Dave's private channels are in
// fact being used in the chosen blinded paths.
//
//		    ---- Bob ---
//	          /		\
//	       Alice		Carol --- Dave
//		   \		 /
//		    ---- Eve ---
func testMPPToSingleBlindedPath(ht *lntest.HarnessTest) {
	// Create a five-node context consisting of Alice, Bob and three new
	// nodes.
	alice, bob := ht.Alice, ht.Bob

	// Restrict Dave so that he only ever chooses the Carol->Dave path for
	// a blinded route.
	dave := ht.NewNode("dave", []string{
		"--routing.blinding.min-num-real-hops=1",
		"--routing.blinding.num-hops=1",
	})
	carol := ht.NewNode("carol", nil)
	eve := ht.NewNode("eve", nil)

	// Connect nodes to ensure propagation of channels.
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(alice, eve)
	ht.EnsureConnected(carol, bob)
	ht.EnsureConnected(carol, eve)
	ht.EnsureConnected(carol, dave)

	// Send coins to the nodes and mine 1 blocks to confirm them.
	for i := 0; i < 2; i++ {
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, dave)
		ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, eve)
		ht.MineBlocksAndAssertNumTxes(1, 3)
	}

	const paymentAmt = btcutil.Amount(300000)

	nodes := []*node.HarnessNode{alice, bob, carol, dave, eve}
	reqs := []*lntest.OpenChannelRequest{
		{
			Local:  alice,
			Remote: bob,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2 / 3,
			},
		},
		{
			Local:  alice,
			Remote: eve,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2 / 3,
			},
		},
		{
			Local:  bob,
			Remote: carol,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2,
			},
		},
		{
			Local:  eve,
			Remote: carol,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2,
			},
		},
		{
			// Note that this is a private channel.
			Local:  carol,
			Remote: dave,
			Param: lntest.OpenChannelParams{
				Amt:     paymentAmt * 2,
				Private: true,
			},
		},
	}

	channelPoints := ht.OpenMultiChannelsAsync(reqs)

	// Make sure every node has heard about every public channel.
	for _, hn := range nodes {
		var numPublic int
		for i, cp := range channelPoints {
			if reqs[i].Param.Private {
				continue
			}

			numPublic++
			ht.AssertTopologyChannelOpen(hn, cp)
		}

		// Each node should have exactly numPublic edges.
		ht.AssertNumEdges(hn, numPublic, false)
	}

	// Make Dave create an invoice with a blinded path for Alice to pay.
	invoice := &lnrpc.Invoice{
		Memo:  "test",
		Value: int64(paymentAmt),
		Blind: true,
	}
	invoiceResp := dave.RPC.AddInvoice(invoice)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		MaxParts:       10,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payment := ht.SendPaymentAssertSettled(alice, sendReq)

	preimageBytes, err := hex.DecodeString(payment.PaymentPreimage)
	require.NoError(ht, err)

	preimage, err := lntypes.MakePreimage(preimageBytes)
	require.NoError(ht, err)

	hash, err := lntypes.MakeHash(invoiceResp.RHash)
	require.NoError(ht, err)

	// Make sure we got the preimage.
	require.True(ht, preimage.Matches(hash), "preimage doesn't match")

	// Check that Alice split the payment in at least two shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore, we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 2
	require.GreaterOrEqual(ht, succeeded, minExpectedShards,
		"expected shards not reached")

	// Make sure Dave show the invoice as settled for the full amount.
	inv := dave.RPC.LookupInvoice(invoiceResp.RHash)

	require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
		"incorrect payment amt")

	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State,
		"Invoice not settled")

	settled := 0
	for _, htlc := range inv.Htlcs {
		if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
			settled++
		}
	}
	require.Equal(ht, succeeded, settled, "num of HTLCs wrong")

	// Close all channels without mining the closing transactions.
	ht.CloseChannelAssertPending(alice, channelPoints[0], false)
	ht.CloseChannelAssertPending(alice, channelPoints[1], false)
	ht.CloseChannelAssertPending(bob, channelPoints[2], false)
	ht.CloseChannelAssertPending(eve, channelPoints[3], false)
	ht.CloseChannelAssertPending(carol, channelPoints[4], false)

	// Now mine a block to include all the closing transactions.
	ht.MineBlocksAndAssertNumTxes(1, 5)

	// Assert that the channels are closed.
	for _, hn := range nodes {
		ht.AssertNumWaitingClose(hn, 0)
	}
}

// testBlindedRouteDummyHops tests that the route blinding flow works as
// expected in the cases where the recipient chooses to pad the blinded path
// with dummy hops.
//
// We will set up the following network were Dave will always be the recipient
// and Alice is always the payer. Bob will not support route blinding and so
// will never be chosen as the introduction node.
//
//	Alice -- Bob -- Carol -- Dave
//
// First we will start Carol _without_ route blinding support. Then we will
// configure Dave such that any blinded route he constructs must be 2 hops long.
// Since Carol cannot be chosen as an introduction node, Dave chooses himself
// as an introduction node and appends two dummy hops.
// Next, we will restart Carol with route blinding support and repeat the test
// but this time we force Dave to construct a path with a minimum distance of 1
// between him and the introduction node. So we expect that Carol is chosen as
// the intro node and that one dummy hops is appended.
func testBlindedRouteDummyHops(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// Disable route blinding for Bob so that he is never chosen as the
	// introduction node.
	ht.RestartNodeWithExtraArgs(bob, []string{
		"--protocol.no-route-blinding",
	})

	// Start carol without route blinding so that initially Carol is also
	// not chosen.
	carol := ht.NewNode("carol", []string{
		"--protocol.no-route-blinding",
	})

	// Configure Dave so that all blinded paths always contain 2 hops and
	// so that there is no minimum number of real hops.
	dave := ht.NewNode("dave", []string{
		"--routing.blinding.min-num-real-hops=0",
		"--routing.blinding.num-hops=2",
	})

	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.EnsureConnected(carol, dave)

	// Send coins to carol and mine 1 blocks to confirm them.
	ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	const paymentAmt = btcutil.Amount(300000)

	nodes := []*node.HarnessNode{alice, bob, carol, dave}
	reqs := []*lntest.OpenChannelRequest{
		{
			Local:  alice,
			Remote: bob,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 3,
			},
		},
		{
			Local:  bob,
			Remote: carol,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 3,
			},
		},
		{
			Local:  carol,
			Remote: dave,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 3,
			},
		},
	}

	channelPoints := ht.OpenMultiChannelsAsync(reqs)

	// Make sure every node has heard about every channel.
	for _, hn := range nodes {
		for _, cp := range channelPoints {
			ht.AssertTopologyChannelOpen(hn, cp)
		}

		// Each node should have exactly 5 edges.
		ht.AssertNumEdges(hn, len(channelPoints), false)
	}

	// Make Dave create an invoice with a blinded path for Alice to pay.
	invoice := &lnrpc.Invoice{
		Memo:  "test",
		Value: int64(paymentAmt),
		Blind: true,
	}
	invoiceResp := dave.RPC.AddInvoice(invoice)

	// Assert that it contains a single blinded path and that the
	// introduction node is dave.
	payReq := dave.RPC.DecodePayReq(invoiceResp.PaymentRequest)
	require.Len(ht, payReq.BlindedPaths, 1)

	// The total number of hop payloads is 3: one for the introduction node
	// and then one for each dummy hop.
	path := payReq.BlindedPaths[0].BlindedPath
	require.Len(ht, path.BlindedHops, 3)
	require.EqualValues(ht, path.IntroductionNode, dave.PubKey[:])

	// Now let Alice pay the invoice.
	ht.CompletePaymentRequests(
		ht.Alice, []string{invoiceResp.PaymentRequest},
	)

	// Make sure Dave show the invoice as settled.
	inv := dave.RPC.LookupInvoice(invoiceResp.RHash)
	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State)

	// Let's also test the case where Dave is not the introduction node.
	// We restart Carol so that she supports route blinding. We also restart
	// Dave and force a minimum of 1 real blinded hop. We keep the number
	// of hops to 2 meaning that one dummy hop should be added.
	ht.RestartNodeWithExtraArgs(carol, nil)
	ht.RestartNodeWithExtraArgs(dave, []string{
		"--routing.blinding.min-num-real-hops=1",
		"--routing.blinding.num-hops=2",
	})
	ht.EnsureConnected(bob, carol)
	ht.EnsureConnected(carol, dave)

	// Make Dave create an invoice with a blinded path for Alice to pay.
	invoiceResp = dave.RPC.AddInvoice(invoice)

	// Assert that it contains a single blinded path and that the
	// introduction node is Carol.
	payReq = dave.RPC.DecodePayReq(invoiceResp.PaymentRequest)
	for _, path := range payReq.BlindedPaths {
		ht.Logf("intro node: %x", path.BlindedPath.IntroductionNode)
	}

	require.Len(ht, payReq.BlindedPaths, 1)

	// The total number of hop payloads is 3: one for the introduction node
	// and then one for each dummy hop.
	path = payReq.BlindedPaths[0].BlindedPath
	require.Len(ht, path.BlindedHops, 3)
	require.EqualValues(ht, path.IntroductionNode, carol.PubKey[:])

	// Now let Alice pay the invoice.
	ht.CompletePaymentRequests(
		ht.Alice, []string{invoiceResp.PaymentRequest},
	)

	// Make sure Dave show the invoice as settled.
	inv = dave.RPC.LookupInvoice(invoiceResp.RHash)
	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State)

	// Close all channels without mining the closing transactions.
	ht.CloseChannelAssertPending(alice, channelPoints[0], false)
	ht.CloseChannelAssertPending(bob, channelPoints[1], false)
	ht.CloseChannelAssertPending(carol, channelPoints[2], false)

	// Now mine a block to include all the closing transactions.
	ht.MineBlocksAndAssertNumTxes(1, 3)

	// Assert that the channels are closed.
	for _, hn := range nodes {
		ht.AssertNumWaitingClose(hn, 0)
	}
}

// testMPPToMultipleBlindedPaths tests that a two-shard MPP payment can be sent
// over a multiple blinded paths. The following network is created where Dave
// is the recipient and Alice the sender. Dave will create an invoice containing
// two blinded paths: one with Bob at the intro node and one with Carol as the
// intro node. Channel liquidity will be set up in such a way that Alice will be
// forced to send one shared via the Bob->Dave route and one over the
// Carol->Dave route.
//
//		   --- Bob ---
//	          /            \
//	      Alice           Dave
//		  \	       /
//		   --- Carol ---
func testMPPToMultipleBlindedPaths(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// Create a four-node context consisting of Alice, Bob and three new
	// nodes.
	dave := ht.NewNode("dave", []string{
		"--routing.blinding.min-num-real-hops=1",
		"--routing.blinding.num-hops=1",
	})
	carol := ht.NewNode("carol", nil)

	// Connect nodes to ensure propagation of channels.
	ht.EnsureConnected(alice, carol)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(carol, dave)
	ht.EnsureConnected(bob, dave)

	// Fund the new nodes.
	ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
	ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, dave)
	ht.MineBlocksAndAssertNumTxes(1, 2)

	const paymentAmt = btcutil.Amount(300000)

	nodes := []*node.HarnessNode{alice, bob, carol, dave}

	reqs := []*lntest.OpenChannelRequest{
		{
			Local:  alice,
			Remote: bob,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2 / 3,
			},
		},
		{
			Local:  alice,
			Remote: carol,
			Param: lntest.OpenChannelParams{
				Amt: paymentAmt * 2 / 3,
			},
		},
		{
			Local:  bob,
			Remote: dave,
			Param:  lntest.OpenChannelParams{Amt: paymentAmt * 2},
		},
		{
			Local:  carol,
			Remote: dave,
			Param:  lntest.OpenChannelParams{Amt: paymentAmt * 2},
		},
	}

	channelPoints := ht.OpenMultiChannelsAsync(reqs)

	// Make sure every node has heard every channel.
	for _, hn := range nodes {
		for _, cp := range channelPoints {
			ht.AssertTopologyChannelOpen(hn, cp)
		}

		// Each node should have exactly 5 edges.
		ht.AssertNumEdges(hn, len(channelPoints), false)
	}

	// Ok now make a payment that must be split to succeed.

	// Make Dave create an invoice for Alice to pay
	invoice := &lnrpc.Invoice{
		Memo:  "test",
		Value: int64(paymentAmt),
		Blind: true,
	}
	invoiceResp := dave.RPC.AddInvoice(invoice)

	// Assert that two blinded paths are included in the invoice.
	payReq := dave.RPC.DecodePayReq(invoiceResp.PaymentRequest)
	require.Len(ht, payReq.BlindedPaths, 2)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		MaxParts:       10,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payment := ht.SendPaymentAssertSettled(alice, sendReq)

	preimageBytes, err := hex.DecodeString(payment.PaymentPreimage)
	require.NoError(ht, err)

	preimage, err := lntypes.MakePreimage(preimageBytes)
	require.NoError(ht, err)

	hash, err := lntypes.MakeHash(invoiceResp.RHash)
	require.NoError(ht, err)

	// Make sure we got the preimage.
	require.True(ht, preimage.Matches(hash), "preimage doesn't match")

	// Check that Alice split the payment in at least two shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 2
	require.GreaterOrEqual(ht, succeeded, minExpectedShards,
		"expected shards not reached")

	// Make sure Dave show the invoice as settled for the full amount.
	inv := dave.RPC.LookupInvoice(invoiceResp.RHash)

	require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
		"incorrect payment amt")

	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State,
		"Invoice not settled")

	settled := 0
	for _, htlc := range inv.Htlcs {
		if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
			settled++
		}
	}
	require.Equal(ht, succeeded, settled, "num of HTLCs wrong")

	// Close all channels without mining the closing transactions.
	ht.CloseChannelAssertPending(alice, channelPoints[0], false)
	ht.CloseChannelAssertPending(alice, channelPoints[1], false)
	ht.CloseChannelAssertPending(bob, channelPoints[2], false)
	ht.CloseChannelAssertPending(carol, channelPoints[3], false)

	// Now mine a block to include all the closing transactions. (first
	// iteration: no blinded paths)
	ht.MineBlocksAndAssertNumTxes(1, 4)

	// Assert that the channels are closed.
	for _, hn := range nodes {
		ht.AssertNumWaitingClose(hn, 0)
	}
}
