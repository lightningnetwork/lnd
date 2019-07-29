package routing

import (
	"bytes"
	"fmt"
	"image/color"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

var uniquePaymentID uint64 = 1 // to be used atomically

type testCtx struct {
	router *ChannelRouter

	graph *channeldb.ChannelGraph

	aliases map[string]route.Vertex

	chain *mockChain

	chainView *mockChainView
}

func (c *testCtx) RestartRouter() error {
	// First, we'll reset the chainView's state as it doesn't persist the
	// filter between restarts.
	c.chainView.Reset()

	// With the chainView reset, we'll now re-create the router itself, and
	// start it.
	router, err := New(Config{
		Graph:              c.graph,
		Chain:              c.chain,
		ChainView:          c.chainView,
		Payer:              &mockPaymentAttemptDispatcher{},
		Control:            makeMockControlTower(),
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,
	})
	if err != nil {
		return fmt.Errorf("unable to create router %v", err)
	}
	if err := router.Start(); err != nil {
		return fmt.Errorf("unable to start router: %v", err)
	}

	// Finally, we'll swap out the pointer in the testCtx with this fresh
	// instance of the router.
	c.router = router
	return nil
}

func copyPubKey(pub *btcec.PublicKey) *btcec.PublicKey {
	return &btcec.PublicKey{
		Curve: btcec.S256(),
		X:     pub.X,
		Y:     pub.Y,
	}
}

func createTestCtxFromGraphInstance(startingHeight uint32, graphInstance *testGraphInstance) (
	*testCtx, func(), error) {

	// We'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send messages won't
	// be populated.
	chain := newMockChain(startingHeight)
	chainView := newMockChainView(chain)

	selfNode, err := graphInstance.graph.SourceNode()
	if err != nil {
		return nil, nil, err
	}

	pathFindingConfig := PathFindingConfig{
		MinProbability:        0.01,
		PaymentAttemptPenalty: 100,
	}

	mcConfig := &MissionControlConfig{
		PenaltyHalfLife:       time.Hour,
		AprioriHopProbability: 0.9,
	}

	mc, err := NewMissionControl(
		graphInstance.graph.Database().DB,
		mcConfig,
	)
	if err != nil {
		return nil, nil, err
	}

	sessionSource := &SessionSource{
		Graph:    graphInstance.graph,
		SelfNode: selfNode,
		QueryBandwidth: func(e *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
			return lnwire.NewMSatFromSatoshis(e.Capacity)
		},
		PathFindingConfig: pathFindingConfig,
		MissionControl:    mc,
	}

	router, err := New(Config{
		Graph:              graphInstance.graph,
		Chain:              chain,
		ChainView:          chainView,
		Payer:              &mockPaymentAttemptDispatcher{},
		Control:            makeMockControlTower(),
		MissionControl:     mc,
		SessionSource:      sessionSource,
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,
		QueryBandwidth: func(e *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
			return lnwire.NewMSatFromSatoshis(e.Capacity)
		},
		NextPaymentID: func() (uint64, error) {
			next := atomic.AddUint64(&uniquePaymentID, 1)
			return next, nil
		},
		PathFindingConfig: pathFindingConfig,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create router %v", err)
	}
	if err := router.Start(); err != nil {
		return nil, nil, fmt.Errorf("unable to start router: %v", err)
	}

	ctx := &testCtx{
		router:    router,
		graph:     graphInstance.graph,
		aliases:   graphInstance.aliasMap,
		chain:     chain,
		chainView: chainView,
	}

	cleanUp := func() {
		ctx.router.Stop()
		graphInstance.cleanUp()
	}

	return ctx, cleanUp, nil
}

func createTestCtxSingleNode(startingHeight uint32) (*testCtx, func(), error) {
	var (
		graph      *channeldb.ChannelGraph
		sourceNode *channeldb.LightningNode
		cleanup    func()
		err        error
	)

	graph, cleanup, err = makeTestGraph()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create test graph: %v", err)
	}

	sourceNode, err = createTestNode()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create source node: %v", err)
	}
	if err = graph.SetSourceNode(sourceNode); err != nil {
		return nil, nil, fmt.Errorf("unable to set source node: %v", err)
	}

	graphInstance := &testGraphInstance{
		graph:   graph,
		cleanUp: cleanup,
	}

	return createTestCtxFromGraphInstance(startingHeight, graphInstance)
}

func createTestCtxFromFile(startingHeight uint32, testGraph string) (*testCtx, func(), error) {
	// We'll attempt to locate and parse out the file
	// that encodes the graph that our tests should be run against.
	graphInstance, err := parseTestGraph(testGraph)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create test graph: %v", err)
	}

	return createTestCtxFromGraphInstance(startingHeight, graphInstance)
}

// TestFindRoutesWithFeeLimit asserts that routes found by the FindRoutes method
// within the channel router contain a total fee less than or equal to the fee
// limit.
func TestFindRoutesWithFeeLimit(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(
		startingBlockHeight, basicGraphFilePath,
	)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// This test will attempt to find routes from roasbeef to sophon for 100
	// satoshis with a fee limit of 10 satoshis. There are two routes from
	// roasbeef to sophon:
	//	1. roasbeef -> songoku -> sophon
	//	2. roasbeef -> phamnuwen -> sophon
	// The second route violates our fee limit, so we should only expect to
	// see the first route.
	target := ctx.aliases["sophon"]
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	restrictions := &RestrictParams{
		FeeLimit:          lnwire.NewMSatFromSatoshis(10),
		ProbabilitySource: noProbabilitySource,
	}

	route, err := ctx.router.FindRoute(
		ctx.router.selfNode.PubKeyBytes,
		target, paymentAmt, restrictions,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	if route.TotalFees() > restrictions.FeeLimit {
		t.Fatalf("route exceeded fee limit: %v", spew.Sdump(route))
	}

	hops := route.Hops
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(hops))
	}

	if hops[0].PubKeyBytes != ctx.aliases["songoku"] {

		t.Fatalf("expected first hop through songoku, got %s",
			getAliasFromPubKey(hops[0].PubKeyBytes,
				ctx.aliases))
	}
}

// TestSendPaymentRouteFailureFallback tests that when sending a payment, if
// one of the target routes is seen as unavailable, then the next route in the
// queue is used instead. This process should continue until either a payment
// succeeds, or all routes have been exhausted.
func TestSendPaymentRouteFailureFallback(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 1000 satoshis, with a maximum of 1000 satoshis in fees.
	var payHash [32]byte
	paymentAmt := lnwire.NewMSatFromSatoshis(1000)
	payment := LightningPayment{
		Target:      ctx.aliases["luoji"],
		Amount:      paymentAmt,
		FeeLimit:    noFeeLimit,
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll modify the SendToSwitch method that's been set within the
	// router's configuration to ignore the path that has luo ji as the
	// first hop. This should force the router to instead take the
	// available two hop path (through satoshi).
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			roasbeefLuoji := lnwire.NewShortChanIDFromInt(689530843)
			if firstHop == roasbeefLuoji {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 0,
					// TODO(roasbeef): temp node failure should be?
					FailureMessage: &lnwire.FailTemporaryChannelFailure{},
				}
			}

			return preImage, nil
		})

	// Send off the payment request to the router, route through satoshi
	// should've been selected as a fall back and succeeded correctly.
	paymentPreImage, route, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The route selected should have two hops
	if len(route.Hops) != 2 {
		t.Fatalf("incorrect route length: expected %v got %v", 2,
			len(route.Hops))
	}

	// The preimage should match up with the once created above.
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}

	// The route should have satoshi as the first hop.
	if route.Hops[0].PubKeyBytes != ctx.aliases["satoshi"] {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes,
				ctx.aliases))
	}
}

