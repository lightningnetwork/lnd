package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
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

	// Since Carol doesn't understand blinded routes, we expect her to fail
	// the payment because the onion payload is invalid (missing amount to
	// forward).
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

// setup spins up additional nodes needed for our test and creates a four hop
// network for testing blinded forwarding and returns a blinded route from
// Bob -> Carol -> Dave, with Bob acting as the introduction point and an
// interceptor on Carol's node to manage HTLCs (as Dave does not yet support
// receiving).
func (b *blindedForwardTest) setup(
	ctx context.Context) *routing.BlindedPayment {

	b.carol = b.ht.NewNode("Carol", []string{
		"requireinterceptor",
	})

	var err error
	b.carolInterceptor, err = b.carol.RPC.Router.HtlcInterceptor(ctx)
	require.NoError(b.ht, err, "interceptor")

	b.dave = b.ht.NewNode("Dave", nil)

	b.channels = setupFourHopNetwork(b.ht, b.carol, b.dave)

	// Create a blinded route to Dave via Bob --- Carol --- Dave:
	bobChan := b.ht.GetChannelByChanPoint(b.ht.Bob, b.channels[1])
	carolChan := b.ht.GetChannelByChanPoint(b.carol, b.channels[2])

	edges := []*forwardingEdge{
		getForwardingEdge(b.ht, b.ht.Bob, bobChan.ChanId),
		getForwardingEdge(b.ht, b.carol, carolChan.ChanId),
	}

	davePk, err := btcec.ParsePubKey(b.dave.PubKey[:])
	require.NoError(b.ht, err, "dave pubkey")

	return b.createBlindedRoute(edges, davePk, 50)
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
	route *routing.BlindedPayment) *lnrpc.Route {

	intro := route.BlindedPath.IntroductionPoint.SerializeCompressed()
	blinding := route.BlindedPath.BlindingPoint.SerializeCompressed()

	blindedRoute := &lnrpc.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    blinding,
		BlindedHops: make(
			[]*lnrpc.BlindedHop,
			len(route.BlindedPath.BlindedHops),
		),
	}

	for i, hop := range route.BlindedPath.BlindedHops {
		blindedRoute.BlindedHops[i] = &lnrpc.BlindedHop{
			BlindedNode:   hop.BlindedNodePub.SerializeCompressed(),
			EncryptedData: hop.CipherText,
		}
	}
	blindedPath := &lnrpc.BlindedPaymentPath{
		BlindedPath: blindedRoute,
		BaseFeeMsat: uint64(
			route.BaseFee,
		),
		ProportionalFeeRate: route.ProportionalFeeRate,
		TotalCltvDelta: uint32(
			route.CltvExpiryDelta,
		),
	}

	req := &lnrpc.QueryRoutesRequest{
		AmtMsat: paymentAmt,
		// Our fee limit doesn't really matter, we just want to
		// be able to make the payment.
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

// sendBlindedPayment dispatches a payment to the route provided. The streaming
// client for the send is returned with a cancel function that can be used to
// terminate the stream.
func (b *blindedForwardTest) sendBlindedPayment(ctx context.Context,
	route *lnrpc.Route) (lnrpc.Lightning_SendToRouteClient, func()) {

	hash := sha256.Sum256(b.preimage[:])

	ctxt, cancel := context.WithCancel(ctx)
	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: hash[:],
		Route:       route,
	}

	sendClient, err := b.ht.Alice.RPC.LN.SendToRoute(ctxt)
	require.NoError(b.ht, err, "send to route client")

	err = sendClient.SendMsg(sendReq)
	require.NoError(b.ht, err, "send to route request")

	return sendClient, cancel
}

