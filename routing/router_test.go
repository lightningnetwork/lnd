package routing

import (
	"bytes"
	"fmt"
	"image/color"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// defaultNumRoutes is the default value for the maximum number of routes to
// be returned by FindRoutes
const defaultNumRoutes = 10

type testCtx struct {
	router *ChannelRouter

	graph *channeldb.ChannelGraph

	aliases map[string]*btcec.PublicKey

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
		Graph:     c.graph,
		Chain:     c.chain,
		ChainView: c.chainView,
		SendToSwitch: func(_ lnwire.ShortChannelID,
			_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {
			return [32]byte{}, nil
		},
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
	router, err := New(Config{
		Graph:     graphInstance.graph,
		Chain:     chain,
		ChainView: chainView,
		SendToSwitch: func(_ lnwire.ShortChannelID,
			_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

			return [32]byte{}, nil
		},
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,
		QueryBandwidth: func(e *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
			return lnwire.NewMSatFromSatoshis(e.Capacity)
		},
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

// TestFindRoutesFeeSorting asserts that routes found by the FindRoutes method
// within the channel router are properly returned in a sorted order, with the
// lowest fee route coming first.
func TestFindRoutesFeeSorting(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtxFromFile(startingBlockHeight, basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

	// In this test we'd like to ensure proper integration of the various
	// functions that are involved in path finding, and also route
	// selection.

	// Execute a query for all possible routes between roasbeef and luo ji.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.aliases["luoji"]
	routes, err := ctx.router.FindRoutes(
		target, paymentAmt, noFeeLimit, defaultNumRoutes,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	// Exactly, two such paths should be found.
	if len(routes) != 2 {
		t.Fatalf("2 routes should've been selected, instead %v were: %v",
			len(routes), spew.Sdump(routes))
	}

	// We shouldn't pay a fee for the fist route, but the second route
	// should have a fee intact.
	if routes[0].TotalFees != 0 {
		t.Fatalf("incorrect fees for first route, expected 0 got: %v",
			routes[0].TotalFees)
	}
	if routes[1].TotalFees == 0 {
		t.Fatalf("total fees not set in second route: %v",
			spew.Sdump(routes[0]))
	}

	// The paths should properly be ranked according to their total fee
	// rate.
	if routes[0].TotalFees > routes[1].TotalFees {
		t.Fatalf("routes not ranked by total fee: %v",
			spew.Sdump(routes))
	}
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
	feeLimit := lnwire.NewMSatFromSatoshis(10)

	routes, err := ctx.router.FindRoutes(
		target, paymentAmt, feeLimit, defaultNumRoutes,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}

	if routes[0].TotalFees > feeLimit {
		t.Fatalf("route exceeded fee limit: %v", spew.Sdump(routes[0]))
	}

	hops := routes[0].Hops
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(hops))
	}

	if !bytes.Equal(hops[0].PubKeyBytes[:],
		ctx.aliases["songoku"].SerializeCompressed()) {

		t.Fatalf("expected first hop through songoku, got %s",
			getAliasFromPubKey(hops[0].PubKeyBytes[:],
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

	sourceNode := ctx.router.selfNode

	// We'll modify the SendToSwitch method that's been set within the
	// router's configuration to ignore the path that has luo ji as the
	// first hop. This should force the router to instead take the
	// available two hop path (through satoshi).
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		roasbeefLuoji := lnwire.NewShortChanIDFromInt(689530843)
		if firstHop == roasbeefLuoji {
			pub, err := sourceNode.PubKey()
			if err != nil {
				return preImage, err
			}
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource: pub,
				// TODO(roasbeef): temp node failure should be?
				FailureMessage: &lnwire.FailTemporaryChannelFailure{},
			}
		}

		return preImage, nil
	}

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
	if !bytes.Equal(route.Hops[0].PubKeyBytes[:],
		ctx.aliases["satoshi"].SerializeCompressed()) {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes[:],
				ctx.aliases))
	}
}

// TestChannelUpdateValidation tests that a failed payment with an associated
// channel update will only be applied to the graph when the update contains a
// valid signature.
func TestChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	// Setup a three node network.
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}, 1),
		symmetricTestChannel("b", "c", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(testChannels)
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
	var hop1 [33]byte
	copy(hop1[:], ctx.aliases["b"].SerializeCompressed())

	var hop2 [33]byte
	copy(hop2[:], ctx.aliases["c"].SerializeCompressed())

	hops := []*Hop{
		{
			ChannelID:   1,
			PubKeyBytes: hop1,
		},
		{
			ChannelID:   2,
			PubKeyBytes: hop2,
		},
	}

	route, err := NewRouteFromHops(
		lnwire.MilliSatoshi(10000), 100,
		NewVertex(ctx.aliases["a"]), hops,
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
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		return [32]byte{}, &htlcswitch.ForwardingError{
			ErrorSource: ctx.aliases["b"],
			FailureMessage: &lnwire.FailFeeInsufficient{
				Update: errChanUpdate,
			},
		}
	}

	// The payment parameter is mostly redundant in SendToRoute. Can be left
	// empty for this test.
	payment := &LightningPayment{}

	// Send off the payment request to the router. The specified route
	// should be attempted and the channel update should be received by
	// router and ignored because it is missing a valid signature.
	_, _, err = ctx.router.SendToRoute([]*Route{route}, payment)
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
	_, _, err = ctx.router.SendToRoute([]*Route{route}, payment)
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
	_, _, edgeUpateToFail, err := ctx.graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch chan id: %v", err)
	}

	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(chanID),
		Timestamp:       uint32(edgeUpateToFail.LastUpdate.Unix()),
		MessageFlags:    edgeUpateToFail.MessageFlags,
		ChannelFlags:    edgeUpateToFail.ChannelFlags,
		TimeLockDelta:   edgeUpateToFail.TimeLockDelta,
		HtlcMinimumMsat: edgeUpateToFail.MinHTLC,
		BaseFee:         uint32(edgeUpateToFail.FeeBaseMSat),
		FeeRate:         uint32(edgeUpateToFail.FeeProportionalMillionths),
	}

	// The error will be returned by Son Goku.
	sourceNode := ctx.aliases["songoku"]

	// We'll now modify the SendToSwitch method to return an error for the
	// outgoing channel to Son goku. This will be a fee related error, so
	// it should only cause the edge to be pruned after the second attempt.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		roasbeefSongoku := lnwire.NewShortChanIDFromInt(chanID)
		if firstHop == roasbeefSongoku {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource: sourceNode,

				// Within our error, we'll add a channel update
				// which is meant to reflect he new fee
				// schedule for the node/channel.
				FailureMessage: &lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				},
			}
		}

		return preImage, nil
	}

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
	if !bytes.Equal(route.Hops[0].PubKeyBytes[:],
		ctx.aliases["phamnuwen"].SerializeCompressed()) {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes[:],
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
	_, _, edgeUpateToFail, err := ctx.graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch chan id: %v", err)
	}

	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(chanID),
		Timestamp:       uint32(edgeUpateToFail.LastUpdate.Unix()),
		MessageFlags:    edgeUpateToFail.MessageFlags,
		ChannelFlags:    edgeUpateToFail.ChannelFlags,
		TimeLockDelta:   edgeUpateToFail.TimeLockDelta,
		HtlcMinimumMsat: edgeUpateToFail.MinHTLC,
		BaseFee:         uint32(edgeUpateToFail.FeeBaseMSat),
		FeeRate:         uint32(edgeUpateToFail.FeeProportionalMillionths),
	}

	// The error will be returned by Son Goku.
	sourceNode := ctx.aliases["songoku"]

	// We'll now modify the SendToSwitch method to return an error for the
	// outgoing channel to son goku. Since this is a time lock related
	// error, we should fail the payment flow all together, as Goku is the
	// only channel to Sophon.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if firstHop == roasbeefSongoku {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource: sourceNode,
				FailureMessage: &lnwire.FailExpiryTooSoon{
					Update: errChanUpdate,
				},
			}
		}

		return preImage, nil
	}

	// assertExpectedPath is a helper function that asserts the returned
	// route properly routes around the failure we've introduced in the
	// graph.
	assertExpectedPath := func(retPreImage [32]byte, route *Route) {
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
		if !bytes.Equal(route.Hops[0].PubKeyBytes[:],
			ctx.aliases["phamnuwen"].SerializeCompressed()) {

			t.Fatalf("route should go through phamnuwen as first hop, "+
				"instead passes through: %v",
				getAliasFromPubKey(route.Hops[0].PubKeyBytes[:],
					ctx.aliases))
		}
	}

	// Send off the payment request to the router, this payment should
	// succeed as we should actually go through Pham Nuwen in order to get
	// to Sophon, even though he has higher fees.
	paymentPreImage, route, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	assertExpectedPath(paymentPreImage, route)

	// We'll now modify the error return an IncorrectCltvExpiry error
	// instead, this should result in the same behavior of roasbeef routing
	// around the faulty Son Goku node.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if firstHop == roasbeefSongoku {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource: sourceNode,
				FailureMessage: &lnwire.FailIncorrectCltvExpiry{
					Update: errChanUpdate,
				},
			}
		}

		return preImage, nil
	}

	// Once again, Roasbeef should route around Goku since they disagree
	// w.r.t to the block height, and instead go through Pham Nuwen.
	paymentPreImage, route, err = ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	assertExpectedPath(paymentPreImage, route)
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

	sourceNode, err := ctx.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	sourcePub, err := sourceNode.PubKey()
	if err != nil {
		t.Fatalf("unable to fetch source node pub: %v", err)
	}

	roasbeefLuoji := lnwire.NewShortChanIDFromInt(689530843)

	// First, we'll modify the SendToSwitch method to return an error
	// indicating that the channel from roasbeef to luoji is not operable
	// with an UnknownNextPeer.
	//
	// TODO(roasbeef): filtering should be intelligent enough so just not
	// go through satoshi at all at this point.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if firstHop == roasbeefLuoji {
			// We'll first simulate an error from the first
			// outgoing link to simulate the channel from luo ji to
			// roasbeef not having enough capacity.
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourcePub,
				FailureMessage: &lnwire.FailTemporaryChannelFailure{},
			}
		}

		// Next, we'll create an error from satoshi to indicate
		// that the luoji node is not longer online, which should
		// prune out the rest of the routes.
		roasbeefSatoshi := lnwire.NewShortChanIDFromInt(2340213491)
		if firstHop == roasbeefSatoshi {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    ctx.aliases["satoshi"],
				FailureMessage: &lnwire.FailUnknownNextPeer{},
			}
		}

		return preImage, nil
	}

	ctx.router.missionControl.ResetHistory()

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

	ctx.router.missionControl.ResetHistory()

	// Next, we'll modify the SendToSwitch method to indicate that luo ji
	// wasn't originally online. This should also halt the send all
	// together as all paths contain luoji and he can't be reached.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if firstHop == roasbeefLuoji {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourcePub,
				FailureMessage: &lnwire.FailUnknownNextPeer{},
			}
		}

		return preImage, nil
	}

	// This shouldn't return an error, as we'll make a payment attempt via
	// the satoshi channel based on the assumption that there might be an
	// intermittent issue with the roasbeef <-> lioji channel.
	paymentPreImage, route, err := ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable send payment: %v", err)
	}

	// This path should go: roasbeef -> satoshi -> luoji
	if len(route.Hops) != 2 {
		t.Fatalf("incorrect route length: expected %v got %v", 2,
			len(route.Hops))
	}
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}
	if !bytes.Equal(route.Hops[0].PubKeyBytes[:],
		ctx.aliases["satoshi"].SerializeCompressed()) {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes[:],
				ctx.aliases))
	}

	ctx.router.missionControl.ResetHistory()

	// Finally, we'll modify the SendToSwitch function to indicate that the
	// roasbeef -> luoji channel has insufficient capacity. This should
	// again cause us to instead go via the satoshi route.
	ctx.router.cfg.SendToSwitch = func(firstHop lnwire.ShortChannelID,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if firstHop == roasbeefLuoji {
			// We'll first simulate an error from the first
			// outgoing link to simulate the channel from luo ji to
			// roasbeef not having enough capacity.
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourcePub,
				FailureMessage: &lnwire.FailTemporaryChannelFailure{},
			}
		}
		return preImage, nil
	}

	paymentPreImage, route, err = ctx.router.SendPayment(&payment)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// This should succeed finally.  The route selected should have two
	// hops.
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
	if !bytes.Equal(route.Hops[0].PubKeyBytes[:],
		ctx.aliases["satoshi"].SerializeCompressed()) {

		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			getAliasFromPubKey(route.Hops[0].PubKeyBytes[:],
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
	testGraph, err := createTestGraphFromChannels(testChannels)
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
	if connectNode == nil {
		t.Fatalf("could not find node to connect to")
	}

	var (
		pubKey1 *btcec.PublicKey
		pubKey2 *btcec.PublicKey
	)
	node1Bytes := priv1.PubKey().SerializeCompressed()
	node2Bytes := connectNode.SerializeCompressed()
	if bytes.Compare(node1Bytes, node2Bytes) == -1 {
		pubKey1 = priv1.PubKey()
		pubKey2 = connectNode
	} else {
		pubKey1 = connectNode
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
	copy(edge.NodeKey2Bytes[:], node2Bytes)
	copy(edge.BitcoinKey1Bytes[:], node1Bytes)
	copy(edge.BitcoinKey2Bytes[:], node2Bytes)

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

	// We should now be able to find two routes to node 2.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	targetNode := priv2.PubKey()
	routes, err := ctx.router.FindRoutes(
		targetNode, paymentAmt, noFeeLimit, defaultNumRoutes,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected to find 2 route, found: %v", len(routes))
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

	// Should still be able to find the routes, and the info should be
	// updated.
	routes, err = ctx.router.FindRoutes(
		targetNode, paymentAmt, noFeeLimit, defaultNumRoutes,
		zpay32.DefaultFinalCLTVDelta,
	)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected to find 2 route, found: %v", len(routes))
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
	_, _, has, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}

	_, _, has, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
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
		Graph:     ctx.graph,
		Chain:     ctx.chain,
		ChainView: ctx.chainView,
		SendToSwitch: func(_ lnwire.ShortChannelID,
			_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {
			return [32]byte{}, nil
		},
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
	_, _, has, err = ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("did not find edge in graph")
	}

	_, _, has, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
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
	_, _, has, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}

	_, _, has, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
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
	_, _, has, err = ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("did not find edge in graph")
	}

	_, _, has, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
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
	_, _, hasChan, err := ctx.graph.HasChannelEdge(chanID1.ToUint64())
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !hasChan {
		t.Fatalf("could not find edge in graph")
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
	_, _, hasChan, err = ctx.graph.HasChannelEdge(chanID1.ToUint64())
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if hasChan {
		t.Fatalf("channel was found in graph but shouldn't have been")
	}
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

	ignoreVertex := make(map[Vertex]struct{})
	ignoreEdge := make(map[edgeLocator]struct{})

	amt := lnwire.MilliSatoshi(100)

	target := ctx.aliases["luoji"]
	if target == nil {
		t.Fatalf("unable to find target node")
	}

	// We'll now attempt a path finding attempt using this set up. Due to
	// the edge weighting, we should select the direct path over the 2 hop
	// path even though the direct path has a higher potential time lock.
	path, err := findPath(
		&graphParams{
			graph: ctx.graph,
		},
		&restrictParams{
			ignoredNodes: ignoreVertex,
			ignoredEdges: ignoreEdge,
			feeLimit:     noFeeLimit,
		},
		sourceNode, target, amt,
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

	emptyRoute := &Route{}
	_, _, err := generateSphinxPacket(emptyRoute, testHash[:])
	if err != ErrNoRouteHopsProvided {
		t.Fatalf("expected empty hops error: instead got: %v", err)
	}
}