// TestChannelUpdateValidation tests that a failed payment with an associated
// channel update will only be applied to the graph when the update contains a
// valid signature.
func TestChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	// Setup a three node network.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	defer testGraph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	const startingBlockHeight = 101

	ctx, cleanUp, err := createTestCtxFromGraphInstance(startingBlockHeight,
		testGraph)

	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Assert that the initially configured fee is retrieved correctly.
	_, policy, _, err := ctx.router.GetChannelByID(
		lnwire.NewShortChanIDFromInt(1))
	if err != nil {
		t.Fatalf("cannot retrieve channel")
	}

	if policy.FeeProportionalMillionths != 400 {
		t.Fatalf("invalid fee")
	}

	// Setup a route from source a to destination c. The route will be used
	// in a call to SendToRoute. SendToRoute also applies channel updates,
	// but it saves us from including RequestRoute in the test scope too.
	hop1 := ctx.aliases["b"]

	hop2 := ctx.aliases["c"]

	hops := []*route.Hop{
		{
			ChannelID:   1,
			PubKeyBytes: hop1,
		},
		{
			ChannelID:   2,
			PubKeyBytes: hop2,
		},
	}

	rt, err := route.NewRouteFromHops(
		lnwire.MilliSatoshi(10000), 100,
		ctx.aliases["a"], hops,
	)
	if err != nil {
		t.Fatalf("unable to create route: %v", err)
	}

	// Set up a channel update message with an invalid signature to be
	// returned to the sender.
	var invalidSignature [64]byte
	errChanUpdate := lnwire.ChannelUpdate{
		Signature:      invalidSignature,
		FeeRate:        500,
		ShortChannelID: lnwire.NewShortChanIDFromInt(1),
		Timestamp:      uint32(testTime.Add(time.Minute).Unix()),
	}

	// We'll modify the SendToSwitch method so that it simulates a failed
	// payment with an error originating from the first hop of the route.
	// The unsigned channel update is attached to the failure message.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			return [32]byte{}, &htlcswitch.ForwardingError{
				FailureSourceIdx: 1,
				FailureMessage: &lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				},
			}
		})

	// The payment parameter is mostly redundant in SendToRoute. Can be left
	// empty for this test.
	var payment lntypes.Hash

	// Send off the payment request to the router. The specified route
	// should be attempted and the channel update should be received by
	// router and ignored because it is missing a valid signature.
	_, err = ctx.router.SendToRoute(payment, rt)
	if err == nil {
		t.Fatalf("expected route to fail with channel update")
	}

	_, policy, _, err = ctx.router.GetChannelByID(
		lnwire.NewShortChanIDFromInt(1))
	if err != nil {
		t.Fatalf("cannot retrieve channel")
	}

	if policy.FeeProportionalMillionths != 400 {
		t.Fatalf("fee updated without valid signature")
	}

	// Next, add a signature to the channel update.
	chanUpdateMsg, err := errChanUpdate.DataToSign()
	if err != nil {
		t.Fatal(err)
	}

	digest := chainhash.DoubleHashB(chanUpdateMsg)
	sig, err := testGraph.privKeyMap["b"].Sign(digest)
	if err != nil {
		t.Fatal(err)
	}

	errChanUpdate.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		t.Fatal(err)
	}

	// Retry the payment using the same route as before.
	_, err = ctx.router.SendToRoute(payment, rt)
	if err == nil {
		t.Fatalf("expected route to fail with channel update")
	}

	// This time a valid signature was supplied and the policy change should
	// have been applied to the graph.
	_, policy, _, err = ctx.router.GetChannelByID(
		lnwire.NewShortChanIDFromInt(1))
	if err != nil {
		t.Fatalf("cannot retrieve channel")
	}

	if policy.FeeProportionalMillionths != 500 {
		t.Fatalf("fee not updated even though signature is valid")
	}
}

// TestSendPaymentErrorRepeatedFeeInsufficient tests that if we receive
// multiple fee related errors from a channel that we're attempting to route
// through, then we'll prune the channel after the second attempt.
func TestSendPaymentErrorRepeatedFeeInsufficient(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 100 satoshis.
	var payHash [32]byte
	amt := lnwire.NewMSatFromSatoshis(1000)
	payment := LightningPayment{
		Target:      ctx.aliases["sophon"],
		Amount:      amt,
		FeeLimit:    noFeeLimit,
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll also fetch the first outgoing channel edge from roasbeef to
	// son goku. We'll obtain this as we'll need to to generate the
	// FeeInsufficient error that we'll send back.
	chanID := uint64(12345)
	_, _, edgeUpdateToFail, err := ctx.graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch chan id: %v", err)
	}

	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(chanID),
		Timestamp:       uint32(edgeUpdateToFail.LastUpdate.Unix()),
		MessageFlags:    edgeUpdateToFail.MessageFlags,
		ChannelFlags:    edgeUpdateToFail.ChannelFlags,
		TimeLockDelta:   edgeUpdateToFail.TimeLockDelta,
		HtlcMinimumMsat: edgeUpdateToFail.MinHTLC,
		HtlcMaximumMsat: edgeUpdateToFail.MaxHTLC,
		BaseFee:         uint32(edgeUpdateToFail.FeeBaseMSat),
		FeeRate:         uint32(edgeUpdateToFail.FeeProportionalMillionths),
	}

	// We'll now modify the SendToSwitch method to return an error for the
	// outgoing channel to Son goku. This will be a fee related error, so
	// it should only cause the edge to be pruned after the second attempt.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			roasbeefSongoku := lnwire.NewShortChanIDFromInt(chanID)
			if firstHop == roasbeefSongoku {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 1,

					// Within our error, we'll add a channel update
					// which is meant to reflect he new fee
					// schedule for the node/channel.
					FailureMessage: &lnwire.FailFeeInsufficient{
						Update: errChanUpdate,
					},
				}
			}

			return preImage, nil
		})

	// Send off the payment request to the router, route through satoshi
	// should've been selected as a fall back and succeeded correctly.
	paymentPreImage, route, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The route selected should have two hops
	if len(route.Hops) != 2 {
		t.Fatalf("incorrect route length: expected %v got %v", 2,
			len(route.Hops))
	}

	// The preimage should match up with the once created above.
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}

	// The route should have pham nuwen as the first hop.
	if route.Hops[0].PubKeyBytes != ctx.aliases["phamnuwen"] {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes,
				ctx.aliases))
	}
}

// TestSendPaymentErrorNonFinalTimeLockErrors tests that if we receive either
// an ExpiryTooSoon or a IncorrectCltvExpiry error from a node, then we prune
// that node from the available graph witin a mission control session. This
// test ensures that we'll route around errors due to nodes not knowing the
// current block height.
func TestSendPaymentErrorNonFinalTimeLockErrors(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to sophon for 1k satoshis.
	var payHash [32]byte
	amt := lnwire.NewMSatFromSatoshis(1000)
	payment := LightningPayment{
		Target:      ctx.aliases["sophon"],
		Amount:      amt,
		FeeLimit:    noFeeLimit,
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll also fetch the first outgoing channel edge from roasbeef to
	// son goku. This edge will be included in the time lock related expiry
	// errors that we'll get back due to disagrements in what the current
	// block height is.
	chanID := uint64(12345)
	roasbeefSongoku := lnwire.NewShortChanIDFromInt(chanID)
	_, _, edgeUpdateToFail, err := ctx.graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch chan id: %v", err)
	}

	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(chanID),
		Timestamp:       uint32(edgeUpdateToFail.LastUpdate.Unix()),
		MessageFlags:    edgeUpdateToFail.MessageFlags,
		ChannelFlags:    edgeUpdateToFail.ChannelFlags,
		TimeLockDelta:   edgeUpdateToFail.TimeLockDelta,
		HtlcMinimumMsat: edgeUpdateToFail.MinHTLC,
		HtlcMaximumMsat: edgeUpdateToFail.MaxHTLC,
		BaseFee:         uint32(edgeUpdateToFail.FeeBaseMSat),
		FeeRate:         uint32(edgeUpdateToFail.FeeProportionalMillionths),
	}

	// We'll now modify the SendToSwitch method to return an error for the
	// outgoing channel to son goku. Since this is a time lock related
	// error, we should fail the payment flow all together, as Goku is the
	// only channel to Sophon.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefSongoku {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 1,
					FailureMessage: &lnwire.FailExpiryTooSoon{
						Update: errChanUpdate,
					},
				}
			}

			return preImage, nil
		})

	// assertExpectedPath is a helper function that asserts the returned
	// route properly routes around the failure we've introduced in the
	// graph.
	assertExpectedPath := func(retPreImage [32]byte, route *route.Route) {
		// The route selected should have two hops
		if len(route.Hops) != 2 {
			t.Fatalf("incorrect route length: expected %v got %v", 2,
				len(route.Hops))
		}

		// The preimage should match up with the once created above.
		if !bytes.Equal(retPreImage[:], preImage[:]) {
			t.Fatalf("incorrect preimage used: expected %x got %x",
				preImage[:], retPreImage[:])
		}

		// The route should have satoshi as the first hop.
		if route.Hops[0].PubKeyBytes != ctx.aliases["phamnuwen"] {

			t.Fatalf("route should go through phamnuwen as first hop, "+
				"instead passes through: %v",
				getAliasFromPubKey(route.Hops[0].PubKeyBytes,
					ctx.aliases))
		}
	}

	// Send off the payment request to the router, this payment should
	// succeed as we should actually go through Pham Nuwen in order to get
	// to Sophon, even though he has higher fees.
	paymentPreImage, rt, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	assertExpectedPath(paymentPreImage, rt)

	// We'll now modify the error return an IncorrectCltvExpiry error
	// instead, this should result in the same behavior of roasbeef routing
	// around the faulty Son Goku node.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefSongoku {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 1,
					FailureMessage: &lnwire.FailIncorrectCltvExpiry{
						Update: errChanUpdate,
					},
				}
			}

			return preImage, nil
		})

	// Once again, Roasbeef should route around Goku since they disagree
	// w.r.t to the block height, and instead go through Pham Nuwen. We
	// flip a bit in the payment hash to allow resending this payment.
	payment.PaymentHash[1] ^= 1
	paymentPreImage, rt, err = ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	assertExpectedPath(paymentPreImage, rt)
}