// interceptFinalHop launches a goroutine to intercept Carol's htlcs and
// returns a closure that can be used to resolve intercepted htlcs.
//
//nolint:lll
func (b *blindedForwardTest) interceptFinalHop() func(routerrpc.ResolveHoldForwardAction) {
	hash := sha256.Sum256(b.preimage[:])
	htlcReceived := make(chan *routerrpc.ForwardHtlcInterceptRequest)

	// Launch a goroutine which will receive from the interceptor and pipe
	// it into our request channel.
	go func() {
		forward, err := b.carolInterceptor.Recv()
		if err != nil {
			b.ht.Fatalf("intercept receive failed: %v", err)
		}

		if !bytes.Equal(forward.PaymentHash, hash[:]) {
			b.ht.Fatalf("unexpected payment hash: %v", hash)
		}

		select {
		case htlcReceived <- forward:

		case <-time.After(lntest.DefaultTimeout):
			b.ht.Fatal("timeout waiting to send intercepted htlc")
		}
	}()

	// Create a closure that will wait for the intercept request and
	// resolve the HTLC with the appropriate action.
	resolve := func(action routerrpc.ResolveHoldForwardAction) {
		select {
		case forward := <-htlcReceived:
			resp := &routerrpc.ForwardHtlcInterceptResponse{
				IncomingCircuitKey: forward.IncomingCircuitKey,
			}

			switch action {
			case routerrpc.ResolveHoldForwardAction_FAIL:
				resp.Action = routerrpc.ResolveHoldForwardAction_FAIL

			case routerrpc.ResolveHoldForwardAction_SETTLE:
				resp.Action = routerrpc.ResolveHoldForwardAction_SETTLE
				resp.Preimage = b.preimage[:]

			case routerrpc.ResolveHoldForwardAction_RESUME:
				resp.Action = routerrpc.ResolveHoldForwardAction_RESUME
			}

			require.NoError(b.ht, b.carolInterceptor.Send(resp))

		case <-time.After(lntest.DefaultTimeout):
			b.ht.Fatal("timeout waiting for htlc intercept")
		}
	}

	return resolve
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

// createBlindedRoute creates a blinded route to the recipient node provided.
// The set of hops is expected to start at the introduction node and end at
// the recipient.
func (b *blindedForwardTest) createBlindedRoute(hops []*forwardingEdge,
	dest *btcec.PublicKey, finalCLTV uint16) *routing.BlindedPayment {

	// Create a path with space for each of our hops + the destination
	// node. We include our passed final cltv delta here because blinded
	// paths include the delta in the blinded portion (not the invoice).
	blindedPayment := &routing.BlindedPayment{
		CltvExpiryDelta: finalCLTV,
	}

	pathLength := len(hops) + 1
	blindedPath := make([]*sphinx.HopInfo, pathLength)

	// Run forwards through our hops to create blinded route data for each
	// node with the next node's short channel id and our payment
	// constraints.
	for i := 0; i < len(hops); i++ {
		node := hops[i]
		scid := node.channelID

		// Set the relay information for this edge based on its policy.
		delta := uint16(node.edge.TimeLockDelta)
		relayInfo := &record.PaymentRelayInfo{
			BaseFee:         uint32(node.edge.FeeBaseMsat),
			FeeRate:         uint32(node.edge.FeeRateMilliMsat),
			CltvExpiryDelta: delta,
		}

		// We set our constraints with our edge's actual htlc min, and
		// an arbitrary maximum expiry (since it's just an anti-probing
		// mechanism).
		constraints := &record.PaymentConstraints{
			HtlcMinimumMsat: lnwire.MilliSatoshi(node.edge.MinHtlc),
			MaxCltvExpiry:   100000,
		}

		// Add CLTV delta of each hop to the blinded payment.
		blindedPayment.CltvExpiryDelta += delta

		// Encode the route's blinded data and include it in the
		// blinded hop.
		payload := record.NewBlindedRouteData(
			scid, nil, *relayInfo, constraints, nil,
		)
		payloadBytes, err := record.EncodeBlindedRouteData(payload)
		require.NoError(b.ht, err)

		blindedPath[i] = &sphinx.HopInfo{
			NodePub:   node.pubkey,
			PlainText: payloadBytes,
		}
	}

	// Next, we'll run backwards through our route to build up the aggregate
	// fees for the blinded payment as a whole. This is done in a separate
	// loop for the sake of readability.
	//
	// For blinded path aggregated fees, we start at the receiving node
	// and add up base an proportional fees *including* the fees that we'll
	// charge on accumulated fees. We use the int ceiling to round up so
	// that the sender will always over-pay, ensuring that we don't round
	// down along the route leaving one forwarding node short of what
	// they're expecting.
	var (
		hopCount                = len(hops) - 1
		currentHopBaseFee       = hops[hopCount].edge.FeeBaseMsat
		currentHopPropFee       = hops[hopCount].edge.FeeRateMilliMsat
		feeParts          int64 = 1e6
	)

	// Note: the spec says to iterate backwards, but then uses n / n +1 to
	// express the "next" hop in the route going backwards. This works for
	// languages where we can iterate backwards and get an increasing
	// index, but since we're counting backwards we use n-1 instead.
	//
	// Specification reference:
	//nolint:lll
	// https://github.com/lightning/bolts/blob/60de4a09727c20dea330f9ee8313034de6e50594/proposals/route-blinding.md?plain=1#L253-L254
	for i := hopCount; i > 0; i-- {
		preceedingBase := hops[i-1].edge.FeeBaseMsat
		preceedingProp := hops[i-1].edge.FeeBaseMsat

		// Separate numerator from ceiling division to break up large
		// lines.
		baseFeeNumerator := preceedingBase*feeParts +
			currentHopBaseFee*(feeParts+preceedingProp)
		currentHopBaseFee = (baseFeeNumerator + feeParts - 1) / feeParts

		propFeeNumerator := (currentHopPropFee+preceedingProp)*
			feeParts + currentHopPropFee*preceedingProp
		currentHopPropFee = (propFeeNumerator + feeParts - 1) / feeParts
	}

	blindedPayment.BaseFee = uint32(currentHopBaseFee)
	blindedPayment.ProportionalFeeRate = uint32(currentHopPropFee)

	// Add our destination node at the end of the path. We don't need to
	// add any forwarding parameters because we're at the final hop.
	payloadBytes, err := record.EncodeBlindedRouteData(
		// TODO: we don't have support for the final hop fields,
		// because only forwarding is supported. We add a next
		// node ID here so that it _looks like_ a valid
		// forwarding hop (though in reality it's the last
		// hop).
		record.NewBlindedRouteData(
			lnwire.NewShortChanIDFromInt(100), nil,
			record.PaymentRelayInfo{}, nil, nil,
		),
	)
	require.NoError(b.ht, err, "final payload")

	blindedPath[pathLength-1] = &sphinx.HopInfo{
		NodePub:   dest,
		PlainText: payloadBytes,
	}

	// Blind the path.
	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(b.ht, err)

	blindedPayment.BlindedPath, err = sphinx.BuildBlindedPath(
		blindingKey, blindedPath,
	)
	require.NoError(b.ht, err, "build blinded path")

	return blindedPayment
}

// forwardingEdge contains the channel id/source public key for a forwarding
// edge and the policy associated with the channel in that direction.
type forwardingEdge struct {
	pubkey    *btcec.PublicKey
	channelID lnwire.ShortChannelID
	edge      *lnrpc.RoutingPolicy
}

func getForwardingEdge(ht *lntest.HarnessTest,
	node *node.HarnessNode, chanID uint64) *forwardingEdge {

	chanInfo := node.RPC.GetChanInfo(&lnrpc.ChanInfoRequest{
		ChanId: chanID,
	})

	pubkey, err := btcec.ParsePubKey(node.PubKey[:])
	require.NoError(ht, err, "%v pubkey", node.Cfg.Name)

	fwdEdge := &forwardingEdge{
		pubkey:    pubkey,
		channelID: lnwire.NewShortChanIDFromInt(chanID),
	}

	if chanInfo.Node1Pub == node.PubKeyStr {
		fwdEdge.edge = chanInfo.Node1Policy
	} else {
		require.Equal(ht, node.PubKeyStr, chanInfo.Node2Pub,
			"policy edge sanity check")

		fwdEdge.edge = chanInfo.Node2Policy
	}

	return fwdEdge
}

// testForwardBlindedRoute tests lnd's ability to forward payments in a blinded
// route.
func testForwardBlindedRoute(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()

	route := testCase.setup(ctx)
	blindedRoute := testCase.createRouteToBlinded(10_000_000, route)

	// Receiving via blinded routes is not yet supported, so Dave won't be
	// able to process the payment.
	//
	// We have an interceptor at our disposal that will catch htlcs as they
	// are forwarded (ie, it won't intercept a HTLC that dave is receiving,
	// since no forwarding occurs). We initiate this interceptor with
	// Carol, so that we can catch it and settle on the outgoing link to
	// Dave. Once we hit the outgoing link, we know that we successfully
	// parsed the htlc, so this is an acceptable compromise.
	// Assert that our interceptor has exited without an error.
	resolveHTLC := testCase.interceptFinalHop()

	// Once our interceptor is set up, we can send the blinded payment.
	testCase.sendBlindedPayment(ctx, blindedRoute)

	// Wait for the HTLC to be active on Alice's channel.
	hash := sha256.Sum256(testCase.preimage[:])
	ht.AssertOutgoingHTLCActive(ht.Alice, testCase.channels[0], hash[:])
	ht.AssertOutgoingHTLCActive(ht.Bob, testCase.channels[1], hash[:])

	// Intercept and settle the HTLC.
	resolveHTLC(routerrpc.ResolveHoldForwardAction_SETTLE)

	// Wait for the HTLC to reflect as settled for Alice.
	preimage, err := lntypes.MakePreimage(testCase.preimage[:])
	require.NoError(ht, err)
	ht.AssertPaymentStatus(ht.Alice, preimage, lnrpc.Payment_SUCCEEDED)

	// Assert that the HTLC has settled before test cleanup runs so that
	// we can cooperatively close all channels.
	ht.AssertHLTCNotActive(ht.Bob, testCase.channels[1], hash[:])
	ht.AssertHLTCNotActive(ht.Alice, testCase.channels[0], hash[:])
}

// Tests handling of errors from the receiving node in a blinded route, testing
// a payment over: Alice -- Bob -- Carol -- Dave, where Bob is the introduction
// node.
//
// Note that at present the payment fails at Dave because we do not yet support
// receiving to blinded routes. In future, we can substitute this test out to
// trigger an IncorrectPaymentDetails failure. In the meantime, this test
// provides valuable coverage for the case where a node in the route is not
// spec compliant (ie, does not return the blinded failure and just uses a
// normal one) because Dave will not appropriately convert the error.
func testReceiverBlindedError(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()
	route := testCase.setup(ctx)

	sendAndResumeBlindedPayment(ctx, ht, testCase, route)
}

// Tests handling of errors from the receiving node in a blinded route, testing
// a payment over: Alice -- Bob -- Carol -- Dave, where Bob is the introduction
// node.
//
// This test shuts down Dave to force a failure at Carol, a node _within_ the
// blinded route and asserts that the error is still converted by the
// introduction node.
func testRelayingBlindedError(ht *lntest.HarnessTest) {
	ctx, testCase := newBlindedForwardTest(ht)
	defer testCase.cleanup()
	route := testCase.setup(ctx)

	// Before we send our payment, drain all of Carol's liquidity
	// so that she can't forward the payment to Dave.
	testCase.drainCarolLiquidity(false)

	// Then dispatch the payment through Carol which will fail due to
	// a lack of liquidity. This check only happens _after_ the interceptor
	// has given the instruction to resume so we can use test
	// infrastructure that will go ahead and intercept the payment.
	sendAndResumeBlindedPayment(ctx, ht, testCase, route)
}

// sendAndResumeBlindedPayment sends a blinded payment through the test
// network provided, intercepting the payment at Carol and allowing it to
// resume. This utility function allows us to ensure that payments at least
// reach Carol and asserts that all errors appear to originate from the
// introduction node.
func sendAndResumeBlindedPayment(ctx context.Context, ht *lntest.HarnessTest,
	testCase *blindedForwardTest, route *routing.BlindedPayment) {

	blindedRoute := testCase.createRouteToBlinded(10_000_000, route)

	// Before we dispatch the payment, spin up a goroutine that will
	// intercept the HTLC on Carol's forward. This allows us to ensure
	// that the HTLC actually reaches the location we expect it to.
	resolveHTLC := testCase.interceptFinalHop()

	// First, test sending the payment all the way through to Dave. We
	// expect this payment to fail, because he does not know how to
	// process payments to a blinded route (not yet supported).
	sendClient, cancel := testCase.sendBlindedPayment(ctx, blindedRoute)
	defer cancel()

	// When Carol intercepts the HTLC, instruct her to resume the payment
	// so that it'll reach Dave and fail.
	resolveHTLC(routerrpc.ResolveHoldForwardAction_RESUME)

	// Make sure that the payment has registered before we try to track it.
	_, err := sendClient.Recv()
	require.NoError(ht, err)

	// Wait for the HTLC to reflect as failed for Alice.
	preimage, err := lntypes.MakePreimage(testCase.preimage[:])
	require.NoError(ht, err)
	pmt := ht.AssertPaymentStatus(ht.Alice, preimage, lnrpc.Payment_FAILED)
	require.Len(ht, pmt.Htlcs, 1)
	require.EqualValues(
		ht, 1, pmt.Htlcs[0].Failure.FailureSourceIndex,
	)
	require.Equal(
		ht, lnrpc.Failure_INVALID_ONION_BLINDING,
		pmt.Htlcs[0].Failure.Code,
	)
}