// TestSendPaymentErrorPathPruning tests that the send of candidate routes
// properly gets pruned in response to ForwardingError response from the
// underlying SendToSwitch function.
func TestSendPaymentErrorPathPruning(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 1000 satoshis, with a maximum of 1000 satoshis in fees.
	var payHash [32]byte
	paymentAmt := lnwire.NewMSatFromSatoshis(1000)
	payment := LightningPayment{
		Target:      ctx.aliases["luoji"],
		Amount:      paymentAmt,
		FeeLimit:    noFeeLimit,
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	roasbeefLuoji := lnwire.NewShortChanIDFromInt(689530843)

	// First, we'll modify the SendToSwitch method to return an error
	// indicating that the channel from roasbeef to luoji is not operable
	// with an UnknownNextPeer.
	//
	// TODO(roasbeef): filtering should be intelligent enough so just not
	// go through satoshi at all at this point.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefLuoji {
				// We'll first simulate an error from the first
				// outgoing link to simulate the channel from luo ji to
				// roasbeef not having enough capacity.
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 0,
					FailureMessage:   &lnwire.FailTemporaryChannelFailure{},
				}
			}

			// Next, we'll create an error from satoshi to indicate
			// that the luoji node is not longer online, which should
			// prune out the rest of the routes.
			roasbeefSatoshi := lnwire.NewShortChanIDFromInt(2340213491)
			if firstHop == roasbeefSatoshi {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 1,
					FailureMessage:   &lnwire.FailUnknownNextPeer{},
				}
			}

			return preImage, nil
		})

	ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()

	// When we try to dispatch that payment, we should receive an error as
	// both attempts should fail and cause both routes to be pruned.
	_, _, err = ctx.router.SendPayment(&payment)
	if err == nil {
		t.Fatalf("payment didn't return error")
	}

	// The final error returned should also indicate that the peer wasn't
	// online (the last error we returned).
	if !strings.Contains(err.Error(), "UnknownNextPeer") {
		t.Fatalf("expected UnknownNextPeer instead got: %v", err)
	}

	ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()

	// Next, we'll modify the SendToSwitch method to indicate that luo ji
	// wasn't originally online. This should also halt the send all
	// together as all paths contain luoji and he can't be reached.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefLuoji {
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 0,
					FailureMessage:   &lnwire.FailUnknownNextPeer{},
				}
			}

			return preImage, nil
		})

	// This shouldn't return an error, as we'll make a payment attempt via
	// the satoshi channel based on the assumption that there might be an
	// intermittent issue with the roasbeef <-> lioji channel.
	paymentPreImage, rt, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable send payment: %v", err)
	}

	// This path should go: roasbeef -> satoshi -> luoji
	if len(rt.Hops) != 2 {
		t.Fatalf("incorrect route length: expected %v got %v", 2,
			len(rt.Hops))
	}
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}
	if rt.Hops[0].PubKeyBytes != ctx.aliases["satoshi"] {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(rt.Hops[0].PubKeyBytes,
				ctx.aliases))
	}

	ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()

	// Finally, we'll modify the SendToSwitch function to indicate that the
	// roasbeef -> luoji channel has insufficient capacity. This should
	// again cause us to instead go via the satoshi route.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefLuoji {
				// We'll first simulate an error from the first
				// outgoing link to simulate the channel from luo ji to
				// roasbeef not having enough capacity.
				return [32]byte{}, &htlcswitch.ForwardingError{
					FailureSourceIdx: 0,
					FailureMessage:   &lnwire.FailTemporaryChannelFailure{},
				}
			}
			return preImage, nil
		})

	// We flip a bit in the payment hash to allow resending this payment.
	payment.PaymentHash[1] ^= 1
	paymentPreImage, rt, err = ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// This should succeed finally.  The route selected should have two
	// hops.
	if len(rt.Hops) != 2 {
		t.Fatalf("incorrect route length: expected %v got %v", 2,
			len(rt.Hops))
	}

	// The preimage should match up with the once created above.
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}

	// The route should have satoshi as the first hop.
	if rt.Hops[0].PubKeyBytes != ctx.aliases["satoshi"] {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(rt.Hops[0].PubKeyBytes,
				ctx.aliases))
	}
}

// TestAddProof checks that we can update the channel proof after channel
// info was added to the database.
func TestAddProof(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtxSingleNode(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Before creating out edge, we'll create two new nodes within the
	// network that the channel will connect.
	node1, err := createTestNode()
	if err != nil {
		t.Fatal(err)
	}
	node2, err := createTestNode()
	if err != nil {
		t.Fatal(err)
	}

	// In order to be able to add the edge we should have a valid funding
	// UTXO within the blockchain.
	fundingTx, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(), bitcoinKey2.SerializeCompressed(),
		100, 0)
	if err != nil {
		t.Fatalf("unable create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	// After utxo was recreated adding the edge without the proof.
	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:     chanID.ToUint64(),
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof:     nil,
	}
	copy(edge.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Now we'll attempt to update the proof and check that it has been
	// properly updated.
	if err := ctx.router.AddProof(*chanID, &testAuthProof); err != nil {
		t.Fatalf("unable to add proof: %v", err)
	}

	info, _, _, err := ctx.router.GetChannelByID(*chanID)
	if info.AuthProof == nil {
		t.Fatal("proof have been updated")
	}
}

// TestIgnoreNodeAnnouncement tests that adding a node to the router that is
// not known from any channel announcement, leads to the announcement being
// ignored.
func TestIgnoreNodeAnnouncement(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight,
		basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	pub := priv1.PubKey()
	node := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node11",
		AuthSigBytes:         testSig.Serialize(),
		Features:             testFeatures,
	}
	copy(node.PubKeyBytes[:], pub.SerializeCompressed())

	err = ctx.router.AddNode(node)
	if !IsError(err, ErrIgnored) {
		t.Fatalf("expected to get ErrIgnore, instead got: %v", err)
	}
}

// TestIgnoreChannelEdgePolicyForUnknownChannel checks that a router will
// ignore a channel policy for a channel not in the graph.
func TestIgnoreChannelEdgePolicyForUnknownChannel(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101

	// Setup an initially empty network.
	testChannels := []*testChannel{}
	testGraph, err := createTestGraphFromChannels(
		testChannels, "roasbeef",
	)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraph.cleanUp()

	ctx, cleanUp, err := createTestCtxFromGraphInstance(
		startingBlockHeight, testGraph,
	)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	var pub1 [33]byte
	copy(pub1[:], priv1.PubKey().SerializeCompressed())

	var pub2 [33]byte
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	// Add the edge between the two unknown nodes to the graph, and check
	// that the nodes are found after the fact.
	fundingTx, _, chanID, err := createChannelEdge(
		ctx, bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(), 10000, 500,
	)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	edgePolicy := &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}

	// Attempt to update the edge. This should be ignored, since the edge
	// is not yet added to the router.
	err = ctx.router.UpdateEdge(edgePolicy)
	if !IsError(err, ErrIgnored) {
		t.Fatalf("expected to get ErrIgnore, instead got: %v", err)
	}

	// Add the edge.
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("expected to be able to add edge to the channel graph,"+
			" even though the vertexes were unknown: %v.", err)
	}

	// Now updating the edge policy should succeed.
	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}
}

// TestAddEdgeUnknownVertexes tests that if an edge is added that contains two
// vertexes which we don't know of, the edge should be available for use
// regardless. This is due to the fact that we don't actually need node
// announcements for the channel vertexes to be able to use the channel.
func TestAddEdgeUnknownVertexes(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight,
		basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	var pub1 [33]byte
	copy(pub1[:], priv1.PubKey().SerializeCompressed())

	var pub2 [33]byte
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	// The two nodes we are about to add should not exist yet.
	_, exists1, err := ctx.graph.HasLightningNode(pub1)
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if exists1 {
		t.Fatalf("node already existed")
	}
	_, exists2, err := ctx.graph.HasLightningNode(pub2)
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if exists2 {
		t.Fatalf("node already existed")
	}

	// Add the edge between the two unknown nodes to the graph, and check
	// that the nodes are found after the fact.
	fundingTx, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("expected to be able to add edge to the channel graph,"+
			" even though the vertexes were unknown: %v.", err)
	}

	// We must add the edge policy to be able to use the edge for route
	// finding.
	edgePolicy := &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 0

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// Create edge in the other direction as well.
	edgePolicy = &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 1

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// After adding the edge between the two previously unknown nodes, they
	// should have been added to the graph.
	_, exists1, err = ctx.graph.HasLightningNode(pub1)
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if !exists1 {
		t.Fatalf("node1 was not added to the graph")
	}
	_, exists2, err = ctx.graph.HasLightningNode(pub2)
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if !exists2 {
		t.Fatalf("node2 was not added to the graph")
	}

	// We will connect node1 to the rest of the test graph, and make sure
	// we can find a route to node2, which will use the just added channel
	// edge.

	// We will connect node 1 to "sophon"
	connectNode := ctx.aliases["sophon"]
	connectNodeKey, err := btcec.ParsePubKey(connectNode[:], btcec.S256())
	if err != nil {
		t.Fatal(err)
	}

	var (
		pubKey1 *btcec.PublicKey
		pubKey2 *btcec.PublicKey
	)
	node1Bytes := priv1.PubKey().SerializeCompressed()
	node2Bytes := connectNode
	if bytes.Compare(node1Bytes[:], node2Bytes[:]) == -1 {
		pubKey1 = priv1.PubKey()
		pubKey2 = connectNodeKey
	} else {
		pubKey1 = connectNodeKey
		pubKey2 = priv1.PubKey()
	}

	fundingTx, _, chanID, err = createChannelEdge(ctx,
		pubKey1.SerializeCompressed(), pubKey2.SerializeCompressed(),
		10000, 510)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock = &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge = &channeldb.ChannelEdgeInfo{
		ChannelID: chanID.ToUint64(),
		AuthProof: nil,
	}
	copy(edge.NodeKey1Bytes[:], node1Bytes)
	edge.NodeKey2Bytes = node2Bytes
	copy(edge.BitcoinKey1Bytes[:], node1Bytes)
	edge.BitcoinKey2Bytes = node2Bytes

	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge to the channel graph: %v.", err)
	}

	edgePolicy = &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 0

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	edgePolicy = &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 1

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// We should now be able to find a route to node 2.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	targetNode := priv2.PubKey()
	var targetPubKeyBytes route.Vertex
	copy(targetPubKeyBytes[:], targetNode.SerializeCompressed())
	_, err = ctx.router.FindRoute(
		ctx.router.selfNode.PubKeyBytes,
		targetPubKeyBytes, paymentAmt, noRestrictions,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	// Now check that we can update the node info for the partial node
	// without messing up the channel graph.
	n1 := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node11",
		AuthSigBytes:         testSig.Serialize(),
		Features:             testFeatures,
	}
	copy(n1.PubKeyBytes[:], priv1.PubKey().SerializeCompressed())

	if err := ctx.router.AddNode(n1); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	n2 := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node22",
		AuthSigBytes:         testSig.Serialize(),
		Features:             testFeatures,
	}
	copy(n2.PubKeyBytes[:], priv2.PubKey().SerializeCompressed())

	if err := ctx.router.AddNode(n2); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	// Should still be able to find the route, and the info should be
	// updated.
	_, err = ctx.router.FindRoute(
		ctx.router.selfNode.PubKeyBytes,
		targetPubKeyBytes, paymentAmt, noRestrictions,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	copy1, err := ctx.graph.FetchLightningNode(priv1.PubKey())
	if err != nil {
		t.Fatalf("unable to fetch node: %v", err)
	}

	if copy1.Alias != n1.Alias {
		t.Fatalf("fetched node not equal to original")
	}

	copy2, err := ctx.graph.FetchLightningNode(priv2.PubKey())
	if err != nil {
		t.Fatalf("unable to fetch node: %v", err)
	}

	if copy2.Alias != n2.Alias {
		t.Fatalf("fetched node not equal to original")
	}
}

// TestWakeUpOnStaleBranch tests that upon startup of the ChannelRouter, if the
// the chain previously reflected in the channel graph is stale (overtaken by a
// longer chain), the channel router will prune the graph for any channels
// confirmed on the stale chain, and resync to the main chain.
func TestWakeUpOnStaleBranch(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxSingleNode(startingBlockHeight)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	const chanValue = 10000

	// chanID1 will not be reorged out.
	var chanID1 uint64

	// chanID2 will be reorged out.
	var chanID2 uint64

	// Create 10 common blocks, confirming chanID1.
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := startingBlockHeight + i
		if i == 5 {
			fundingTx, _, chanID, err := createChannelEdge(ctx,
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID1 = chanID.ToUint64()

		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{})
	}

	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	_, forkHeight, err := ctx.chain.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to ge best block: %v", err)
	}

	// Create 10 blocks on the minority chain, confirming chanID2.
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		if i == 5 {
			fundingTx, _, chanID, err := createChannelEdge(ctx,
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID2 = chanID.ToUint64()
		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{})
	}
	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	// Now add the two edges to the channel graph, and check that they
	// correctly show up in the database.
	node1, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	edge1 := &channeldb.ChannelEdgeInfo{
		ChannelID:     chanID1,
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &channeldb.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.router.AddEdge(edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge2 := &channeldb.ChannelEdgeInfo{
		ChannelID:     chanID2,
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &channeldb.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
	}
	copy(edge2.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge2.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.router.AddEdge(edge2); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Check that the fundingTxs are in the graph db.
	_, _, has, isZombie, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// Stop the router, so we can reorg the chain while its offline.
	if err := ctx.router.Stop(); err != nil {
		t.Fatalf("unable to stop router: %v", err)
	}

	// Create a 15 block fork.
	for i := uint32(1); i <= 15; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
	}

	// Give time to process new blocks.
	time.Sleep(time.Millisecond * 500)

	// Create new router with same graph database.
	router, err := New(Config{
		Graph:              ctx.graph,
		Chain:              ctx.chain,
		ChainView:          ctx.chainView,
		Payer:              &mockPaymentAttemptDispatcher{},
		Control:            makeMockControlTower(),
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,
	})
	if err != nil {
		t.Fatalf("unable to create router %v", err)
	}

	// It should resync to the longer chain on startup.
	if err := router.Start(); err != nil {
		t.Fatalf("unable to start router: %v", err)
	}

	// The channel with chanID2 should not be in the database anymore,
	// since it is not confirmed on the longest chain. chanID1 should
	// still be.
	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("did not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
	}
	if isZombie {
		t.Fatal("reorged edge should not be marked as zombie")
	}
}

// TestDisconnectedBlocks checks that the router handles a reorg happening when
// it is active.
func TestDisconnectedBlocks(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxSingleNode(startingBlockHeight)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	const chanValue = 10000

	// chanID1 will not be reorged out, while chanID2 will be reorged out.
	var chanID1, chanID2 uint64

	// Create 10 common blocks, confirming chanID1.
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := startingBlockHeight + i
		if i == 5 {
			fundingTx, _, chanID, err := createChannelEdge(ctx,
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID1 = chanID.ToUint64()

		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{})
	}

	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	_, forkHeight, err := ctx.chain.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get best block: %v", err)
	}

	// Create 10 blocks on the minority chain, confirming chanID2.
	var minorityChain []*wire.MsgBlock
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		if i == 5 {
			fundingTx, _, chanID, err := createChannelEdge(ctx,
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID2 = chanID.ToUint64()
		}
		minorityChain = append(minorityChain, block)
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{})
	}
	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	// Now add the two edges to the channel graph, and check that they
	// correctly show up in the database.
	node1, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	edge1 := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID1,
		NodeKey1Bytes:    node1.PubKeyBytes,
		NodeKey2Bytes:    node2.PubKeyBytes,
		BitcoinKey1Bytes: node1.PubKeyBytes,
		BitcoinKey2Bytes: node2.PubKeyBytes,
		AuthProof: &channeldb.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.router.AddEdge(edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge2 := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID2,
		NodeKey1Bytes:    node1.PubKeyBytes,
		NodeKey2Bytes:    node2.PubKeyBytes,
		BitcoinKey1Bytes: node1.PubKeyBytes,
		BitcoinKey2Bytes: node2.PubKeyBytes,
		AuthProof: &channeldb.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
	}
	copy(edge2.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge2.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.router.AddEdge(edge2); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Check that the fundingTxs are in the graph db.
	_, _, has, isZombie, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// Create a 15 block fork. We first let the chainView notify the router
	// about stale blocks, before sending the now connected blocks. We do
	// this because we expect this order from the chainview.
	for i := len(minorityChain) - 1; i >= 0; i-- {
		block := minorityChain[i]
		height := uint32(forkHeight) + uint32(i) + 1
		ctx.chainView.notifyStaleBlock(block.BlockHash(), height,
			block.Transactions)
	}
	for i := uint32(1); i <= 15; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			block.Transactions)
	}

	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	// chanID2 should not be in the database anymore, since it is not
	// confirmed on the longest chain. chanID1 should still be.
	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("did not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
	}
	if isZombie {
		t.Fatal("reorged edge should not be marked as zombie")
	}
}

// TestChansClosedOfflinePruneGraph tests that if channels we know of are
// closed while we're offline, then once we resume operation of the
// ChannelRouter, then the channels are properly pruned.
func TestRouterChansClosedOfflinePruneGraph(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxSingleNode(startingBlockHeight)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	const chanValue = 10000

	// First, we'll create a channel, to be mined shortly at height 102.
	block102 := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	}
	nextHeight := startingBlockHeight + 1
	fundingTx1, chanUTXO, chanID1, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		chanValue, uint32(nextHeight))
	if err != nil {
		t.Fatalf("unable create channel edge: %v", err)
	}
	block102.Transactions = append(block102.Transactions, fundingTx1)
	ctx.chain.addBlock(block102, uint32(nextHeight), rand.Uint32())
	ctx.chain.setBestBlock(int32(nextHeight))
	ctx.chainView.notifyBlock(block102.BlockHash(), uint32(nextHeight),
		[]*wire.MsgTx{})

	// We'll now create the edges and nodes within the database required
	// for the ChannelRouter to properly recognize the channel we added
	// above.
	node1, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	edge1 := &channeldb.ChannelEdgeInfo{
		ChannelID:     chanID1.ToUint64(),
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &channeldb.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())
	if err := ctx.router.AddEdge(edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// The router should now be aware of the channel we created above.
	_, _, hasChan, isZombie, err := ctx.graph.HasChannelEdge(chanID1.ToUint64())
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !hasChan {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// With the transaction included, and the router's database state
	// updated, we'll now mine 5 additional blocks on top of it.
	for i := 0; i < 5; i++ {
		nextHeight++

		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		ctx.chain.addBlock(block, uint32(nextHeight), rand.Uint32())
		ctx.chain.setBestBlock(int32(nextHeight))
		ctx.chainView.notifyBlock(block.BlockHash(), uint32(nextHeight),
			[]*wire.MsgTx{})
	}

	// At this point, our starting height should be 107.
	_, chainHeight, err := ctx.chain.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get best block: %v", err)
	}
	if chainHeight != 107 {
		t.Fatalf("incorrect chain height: expected %v, got %v",
			107, chainHeight)
	}

	// Next, we'll "shut down" the router in order to simulate downtime.
	if err := ctx.router.Stop(); err != nil {
		t.Fatalf("unable to shutdown router: %v", err)
	}

	// While the router is "offline" we'll mine 5 additional blocks, with
	// the second block closing the channel we created above.
	for i := 0; i < 5; i++ {
		nextHeight++

		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}

		if i == 2 {
			// For the second block, we'll add a transaction that
			// closes the channel we created above by spending the
			// output.
			closingTx := wire.NewMsgTx(2)
			closingTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: *chanUTXO,
			})
			block.Transactions = append(block.Transactions,
				closingTx)
		}

		ctx.chain.addBlock(block, uint32(nextHeight), rand.Uint32())
		ctx.chain.setBestBlock(int32(nextHeight))
		ctx.chainView.notifyBlock(block.BlockHash(), uint32(nextHeight),
			[]*wire.MsgTx{})
	}

	// At this point, our starting height should be 112.
	_, chainHeight, err = ctx.chain.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get best block: %v", err)
	}
	if chainHeight != 112 {
		t.Fatalf("incorrect chain height: expected %v, got %v",
			112, chainHeight)
	}

	// Now we'll re-start the ChannelRouter. It should recognize that it's
	// behind the main chain and prune all the blocks that it missed while
	// it was down.
	ctx.RestartRouter()

	// At this point, the channel that was pruned should no longer be known
	// by the router.
	_, _, hasChan, isZombie, err = ctx.graph.HasChannelEdge(chanID1.ToUint64())
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if hasChan {
		t.Fatalf("channel was found in graph but shouldn't have been")
	}
	if isZombie {
		t.Fatal("closed channel should not be marked as zombie")
	}
}

// TestPruneChannelGraphStaleEdges ensures that we properly prune stale edges
// from the channel graph.
func TestPruneChannelGraphStaleEdges(t *testing.T) {
	t.Parallel()

	freshTimestamp := time.Now()
	staleTimestamp := time.Unix(0, 0)

	// We'll create the following test graph so that only the last channel
	// is pruned.
	testChannels := []*testChannel{
		// No edges.
		{
			Node1:     &testChannelEnd{Alias: "a"},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Only one edge with a stale timestamp.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: staleTimestamp,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 2,
		},

		// Only one edge with a fresh timestamp.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: freshTimestamp,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 3,
		},

		// One edge fresh, one edge stale.
		{
			Node1: &testChannelEnd{
				Alias: "c",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: freshTimestamp,
				},
			},
			Node2: &testChannelEnd{
				Alias: "d",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: staleTimestamp,
				},
			},
			Capacity:  100000,
			ChannelID: 4,
		},

		// Both edges fresh.
		symmetricTestChannel("g", "h", 100000, &testChannelPolicy{
			LastUpdate: freshTimestamp,
		}, 5),

		// Both edges stale, only one pruned.
		symmetricTestChannel("e", "f", 100000, &testChannelPolicy{
			LastUpdate: staleTimestamp,
		}, 6),
	}

	// We'll create our test graph and router backed with these test
	// channels we've created.
	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	if err != nil {
		t.Fatalf("unable to create test graph: %v", err)
	}
	defer testGraph.cleanUp()

	const startingHeight = 100
	ctx, cleanUp, err := createTestCtxFromGraphInstance(
		startingHeight, testGraph,
	)
	if err != nil {
		t.Fatalf("unable to create test context: %v", err)
	}
	defer cleanUp()

	// All of the channels should exist before pruning them.
	assertChannelsPruned(t, ctx.graph, testChannels)

	// Proceed to prune the channels - only the last one should be pruned.
	if err := ctx.router.pruneZombieChans(); err != nil {
		t.Fatalf("unable to prune zombie channels: %v", err)
	}

	prunedChannel := testChannels[len(testChannels)-1].ChannelID
	assertChannelsPruned(t, ctx.graph, testChannels, prunedChannel)
}

// TestPruneChannelGraphDoubleDisabled test that we can properly prune channels
// with both edges disabled from our channel graph.
func TestPruneChannelGraphDoubleDisabled(t *testing.T) {
	t.Parallel()

	// We'll create the following test graph so that only the last channel
	// is pruned. We'll use a fresh timestamp to ensure they're not pruned
	// according to that heuristic.
	timestamp := time.Now()
	testChannels := []*testChannel{
		// Channel from self shouldn't be pruned.
		symmetricTestChannel(
			"self", "a", 100000, &testChannelPolicy{
				LastUpdate: timestamp,
				Disabled:   true,
			}, 99,
		),

		// No edges.
		{
			Node1:     &testChannelEnd{Alias: "a"},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Only one edge disabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: timestamp,
					Disabled:   true,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 2,
		},

		// Only one edge enabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: timestamp,
					Disabled:   false,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 3,
		},

		// One edge disabled, one edge enabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: timestamp,
					Disabled:   true,
				},
			},
			Node2: &testChannelEnd{
				Alias: "b",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: timestamp,
					Disabled:   false,
				},
			},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Both edges enabled.
		symmetricTestChannel("c", "d", 100000, &testChannelPolicy{
			LastUpdate: timestamp,
			Disabled:   false,
		}, 2),

		// Both edges disabled, only one pruned.
		symmetricTestChannel("e", "f", 100000, &testChannelPolicy{
			LastUpdate: timestamp,
			Disabled:   true,
		}, 3),
	}

	// We'll create our test graph and router backed with these test
	// channels we've created.
	testGraph, err := createTestGraphFromChannels(testChannels, "self")
	if err != nil {
		t.Fatalf("unable to create test graph: %v", err)
	}
	defer testGraph.cleanUp()

	const startingHeight = 100
	ctx, cleanUp, err := createTestCtxFromGraphInstance(
		startingHeight, testGraph,
	)
	if err != nil {
		t.Fatalf("unable to create test context: %v", err)
	}
	defer cleanUp()

	// All the channels should exist within the graph before pruning them.
	assertChannelsPruned(t, ctx.graph, testChannels)

	// If we attempt to prune them without AssumeChannelValid being set,
	// none should be pruned.
	if err := ctx.router.pruneZombieChans(); err != nil {
		t.Fatalf("unable to prune zombie channels: %v", err)
	}

	assertChannelsPruned(t, ctx.graph, testChannels)

	// Now that AssumeChannelValid is set, we'll prune the graph again and
	// the last channel should be the only one pruned.
	ctx.router.cfg.AssumeChannelValid = true
	if err := ctx.router.pruneZombieChans(); err != nil {
		t.Fatalf("unable to prune zombie channels: %v", err)
	}

	prunedChannel := testChannels[len(testChannels)-1].ChannelID
	assertChannelsPruned(t, ctx.graph, testChannels, prunedChannel)
}

// TestFindPathFeeWeighting tests that the findPath method will properly prefer
// routes with lower fees over routes with lower time lock values. This is
// meant to exercise the fact that the internal findPath method ranks edges
// with the square of the total fee in order bias towards lower fees.
func TestFindPathFeeWeighting(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	sourceNode, err := ctx.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	amt := lnwire.MilliSatoshi(100)

	target := ctx.aliases["luoji"]

	// We'll now attempt a path finding attempt using this set up. Due to
	// the edge weighting, we should select the direct path over the 2 hop
	// path even though the direct path has a higher potential time lock.
	path, err := findPath(
		&graphParams{
			graph: ctx.graph,
		},
		noRestrictions,
		testPathFindingConfig,
		sourceNode.PubKeyBytes, target, amt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	// The route that was chosen should be exactly one hop, and should be
	// directly to luoji.
	if len(path) != 1 {
		t.Fatalf("expected path length of 1, instead was: %v", len(path))
	}
	if path[0].Node.Alias != "luoji" {
		t.Fatalf("wrong node: %v", path[0].Node.Alias)
	}
}

// TestIsStaleNode tests that the IsStaleNode method properly detects stale
// node announcements.
func TestIsStaleNode(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxSingleNode(startingBlockHeight)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Before we can insert a node in to the database, we need to create a
	// channel that it's linked to.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	fundingTx, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Before we add the node, if we query for staleness, we should get
	// false, as we haven't added the full node.
	updateTimeStamp := time.Unix(123, 0)
	if ctx.router.IsStaleNode(pub1, updateTimeStamp) {
		t.Fatalf("incorrectly detected node as stale")
	}

	// With the node stub in the database, we'll add the fully node
	// announcement to the database.
	n1 := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           updateTimeStamp,
		Addresses:            testAddrs,
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node11",
		AuthSigBytes:         testSig.Serialize(),
		Features:             testFeatures,
	}
	copy(n1.PubKeyBytes[:], priv1.PubKey().SerializeCompressed())
	if err := ctx.router.AddNode(n1); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	// If we use the same timestamp and query for staleness, we should get
	// true.
	if !ctx.router.IsStaleNode(pub1, updateTimeStamp) {
		t.Fatalf("failure to detect stale node update")
	}

	// If we update the timestamp and once again query for staleness, it
	// should report false.
	newTimeStamp := time.Unix(1234, 0)
	if ctx.router.IsStaleNode(pub1, newTimeStamp) {
		t.Fatalf("incorrectly detected node as stale")
	}
}

// TestIsKnownEdge tests that the IsKnownEdge method properly detects stale
// channel announcements.
func TestIsKnownEdge(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxSingleNode(startingBlockHeight)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// First, we'll create a new channel edge (just the info) and insert it
	// into the database.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	fundingTx, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Now that the edge has been inserted, query is the router already
	// knows of the edge should return true.
	if !ctx.router.IsKnownEdge(*chanID) {
		t.Fatalf("router should detect edge as known")
	}
}

// TestIsStaleEdgePolicy tests that the IsStaleEdgePolicy properly detects
// stale channel edge update announcements.
func TestIsStaleEdgePolicy(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight,
		basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// First, we'll create a new channel edge (just the info) and insert it
	// into the database.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	fundingTx, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	// If we query for staleness before adding the edge, we should get
	// false.
	updateTimeStamp := time.Unix(123, 0)
	if ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
	if ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect fresh edge policy")
	}

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// We'll also add two edge policies, one for each direction.
	edgePolicy := &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                updateTimeStamp,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 0
	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	edgePolicy = &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                updateTimeStamp,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 1
	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// Now that the edges have been added, an identical (chanID, flag,
	// timestamp) tuple for each edge should be detected as a stale edge.
	if !ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect stale edge policy")
	}
	if !ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect stale edge policy")
	}

	// If we now update the timestamp for both edges, the router should
	// detect that this tuple represents a fresh edge.
	updateTimeStamp = time.Unix(9999, 0)
	if ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
	if ctx.router.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
}

// TestEmptyRoutesGenerateSphinxPacket tests that the generateSphinxPacket
// function is able to gracefully handle being passed a nil set of hops for the
// route by the caller.
func TestEmptyRoutesGenerateSphinxPacket(t *testing.T) {
	t.Parallel()

	sessionKey, _ := btcec.NewPrivateKey(btcec.S256())
	emptyRoute := &route.Route{}
	_, _, err := generateSphinxPacket(emptyRoute, testHash[:], sessionKey)
	if err != route.ErrNoRouteHopsProvided {
		t.Fatalf("expected empty hops error: instead got: %v", err)
	}
}

// TestUnknownErrorSource tests that if the source of an error is unknown, all
// edges along the route will be pruned.
func TestUnknownErrorSource(t *testing.T) {
	t.Parallel()

	// Setup a network. It contains two paths to c: a->b->c and an
	// alternative a->d->c.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 3),
		symmetricTestChannel("a", "d", chanCapSat, &testChannelPolicy{
			Expiry:      144,
			FeeRate:     400,
			FeeBaseMsat: 100000,
			MinHTLC:     1,
			MaxHTLC:     lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
		symmetricTestChannel("d", "c", chanCapSat, &testChannelPolicy{
			Expiry:      144,
			FeeRate:     400,
			FeeBaseMsat: 100000,
			MinHTLC:     1,
			MaxHTLC:     lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 4),
	}

	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	defer testGraph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	const startingBlockHeight = 101

	ctx, cleanUp, err := createTestCtxFromGraphInstance(startingBlockHeight,
		testGraph)

	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Create a payment to node c.
	payment := LightningPayment{
		Target:      ctx.aliases["c"],
		Amount:      lnwire.NewMSatFromSatoshis(1000),
		FeeLimit:    noFeeLimit,
		PaymentHash: lntypes.Hash{},
	}

	// We'll modify the SendToSwitch method so that it simulates hop b as a
	// node that returns an unparsable failure if approached via the a->b
	// channel.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			// If channel a->b is used, return an error without
			// source and message. The sender won't know the origin
			// of the error.
			if firstHop.ToUint64() == 1 {
				return [32]byte{},
					htlcswitch.ErrUnreadableFailureMessage
			}

			// Otherwise the payment succeeds.
			return lntypes.Preimage{}, nil
		})

	// Send off the payment request to the router. The expectation is that
	// the route a->b->c is tried first. An unreadable faiure is returned
	// which should pruning the channel a->b. We expect the payment to
	// succeed via a->d.
	_, _, err = ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("expected payment to succeed, but got: %v", err)
	}

	// Next we modify payment result to return an unknown failure.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			// If channel a->b is used, simulate that the failure
			// couldn't be decoded (FailureMessage is nil).
			if firstHop.ToUint64() == 2 {
				return [32]byte{},
					&htlcswitch.ForwardingError{
						FailureSourceIdx: 1,
					}
			}

			// Otherwise the payment succeeds.
			return lntypes.Preimage{}, nil
		})

	// Send off the payment request to the router. We expect the payment to
	// fail because both routes have been pruned.
	payment.PaymentHash = lntypes.Hash{1}
	_, _, err = ctx.router.SendPayment(&payment)
	if err == nil {
		t.Fatalf("expected payment to fail")
	}
}

// assertChannelsPruned ensures that only the given channels are pruned from the
// graph out of the set of all channels.
func assertChannelsPruned(t *testing.T, graph *channeldb.ChannelGraph,
	channels []*testChannel, prunedChanIDs ...uint64) {

	t.Helper()

	pruned := make(map[uint64]struct{}, len(channels))
	for _, chanID := range prunedChanIDs {
		pruned[chanID] = struct{}{}
	}

	for _, channel := range channels {
		_, shouldPrune := pruned[channel.ChannelID]
		_, _, exists, isZombie, err := graph.HasChannelEdge(
			channel.ChannelID,
		)
		if err != nil {
			t.Fatalf("unable to determine existence of "+
				"channel=%v in the graph: %v",
				channel.ChannelID, err)
		}
		if !shouldPrune && !exists {
			t.Fatalf("expected channel=%v to exist within "+
				"the graph", channel.ChannelID)
		}
		if shouldPrune && exists {
			t.Fatalf("expected channel=%v to not exist "+
				"within the graph", channel.ChannelID)
		}
		if !shouldPrune && isZombie {
			t.Fatalf("expected channel=%v to not be marked "+
				"as zombie", channel.ChannelID)
		}
		if shouldPrune && !isZombie {
			t.Fatalf("expected channel=%v to be marked as "+
				"zombie", channel.ChannelID)
		}
	}
}

// TestRouterPaymentStateMachine tests that the router interacts as expected
// with the ControlTower during a payment lifecycle, such that it payment
// attempts are not sent twice to the switch, and results are handled after a
// restart.
func TestRouterPaymentStateMachine(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101

	// Setup two simple channels such that we can mock sending along this
	// route.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraph.cleanUp()

	hop1 := testGraph.aliasMap["b"]
	hop2 := testGraph.aliasMap["c"]
	hops := []*route.Hop{
		{
			ChannelID:   1,
			PubKeyBytes: hop1,
		},
		{
			ChannelID:   2,
			PubKeyBytes: hop2,
		},
	}

	// We create a simple route that we will supply every time the router
	// requests one.
	rt, err := route.NewRouteFromHops(
		lnwire.MilliSatoshi(10000), 100, testGraph.aliasMap["a"], hops,
	)
	if err != nil {
		t.Fatalf("unable to create route: %v", err)
	}

	// A payment state machine test case consists of several ordered steps,
	// that we use for driving the scenario.
	type testCase struct {
		// steps is a list of steps to perform during the testcase.
		steps []string

		// routes is the sequence of routes we will provide to the
		// router when it requests a new route.
		routes []*route.Route
	}

	const (
		// routerInitPayment is a test step where we expect the router
		// to call the InitPayment method on the control tower.
		routerInitPayment = "Router:init-payment"

		// routerRegisterAttempt is a test step where we expect the
		// router to call the RegisterAttempt method on the control
		// tower.
		routerRegisterAttempt = "Router:register-attempt"

		// routerSuccess is a test step where we expect the router to
		// call the Success method on the control tower.
		routerSuccess = "Router:success"

		// routerFail is a test step where we expect the router to call
		// the Fail method on the control tower.
		routerFail = "Router:fail"

		// sendToSwitchSuccess is a step where we expect the router to
		// call send the payment attempt to the switch, and we will
		// respond with a non-error, indicating that the payment
		// attempt was successfully forwarded.
		sendToSwitchSuccess = "SendToSwitch:success"

		// sendToSwitchResultFailure is a step where we expect the
		// router to send the payment attempt to the switch, and we
		// will respond with a forwarding error. This can happen when
		// forwarding fail on our local links.
		sendToSwitchResultFailure = "SendToSwitch:failure"

		// getPaymentResultSuccess is a test step where we expect the
		// router to call the GetPaymentResult method, and we will
		// respond with a successful payment result.
		getPaymentResultSuccess = "GetPaymentResult:success"

		// getPaymentResultFailure is a test step where we expect the
		// router to call the GetPaymentResult method, and we will
		// respond with a forwarding error.
		getPaymentResultFailure = "GetPaymentResult:failure"

		// resendPayment is a test step where we manually try to resend
		// the same payment, making sure the router responds with an
		// error indicating that it is alreayd in flight.
		resendPayment = "ResendPayment"

		// startRouter is a step where we manually start the router,
		// used to test that it automatically will resume payments at
		// startup.
		startRouter = "StartRouter"

		// stopRouter is a test step where we manually make the router
		// shut down.
		stopRouter = "StopRouter"

		// paymentSuccess is a step where assert that we receive a
		// successful result for the original payment made.
		paymentSuccess = "PaymentSuccess"

		// paymentError is a step where assert that we receive an error
		// for the original payment made.
		paymentError = "PaymentError"

		// resentPaymentSuccess is a step where assert that we receive
		// a successful result for a payment that was resent.
		resentPaymentSuccess = "ResentPaymentSuccess"

		// resentPaymentError is a step where assert that we receive an
		// error for a payment that was resent.
		resentPaymentError = "ResentPaymentError"
	)

	tests := []testCase{
		{
			// Tests a normal payment flow that succeeds.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,
				getPaymentResultSuccess,
				routerSuccess,
				paymentSuccess,
			},
			routes: []*route.Route{rt},
		},
		{
			// A payment flow with a failure on the first attempt,
			// but that succeeds on the second attempt.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the first sent attempt fail.
				getPaymentResultFailure,

				// The router should retry.
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the second sent attempt succeed.
				getPaymentResultSuccess,
				routerSuccess,
				paymentSuccess,
			},
			routes: []*route.Route{rt, rt},
		},
		{
			// A payment flow with a forwarding failure first time
			// sending to the switch, but that succeeds on the
			// second attempt.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,

				// Make the first sent attempt fail.
				sendToSwitchResultFailure,

				// The router should retry.
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the second sent attempt succeed.
				getPaymentResultSuccess,
				routerSuccess,
				paymentSuccess,
			},
			routes: []*route.Route{rt, rt},
		},
		{
			// A payment that fails on the first attempt, and has
			// only one route available to try. It will therefore
			// fail permanently.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the first sent attempt fail.
				getPaymentResultFailure,

				// Since there are no more routes to try, the
				// payment should fail.
				routerFail,
				paymentError,
			},
			routes: []*route.Route{rt},
		},
		{
			// We expect the payment to fail immediately if we have
			// no routes to try.
			steps: []string{
				routerInitPayment,
				routerFail,
				paymentError,
			},
			routes: []*route.Route{},
		},
		{
			// A normal payment flow, where we attempt to resend
			// the same payment after each step. This ensures that
			// the router don't attempt to resend a payment already
			// in flight.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,

				// Manually resend the payment, the router
				// should attempt to init with the control
				// tower, but fail since it is already in
				// flight.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// The original payment should proceed as
				// normal.
				sendToSwitchSuccess,

				// Again resend the payment and assert it's not
				// allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// Notify about a success for the original
				// payment.
				getPaymentResultSuccess,
				routerSuccess,

				// Now that the original payment finished,
				// resend it again to ensure this is not
				// allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,
				paymentSuccess,
			},
			routes: []*route.Route{rt},
		},
		{
			// Tests that the router is able to handle the
			// receieved payment result after a restart.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Shut down the router. The original caller
				// should get notified about this.
				stopRouter,
				paymentError,

				// Start the router again, and ensure the
				// router registers the success with the
				// control tower.
				startRouter,
				getPaymentResultSuccess,
				routerSuccess,
			},
			routes: []*route.Route{rt},
		},
		{
			// Tests that we are allowed to resend a payment after
			// it has permanently failed.
			steps: []string{
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Resending the payment at this stage should
				// not be allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// Make the first attempt fail.
				getPaymentResultFailure,
				routerFail,

				// Since we have no more routes to try, the
				// original payment should fail.
				paymentError,

				// Now resend the payment again. This should be
				// allowed, since the payment has failed.
				resendPayment,
				routerInitPayment,
				routerRegisterAttempt,
				sendToSwitchSuccess,
				getPaymentResultSuccess,
				routerSuccess,
				resentPaymentSuccess,
			},
			routes: []*route.Route{rt},
		},
	}

	// Create a mock control tower with channels set up, that we use to
	// synchronize and listen for events.
	control := makeMockControlTower()
	control.init = make(chan initArgs)
	control.register = make(chan registerArgs)
	control.success = make(chan successArgs)
	control.fail = make(chan failArgs)
	control.fetchInFlight = make(chan struct{})

	quit := make(chan struct{})
	defer close(quit)

	// setupRouter is a helper method that creates and starts the router in
	// the desired configuration for this test.
	setupRouter := func() (*ChannelRouter, chan error,
		chan *htlcswitch.PaymentResult, chan error) {

		chain := newMockChain(startingBlockHeight)
		chainView := newMockChainView(chain)

		// We set uo the use the following channels and a mock Payer to
		// synchonize with the interaction to the Switch.
		sendResult := make(chan error)
		paymentResultErr := make(chan error)
		paymentResult := make(chan *htlcswitch.PaymentResult)

		payer := &mockPayer{
			sendResult:       sendResult,
			paymentResult:    paymentResult,
			paymentResultErr: paymentResultErr,
		}

		router, err := New(Config{
			Graph:              testGraph.graph,
			Chain:              chain,
			ChainView:          chainView,
			Control:            control,
			SessionSource:      &mockPaymentSessionSource{},
			MissionControl:     &mockMissionControl{},
			Payer:              payer,
			ChannelPruneExpiry: time.Hour * 24,
			GraphPruneInterval: time.Hour * 2,
			QueryBandwidth: func(e *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
				return lnwire.NewMSatFromSatoshis(e.Capacity)
			},
			NextPaymentID: func() (uint64, error) {
				next := atomic.AddUint64(&uniquePaymentID, 1)
				return next, nil
			},
		})
		if err != nil {
			t.Fatalf("unable to create router %v", err)
		}

		// On startup, the router should fetch all pending payments
		// from the ControlTower, so assert that here.
		didFetch := make(chan struct{})
		go func() {
			select {
			case <-control.fetchInFlight:
				close(didFetch)
			case <-time.After(1 * time.Second):
				t.Fatalf("router did not fetch in flight " +
					"payments")
			}
		}()

		if err := router.Start(); err != nil {
			t.Fatalf("unable to start router: %v", err)
		}

		select {
		case <-didFetch:
		case <-time.After(1 * time.Second):
			t.Fatalf("did not fetch in flight payments at startup")
		}

		return router, sendResult, paymentResult, paymentResultErr
	}

	router, sendResult, getPaymentResult, getPaymentResultErr := setupRouter()
	defer router.Stop()

	for _, test := range tests {
		// Craft a LightningPayment struct.
		var preImage lntypes.Preimage
		if _, err := rand.Read(preImage[:]); err != nil {
			t.Fatalf("unable to generate preimage")
		}

		payHash := preImage.Hash()

		paymentAmt := lnwire.NewMSatFromSatoshis(1000)
		payment := LightningPayment{
			Target:      testGraph.aliasMap["c"],
			Amount:      paymentAmt,
			FeeLimit:    noFeeLimit,
			PaymentHash: payHash,
		}

		copy(preImage[:], bytes.Repeat([]byte{9}, 32))

		router.cfg.SessionSource = &mockPaymentSessionSource{
			routes: test.routes,
		}

		router.cfg.MissionControl = &mockMissionControl{}

		// Send the payment. Since this is new payment hash, the
		// information should be registered with the ControlTower.
		paymentResult := make(chan error)
		go func() {
			_, _, err := router.SendPayment(&payment)
			paymentResult <- err
		}()

		var resendResult chan error
		for _, step := range test.steps {
			switch step {

			case routerInitPayment:
				var args initArgs
				select {
				case args = <-control.init:
				case <-time.After(1 * time.Second):
					t.Fatalf("no init payment with control")
				}

				if args.c == nil {
					t.Fatalf("expected non-nil CreationInfo")
				}

			// In this step we expect the router to make a call to
			// register a new attempt with the ControlTower.
			case routerRegisterAttempt:
				var args registerArgs
				select {
				case args = <-control.register:
				case <-time.After(1 * time.Second):
					t.Fatalf("not registered with control")
				}

				if args.a == nil {
					t.Fatalf("expected non-nil AttemptInfo")
				}

			// In this step we expect the router to call the
			// ControlTower's Succcess method with the preimage.
			case routerSuccess:
				select {
				case _ = <-control.success:
				case <-time.After(1 * time.Second):
					t.Fatalf("not registered with control")
				}

			// In this step we expect the router to call the
			// ControlTower's Fail method, to indicate that the
			// payment failed.
			case routerFail:
				select {
				case _ = <-control.fail:
				case <-time.After(1 * time.Second):
					t.Fatalf("not registered with control")
				}

			// In this step we expect the SendToSwitch method to be
			// called, and we respond with a nil-error.
			case sendToSwitchSuccess:
				select {
				case sendResult <- nil:
				case <-time.After(1 * time.Second):
					t.Fatalf("unable to send result")
				}

			// In this step we expect the SendToSwitch method to be
			// called, and we respond with a forwarding error
			case sendToSwitchResultFailure:
				select {
				case sendResult <- &htlcswitch.ForwardingError{
					FailureSourceIdx: 1,
					FailureMessage:   &lnwire.FailTemporaryChannelFailure{},
				}:
				case <-time.After(1 * time.Second):
					t.Fatalf("unable to send result")
				}

			// In this step we expect the GetPaymentResult method
			// to be called, and we respond with the preimage to
			// complete the payment.
			case getPaymentResultSuccess:
				select {
				case getPaymentResult <- &htlcswitch.PaymentResult{
					Preimage: preImage,
				}:
				case <-time.After(1 * time.Second):
					t.Fatalf("unable to send result")
				}

			// In this state we expect the GetPaymentResult method
			// to be called, and we respond with a forwarding
			// error, indicating that the router should retry.
			case getPaymentResultFailure:
				select {
				case getPaymentResult <- &htlcswitch.PaymentResult{
					Error: &htlcswitch.ForwardingError{
						FailureSourceIdx: 1,
						FailureMessage:   &lnwire.FailTemporaryChannelFailure{},
					},
				}:
				case <-time.After(1 * time.Second):
					t.Fatalf("unable to get result")
				}

			// In this step we manually try to resend the same
			// payment, making sure the router responds with an
			// error indicating that it is alreayd in flight.
			case resendPayment:
				resendResult = make(chan error)
				go func() {
					_, _, err := router.SendPayment(&payment)
					resendResult <- err
				}()

			// In this step we manually stop the router.
			case stopRouter:
				select {
				case getPaymentResultErr <- fmt.Errorf(
					"shutting down"):
				case <-time.After(1 * time.Second):
					t.Fatalf("unable to send payment " +
						"result error")
				}

				if err := router.Stop(); err != nil {
					t.Fatalf("unable to restart: %v", err)
				}

			// In this step we manually start the router.
			case startRouter:
				router, sendResult, getPaymentResult,
					getPaymentResultErr = setupRouter()

			// In this state we expect to receive an error for the
			// original payment made.
			case paymentError:
				select {
				case err := <-paymentResult:
					if err == nil {
						t.Fatalf("expected error")
					}

				case <-time.After(1 * time.Second):
					t.Fatalf("got no payment result")
				}

			// In this state we expect the original payment to
			// succeed.
			case paymentSuccess:
				select {
				case err := <-paymentResult:
					if err != nil {
						t.Fatalf("did not expecte error %v", err)
					}

				case <-time.After(1 * time.Second):
					t.Fatalf("got no payment result")
				}

			// In this state we expect to receive an error for the
			// resent payment made.
			case resentPaymentError:
				select {
				case err := <-resendResult:
					if err == nil {
						t.Fatalf("expected error")
					}

				case <-time.After(1 * time.Second):
					t.Fatalf("got no payment result")
				}

			// In this state we expect the resent payment to
			// succeed.
			case resentPaymentSuccess:
				select {
				case err := <-resendResult:
					if err != nil {
						t.Fatalf("did not expect error %v", err)
					}

				case <-time.After(1 * time.Second):
					t.Fatalf("got no payment result")
				}

			default:
				t.Fatalf("unknown step %v", step)
			}
		}
	}
}

// TestSendToRouteStructuredError asserts that SendToRoute returns a structured
// error.
func TestSendToRouteStructuredError(t *testing.T) {
	t.Parallel()

	// Setup a three node network.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraph.cleanUp()

	const startingBlockHeight = 101

	ctx, cleanUp, err := createTestCtxFromGraphInstance(
		startingBlockHeight, testGraph,
	)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// Set up an init channel for the control tower, such that we can make
	// sure the payment is initiated correctly.
	init := make(chan initArgs, 1)
	ctx.router.cfg.Control.(*mockControlTower).init = init

	// Setup a route from source a to destination c. The route will be used
	// in a call to SendToRoute. SendToRoute also applies channel updates,
	// but it saves us from including RequestRoute in the test scope too.
	const payAmt = lnwire.MilliSatoshi(10000)
	hop1 := ctx.aliases["b"]
	hop2 := ctx.aliases["c"]
	hops := []*route.Hop{
		{
			ChannelID:    1,
			PubKeyBytes:  hop1,
			AmtToForward: payAmt,
		},
		{
			ChannelID:    2,
			PubKeyBytes:  hop2,
			AmtToForward: payAmt,
		},
	}

	rt, err := route.NewRouteFromHops(payAmt, 100, ctx.aliases["a"], hops)
	if err != nil {
		t.Fatalf("unable to create route: %v", err)
	}

	// We'll modify the SendToSwitch method so that it simulates a failed
	// payment with an error originating from the first hop of the route.
	// The unsigned channel update is attached to the failure message.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcher).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			return [32]byte{}, &htlcswitch.ForwardingError{
				FailureSourceIdx: 1,
				FailureMessage: &lnwire.FailFeeInsufficient{
					Update: lnwire.ChannelUpdate{},
				},
			}
		})

	// The payment parameter is mostly redundant in SendToRoute. Can be left
	// empty for this test.
	var payment lntypes.Hash

	// Send off the payment request to the router. The specified route
	// should be attempted and the channel update should be received by
	// router and ignored because it is missing a valid signature.
	_, err = ctx.router.SendToRoute(payment, rt)

	fErr, ok := err.(*htlcswitch.ForwardingError)
	if !ok {
		t.Fatalf("expected forwarding error")
	}

	if _, ok := fErr.FailureMessage.(*lnwire.FailFeeInsufficient); !ok {
		t.Fatalf("expected fee insufficient error")
	}

	// Check that the correct values were used when initiating the payment.
	select {
	case initVal := <-init:
		if initVal.c.Value != payAmt {
			t.Fatalf("expected %v, got %v", payAmt, initVal.c.Value)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("initPayment not called")
	}
}
