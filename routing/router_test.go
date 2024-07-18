package routing

import (
	"bytes"
	"fmt"
	"image/color"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/graph"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	uniquePaymentID uint64 = 1 // to be used atomically

	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	testAddrs = []net.Addr{testAddr}

	testFeatures = lnwire.NewFeatureVector(nil, lnwire.Features)

	testHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	priv1, _    = btcec.NewPrivateKey()
	bitcoinKey1 = priv1.PubKey()

	priv2, _    = btcec.NewPrivateKey()
	bitcoinKey2 = priv2.PubKey()
)

type testCtx struct {
	router *ChannelRouter

	graphBuilder *mockGraphBuilder

	graph *channeldb.ChannelGraph

	aliases map[string]route.Vertex

	privKeys map[string]*btcec.PrivateKey

	channelIDs map[route.Vertex]map[route.Vertex]uint64
}

func (c *testCtx) getChannelIDFromAlias(t *testing.T, a, b string) uint64 {
	vertexA, ok := c.aliases[a]
	require.True(t, ok, "cannot find aliases for %s", a)

	vertexB, ok := c.aliases[b]
	require.True(t, ok, "cannot find aliases for %s", b)

	channelIDMap, ok := c.channelIDs[vertexA]
	require.True(t, ok, "cannot find channelID map %s(%s)", vertexA, a)

	channelID, ok := channelIDMap[vertexB]
	require.True(t, ok, "cannot find channelID using %s(%s)", vertexB, b)

	return channelID
}

func createTestCtxFromGraphInstance(t *testing.T, startingHeight uint32,
	graphInstance *testGraphInstance) *testCtx {

	return createTestCtxFromGraphInstanceAssumeValid(
		t, startingHeight, graphInstance,
	)
}

func createTestCtxFromGraphInstanceAssumeValid(t *testing.T,
	startingHeight uint32, graphInstance *testGraphInstance) *testCtx {

	// We'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send messages won't
	// be populated.
	chain := newMockChain(startingHeight)

	pathFindingConfig := PathFindingConfig{
		MinProbability: 0.01,
		AttemptCost:    100,
	}

	aCfg := AprioriConfig{
		PenaltyHalfLife:       time.Hour,
		AprioriHopProbability: 0.9,
		AprioriWeight:         0.5,
		CapacityFraction:      testCapacityFraction,
	}
	estimator, err := NewAprioriEstimator(aCfg)
	require.NoError(t, err)

	mcConfig := &MissionControlConfig{Estimator: estimator}

	mc, err := NewMissionControl(
		graphInstance.graphBackend, route.Vertex{}, mcConfig,
	)
	require.NoError(t, err, "failed to create missioncontrol")

	sourceNode, err := graphInstance.graph.SourceNode()
	require.NoError(t, err)
	sessionSource := &SessionSource{
		GraphSessionFactory: newMockGraphSessionFactoryFromChanDB(
			graphInstance.graph,
		),
		SourceNode:        sourceNode,
		GetLink:           graphInstance.getLink,
		PathFindingConfig: pathFindingConfig,
		MissionControl:    mc,
	}

	graphBuilder := newMockGraphBuilder(graphInstance.graph)

	router, err := New(Config{
		SelfNode:       sourceNode.PubKeyBytes,
		RoutingGraph:   newMockGraphSessionChanDB(graphInstance.graph),
		Chain:          chain,
		Payer:          &mockPaymentAttemptDispatcherOld{},
		Control:        makeMockControlTower(),
		MissionControl: mc,
		SessionSource:  sessionSource,
		GetLink:        graphInstance.getLink,
		NextPaymentID: func() (uint64, error) {
			next := atomic.AddUint64(&uniquePaymentID, 1)
			return next, nil
		},
		PathFindingConfig:  pathFindingConfig,
		Clock:              clock.NewTestClock(time.Unix(1, 0)),
		ApplyChannelUpdate: graphBuilder.ApplyChannelUpdate,
	})
	require.NoError(t, router.Start(), "unable to start router")

	ctx := &testCtx{
		router:       router,
		graphBuilder: graphBuilder,
		graph:        graphInstance.graph,
		aliases:      graphInstance.aliasMap,
		privKeys:     graphInstance.privKeyMap,
		channelIDs:   graphInstance.channelIDs,
	}

	t.Cleanup(func() {
		ctx.router.Stop()
	})

	return ctx
}

func createTestNode() (*channeldb.LightningNode, error) {
	updateTime := rand.Int63()

	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, errors.Errorf("unable create private key: %v", err)
	}

	pub := priv.PubKey().SerializeCompressed()
	n := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(updateTime, 0),
		Addresses:            testAddrs,
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "kek" + string(pub),
		AuthSigBytes:         testSig.Serialize(),
		Features:             testFeatures,
	}
	copy(n.PubKeyBytes[:], pub)

	return n, nil
}

func createTestCtxFromFile(t *testing.T,
	startingHeight uint32, testGraph string) *testCtx {

	// We'll attempt to locate and parse out the file
	// that encodes the graph that our tests should be run against.
	graphInstance, err := parseTestGraph(t, true, testGraph)
	require.NoError(t, err, "unable to create test graph")

	return createTestCtxFromGraphInstance(t, startingHeight, graphInstance)
}

// Add valid signature to channel update simulated as error received from the
// network.
func signErrChanUpdate(t *testing.T, key *btcec.PrivateKey,
	errChanUpdate *lnwire.ChannelUpdate) {

	chanUpdateMsg, err := errChanUpdate.DataToSign()
	require.NoError(t, err, "failed to retrieve data to sign")

	digest := chainhash.DoubleHashB(chanUpdateMsg)
	sig := ecdsa.Sign(key, digest)

	errChanUpdate.Signature, err = lnwire.NewSigFromSignature(sig)
	require.NoError(t, err, "failed to create new signature")
}

// TestFindRoutesWithFeeLimit asserts that routes found by the FindRoutes method
// within the channel router contain a total fee less than or equal to the fee
// limit.
func TestFindRoutesWithFeeLimit(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

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
		CltvLimit:         math.MaxUint32,
	}

	req, err := NewRouteRequest(
		ctx.router.cfg.SelfNode, &target, paymentAmt, 0,
		restrictions, nil, nil, nil, MinCLTVDelta,
	)
	require.NoError(t, err, "invalid route request")

	route, _, err := ctx.router.FindRoute(req)
	require.NoError(t, err, "unable to find any routes")

	require.Falsef(t,
		route.TotalFees() > restrictions.FeeLimit,
		"route exceeded fee limit: %v", spew.Sdump(route),
	)

	hops := route.Hops
	require.Equal(t, 2, len(hops), "expected 2 hops")

	require.Equalf(t,
		ctx.aliases["songoku"], hops[0].PubKeyBytes,
		"expected first hop through songoku, got %s",
		getAliasFromPubKey(hops[0].PubKeyBytes, ctx.aliases),
	)
}

// TestSendPaymentRouteFailureFallback tests that when sending a payment, if
// one of the target routes is seen as unavailable, then the next route in the
// queue is used instead. This process should continue until either a payment
// succeeds, or all routes have been exhausted.
func TestSendPaymentRouteFailureFallback(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 1000 satoshis, with a maximum of 1000 satoshis in fees.
	paymentAmt := lnwire.NewMSatFromSatoshis(1000)
	payment := createDummyLightningPayment(
		t, ctx.aliases["sophon"], paymentAmt,
	)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// Get the channel ID.
	roasbeefSongoku := lnwire.NewShortChanIDFromInt(
		ctx.getChannelIDFromAlias(t, "roasbeef", "songoku"),
	)

	// We'll modify the SendToSwitch method that's been set within the
	// router's configuration to ignore the path that has son goku as the
	// first hop. This should force the router to instead take the
	// the more costly path (through pham nuwen).
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			if firstHop == roasbeefSongoku {
				return [32]byte{}, htlcswitch.NewForwardingError(
					// TODO(roasbeef): temp node failure
					//  should be?
					&lnwire.FailTemporaryChannelFailure{},
					1,
				)
			}

			return preImage, nil
		})

	// Send off the payment request to the router, route through pham nuwen
	// should've been selected as a fall back and succeeded correctly.
	paymentPreImage, route, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	// The route selected should have two hops
	require.Equal(t, 2, len(route.Hops), "incorrect route length")

	// The preimage should match up with the once created above.
	if !bytes.Equal(paymentPreImage[:], preImage[:]) {
		t.Fatalf("incorrect preimage used: expected %x got %x",
			preImage[:], paymentPreImage[:])
	}

	// The route should have pham nuwen as the first hop.
	require.Equalf(t,
		ctx.aliases["phamnuwen"], route.Hops[0].PubKeyBytes,
		"route should go through phamnuwen as first hop, instead "+
			"passes through: %v",
		getAliasFromPubKey(route.Hops[0].PubKeyBytes, ctx.aliases),
	)
}

// TestSendPaymentRouteInfiniteLoopWithBadHopHint tests that when sending
// a payment with a malformed hop hint in the first hop, the hint is ignored
// and the payment succeeds without an infinite loop of retries.
func TestSendPaymentRouteInfiniteLoopWithBadHopHint(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	source := ctx.aliases["roasbeef"]
	sourceNodeID, err := btcec.ParsePubKey(source[:])
	require.NoError(t, err)

	actualChannelID := ctx.getChannelIDFromAlias(t, "roasbeef", "songoku")
	badChannelID := uint64(66666)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to songoku for 1000 satoshis.
	paymentAmt := lnwire.NewMSatFromSatoshis(1000)
	payment := createDummyLightningPayment(
		t, ctx.aliases["songoku"], paymentAmt,
	)
	payment.RouteHints = [][]zpay32.HopHint{{
		zpay32.HopHint{
			NodeID:          sourceNodeID,
			ChannelID:       badChannelID,
			FeeBaseMSat:     uint32(50),
			CLTVExpiryDelta: uint16(200),
		},
	}}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// Mock a payment result that always fails with FailUnknownNextPeer when
	// the bad channel is the first hop.
	badShortChanID := lnwire.NewShortChanIDFromInt(badChannelID)
	newFwdError := htlcswitch.NewForwardingError(
		&lnwire.FailUnknownNextPeer{}, 0,
	)

	payer, ok := ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld)
	require.Equal(t, ok, true, "failed Payer cast")

	payer.setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			// Returns a FailUnknownNextPeer if it's trying
			// to pay an invalid channel.
			if firstHop == badShortChanID {
				return [32]byte{}, newFwdError
			}

			return preImage, nil
		})

	// Send off the payment request to the router, should succeed
	// ignoring the bad channel id hint.
	paymentPreImage, route, paymentErr := ctx.router.SendPayment(payment)
	require.NoErrorf(t, paymentErr, "unable to send payment: %v",
		payment.paymentHash)

	// The preimage should match up with the one created above.
	require.Equal(t, preImage[:], paymentPreImage[:], "incorrect preimage")

	// The route should have songoku as the first hop.
	require.Equal(t, actualChannelID, route.Hops[0].ChannelID,
		"route should go through the correct channel id",
	)
}

// TestChannelUpdateValidation tests that a failed payment with an associated
// channel update will only be applied to the graph when the update contains a
// valid signature.
func TestChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	// Setup a three node network.
	chanCapSat := btcutil.Amount(100000)
	feeRate := lnwire.MilliSatoshi(400)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: feeRate,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: feeRate,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(
		t, true, testChannels, "a",
	)
	require.NoError(t, err, "unable to create graph")

	const startingBlockHeight = 101
	ctx := createTestCtxFromGraphInstance(t, startingBlockHeight, testGraph)

	// Assert that the initially configured fee is retrieved correctly.
	_, e1, e2, err := ctx.graph.FetchChannelEdgesByID(
		lnwire.NewShortChanIDFromInt(1).ToUint64(),
	)
	require.NoError(t, err, "cannot retrieve channel")

	require.Equal(t, feeRate, e1.FeeProportionalMillionths, "invalid fee")
	require.Equal(t, feeRate, e2.FeeProportionalMillionths, "invalid fee")

	// Setup a route from source a to destination c. The route will be used
	// in a call to SendToRoute. SendToRoute also applies channel updates,
	// but it saves us from including RequestRoute in the test scope too.
	hop1 := ctx.aliases["b"]
	hop2 := ctx.aliases["c"]
	hops := []*route.Hop{
		{
			ChannelID:     1,
			PubKeyBytes:   hop1,
			LegacyPayload: true,
		},
		{
			ChannelID:     2,
			PubKeyBytes:   hop2,
			LegacyPayload: true,
		},
	}

	rt, err := route.NewRouteFromHops(
		lnwire.MilliSatoshi(10000), 100,
		ctx.aliases["a"], hops,
	)
	require.NoError(t, err, "unable to create route")

	// Set up a channel update message with an invalid signature to be
	// returned to the sender.
	var invalidSignature lnwire.Sig
	errChanUpdate := lnwire.ChannelUpdate{
		Signature:       invalidSignature,
		FeeRate:         500,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(1),
		Timestamp:       uint32(testTime.Add(time.Minute).Unix()),
		MessageFlags:    e2.MessageFlags,
		ChannelFlags:    e2.ChannelFlags,
		HtlcMaximumMsat: e2.MaxHTLC,
	}

	// We'll modify the SendToSwitch method so that it simulates a failed
	// payment with an error originating from the first hop of the route.
	// The unsigned channel update is attached to the failure message.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			return [32]byte{}, htlcswitch.NewForwardingError(
				&lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				},
				1,
			)
		})

	// The payment parameter is mostly redundant in SendToRoute. Can be left
	// empty for this test.
	var payment lntypes.Hash

	// Instruct the mock graph builder to reject the next update we send
	// it.
	ctx.graphBuilder.setNextReject(true)

	// Send off the payment request to the router. The specified route
	// should be attempted and the channel update should be received by
	// graph and ignored because it is missing a valid signature.
	_, err = ctx.router.SendToRoute(payment, rt)
	require.Error(t, err, "expected route to fail with channel update")

	_, e1, e2, err = ctx.graph.FetchChannelEdgesByID(
		lnwire.NewShortChanIDFromInt(1).ToUint64(),
	)
	require.NoError(t, err, "cannot retrieve channel")

	require.Equal(t, feeRate, e1.FeeProportionalMillionths,
		"fee updated without valid signature")
	require.Equal(t, feeRate, e2.FeeProportionalMillionths,
		"fee updated without valid signature")

	// Next, add a signature to the channel update.
	signErrChanUpdate(t, testGraph.privKeyMap["b"], &errChanUpdate)

	// Let the graph builder accept the next update.
	ctx.graphBuilder.setNextReject(false)

	// Retry the payment using the same route as before.
	_, err = ctx.router.SendToRoute(payment, rt)
	require.Error(t, err, "expected route to fail with channel update")

	// This time a valid signature was supplied and the policy change should
	// have been applied to the graph.
	_, e1, e2, err = ctx.graph.FetchChannelEdgesByID(
		lnwire.NewShortChanIDFromInt(1).ToUint64(),
	)
	require.NoError(t, err, "cannot retrieve channel")

	require.Equal(t, feeRate, e1.FeeProportionalMillionths,
		"fee should not be updated")
	require.EqualValues(t, 500, int(e2.FeeProportionalMillionths),
		"fee not updated even though signature is valid")
}

// TestSendPaymentErrorRepeatedFeeInsufficient tests that if we receive
// multiple fee related errors from a channel that we're attempting to route
// through, then we'll prune the channel after the second attempt.
func TestSendPaymentErrorRepeatedFeeInsufficient(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// Get the channel ID.
	roasbeefSongokuChanID := ctx.getChannelIDFromAlias(
		t, "roasbeef", "songoku",
	)
	songokuSophonChanID := ctx.getChannelIDFromAlias(
		t, "songoku", "sophon",
	)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to sophon for 1000 satoshis.
	amt := lnwire.NewMSatFromSatoshis(1000)
	payment := createDummyLightningPayment(
		t, ctx.aliases["sophon"], amt,
	)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll also fetch the first outgoing channel edge from son goku
	// to sophon. We'll obtain this as we'll need to to generate the
	// FeeInsufficient error that we'll send back.
	_, _, edgeUpdateToFail, err := ctx.graph.FetchChannelEdgesByID(
		songokuSophonChanID,
	)
	require.NoError(t, err, "unable to fetch chan id")

	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID: lnwire.NewShortChanIDFromInt(
			songokuSophonChanID,
		),
		Timestamp:       uint32(edgeUpdateToFail.LastUpdate.Unix()),
		MessageFlags:    edgeUpdateToFail.MessageFlags,
		ChannelFlags:    edgeUpdateToFail.ChannelFlags,
		TimeLockDelta:   edgeUpdateToFail.TimeLockDelta,
		HtlcMinimumMsat: edgeUpdateToFail.MinHTLC,
		HtlcMaximumMsat: edgeUpdateToFail.MaxHTLC,
		BaseFee:         uint32(edgeUpdateToFail.FeeBaseMSat),
		FeeRate:         uint32(edgeUpdateToFail.FeeProportionalMillionths),
	}

	signErrChanUpdate(t, ctx.privKeys["songoku"], &errChanUpdate)

	// We'll now modify the SendToSwitch method to return an error for the
	// outgoing channel to Son goku. This will be a fee related error, so
	// it should only cause the edge to be pruned after the second attempt.
	dispatcher, ok := ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld) //nolint:lll
	require.True(t, ok)

	dispatcher.setPaymentResult(func(firstHop lnwire.ShortChannelID) (
		[32]byte, error) {

		roasbeefSongoku := lnwire.NewShortChanIDFromInt(
			roasbeefSongokuChanID,
		)
		if firstHop == roasbeefSongoku {
			return [32]byte{}, htlcswitch.NewForwardingError(
				// Within our error, we'll add a
				// channel update which is meant to
				// reflect the new fee schedule for the
				// node/channel.
				&lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				}, 1,
			)
		}

		return preImage, nil
	})

	// Send off the payment request to the router, route through phamnuwen
	// should've been selected as a fall back and succeeded correctly.
	paymentPreImage, route, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	// The route selected should have two hops
	require.Equal(t, 2, len(route.Hops), "incorrect route length")

	// The preimage should match up with the once created above.
	require.Equal(t, preImage[:], paymentPreImage[:], "incorrect preimage")

	// The route should have pham nuwen as the first hop.
	require.Equalf(t,
		ctx.aliases["phamnuwen"], route.Hops[0].PubKeyBytes,
		"route should go through pham nuwen as first hop, "+
			"instead passes through: %v",
		getAliasFromPubKey(route.Hops[0].PubKeyBytes, ctx.aliases),
	)
}

// TestSendPaymentErrorFeeInsufficientPrivateEdge tests that if we receive
// a fee related error from a private channel that we're attempting to route
// through, then we'll update the fees in the route hints and successfully
// route through the private channel in the second attempt.
//
// The test will send a payment from roasbeef to elst, available paths are,
// path1: roasbeef -> songoku -> sophon -> elst, total fee: 210k
// path2: roasbeef -> phamnuwen -> sophon -> elst, total fee: 220k
// path3: roasbeef -> songoku ->(private channel) elst
// We will setup the path3 to have the lowest fee so it's always the preferred
// path.
func TestSendPaymentErrorFeeInsufficientPrivateEdge(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// Get the channel ID.
	roasbeefSongoku := lnwire.NewShortChanIDFromInt(
		ctx.getChannelIDFromAlias(t, "roasbeef", "songoku"),
	)

	var (
		preImage         [32]byte
		amt              = lnwire.NewMSatFromSatoshis(1000)
		privateChannelID = uint64(55555)
		feeBaseMSat      = uint32(15)
		expiryDelta      = uint16(32)
		sgNode           = ctx.aliases["songoku"]
	)

	sgNodeID, err := btcec.ParsePubKey(sgNode[:])
	require.NoError(t, err)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to elst, through a private channel between songoku and elst for
	// 1000 satoshis. This route has lowest fees compared with the rest.
	// This also holds when the private channel fee is updated to a higher
	// value.
	payment := createDummyLightningPayment(
		t, ctx.aliases["elst"], amt,
	)
	payment.RouteHints = [][]zpay32.HopHint{{
		// Add a private channel between songoku and elst.
		zpay32.HopHint{
			NodeID:          sgNodeID,
			ChannelID:       privateChannelID,
			FeeBaseMSat:     feeBaseMSat,
			CLTVExpiryDelta: expiryDelta,
		},
	}}

	// Prepare an error update for the private channel, with twice the
	// original fee.
	updatedFeeBaseMSat := feeBaseMSat * 2
	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID: lnwire.NewShortChanIDFromInt(privateChannelID),
		Timestamp:      uint32(testTime.Add(time.Minute).Unix()),
		BaseFee:        updatedFeeBaseMSat,
		TimeLockDelta:  expiryDelta,
	}
	signErrChanUpdate(t, ctx.privKeys["songoku"], &errChanUpdate)

	// We'll now modify the SendHTLC method to return an error for the
	// outgoing channel to songoku.
	errorReturned := false
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			if firstHop != roasbeefSongoku || errorReturned {
				return preImage, nil
			}

			errorReturned = true
			return [32]byte{}, htlcswitch.NewForwardingError(
				// Within our error, we'll add a
				// channel update which is meant to
				// reflect the new fee schedule for the
				// node/channel.
				&lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				}, 1,
			)
		},
	)

	// Send off the payment request to the router, route through son
	// goku and then across the private channel to elst.
	paymentPreImage, route, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	require.True(t, errorReturned,
		"failed to simulate error in the first payment attempt",
	)

	// The route selected should have two hops. Make sure that,
	//   path: roasbeef -> son goku -> sophon -> elst
	//   path: roasbeef -> pham nuwen -> sophon -> elst
	// are not selected instead.
	require.Equal(t, 2, len(route.Hops), "incorrect route length")

	// The preimage should match up with the one created above.
	require.Equal(t,
		paymentPreImage[:], preImage[:], "incorrect preimage used",
	)

	// The route should have son goku as the first hop.
	require.Equal(t, route.Hops[0].PubKeyBytes, ctx.aliases["songoku"],
		"route should go through son goku as first hop",
	)

	// The route should pass via the private channel.
	require.Equal(t,
		privateChannelID, route.FinalHop().ChannelID,
		"route did not pass through private channel "+
			"between pham nuwen and elst",
	)

	// The route should have the updated fee.
	require.Equal(t,
		lnwire.MilliSatoshi(updatedFeeBaseMSat).String(),
		route.HopFee(0).String(),
		"fee to forward to the private channel not matched",
	)
}

// TestSendPaymentPrivateEdgeUpdateFeeExceedsLimit tests that upon receiving a
// ChannelUpdate in a fee related error from the private channel, we won't
// choose the route in our second attempt if the updated fee exceeds our fee
// limit specified in the payment.
//
// The test will send a payment from roasbeef to elst, available paths are,
// path1: roasbeef -> songoku -> sophon -> elst, total fee: 210k
// path2: roasbeef -> phamnuwen -> sophon -> elst, total fee: 220k
// path3: roasbeef -> songoku ->(private channel) elst
// We will setup the path3 to have the lowest fee and then update it with a fee
// exceeds our fee limit, thus this route won't be chosen.
func TestSendPaymentPrivateEdgeUpdateFeeExceedsLimit(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// Get the channel ID.
	roasbeefSongoku := lnwire.NewShortChanIDFromInt(
		ctx.getChannelIDFromAlias(t, "roasbeef", "songoku"),
	)

	var (
		preImage         [32]byte
		amt              = lnwire.NewMSatFromSatoshis(1000)
		privateChannelID = uint64(55555)
		feeBaseMSat      = uint32(15)
		expiryDelta      = uint16(32)
		sgNode           = ctx.aliases["songoku"]
		feeLimit         = lnwire.MilliSatoshi(500000)
	)

	sgNodeID, err := btcec.ParsePubKey(sgNode[:])
	require.NoError(t, err)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to elst, through a private channel between songoku and elst for
	// 1000 satoshis. This route has lowest fees compared with the rest.
	payment := createDummyLightningPayment(
		t, ctx.aliases["elst"], amt,
	)
	payment.RouteHints = [][]zpay32.HopHint{{
		// Add a private channel between songoku and elst.
		zpay32.HopHint{
			NodeID:          sgNodeID,
			ChannelID:       privateChannelID,
			FeeBaseMSat:     feeBaseMSat,
			CLTVExpiryDelta: expiryDelta,
		},
	}}

	// Prepare an error update for the private channel. The updated fee
	// will exceeds the feeLimit.
	updatedFeeBaseMSat := feeBaseMSat + uint32(feeLimit)
	errChanUpdate := lnwire.ChannelUpdate{
		ShortChannelID: lnwire.NewShortChanIDFromInt(privateChannelID),
		Timestamp:      uint32(testTime.Add(time.Minute).Unix()),
		BaseFee:        updatedFeeBaseMSat,
		TimeLockDelta:  expiryDelta,
	}
	signErrChanUpdate(t, ctx.privKeys["songoku"], &errChanUpdate)

	// We'll now modify the SendHTLC method to return an error for the
	// outgoing channel to songoku.
	errorReturned := false
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			if firstHop != roasbeefSongoku || errorReturned {
				return preImage, nil
			}

			errorReturned = true
			return [32]byte{}, htlcswitch.NewForwardingError(
				// Within our error, we'll add a
				// channel update which is meant to
				// reflect the new fee schedule for the
				// node/channel.
				&lnwire.FailFeeInsufficient{
					Update: errChanUpdate,
				}, 1,
			)
		},
	)

	// Send off the payment request to the router, route through son
	// goku and then across the private channel to elst.
	paymentPreImage, route, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	require.True(t, errorReturned,
		"failed to simulate error in the first payment attempt",
	)

	// The route selected should have three hops. Make sure that,
	//   path1: roasbeef -> son goku -> sophon -> elst
	//   path2: roasbeef -> pham nuwen -> sophon -> elst
	//   path3: roasbeef -> sophon -> (private channel) else
	// path1 is selected.
	require.Equal(t, 3, len(route.Hops), "incorrect route length")

	// The preimage should match up with the one created above.
	require.Equal(t,
		paymentPreImage[:], preImage[:], "incorrect preimage used",
	)

	// The route should have son goku as the first hop.
	require.Equal(t, route.Hops[0].PubKeyBytes, ctx.aliases["songoku"],
		"route should go through son goku as the first hop",
	)

	// The route should have sophon as the first hop.
	require.Equal(t, route.Hops[1].PubKeyBytes, ctx.aliases["sophon"],
		"route should go through sophon as the second hop",
	)
	// The route should pass via the public channel.
	require.Equal(t, route.FinalHop().PubKeyBytes, ctx.aliases["elst"],
		"route should go through elst as the final hop",
	)
}

// TestSendPaymentErrorNonFinalTimeLockErrors tests that if we receive either
// an ExpiryTooSoon or a IncorrectCltvExpiry error from a node, then we prune
// that node from the available graph within a mission control session. This
// test ensures that we'll route around errors due to nodes not knowing the
// current block height.
func TestSendPaymentErrorNonFinalTimeLockErrors(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(
		t, startingBlockHeight, basicGraphFilePath,
	)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to sophon for 1k satoshis.
	amt := lnwire.NewMSatFromSatoshis(1000)
	payment := createDummyLightningPayment(
		t, ctx.aliases["sophon"], amt,
	)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll also fetch the first outgoing channel edge from roasbeef to
	// son goku. This edge will be included in the time lock related expiry
	// errors that we'll get back due to disagrements in what the current
	// block height is.
	chanID := ctx.getChannelIDFromAlias(t, "roasbeef", "songoku")
	roasbeefSongoku := lnwire.NewShortChanIDFromInt(chanID)

	_, _, edgeUpdateToFail, err := ctx.graph.FetchChannelEdgesByID(chanID)
	require.NoError(t, err, "unable to fetch chan id")

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
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			if firstHop == roasbeefSongoku {
				return [32]byte{}, htlcswitch.NewForwardingError(
					&lnwire.FailExpiryTooSoon{
						Update: errChanUpdate,
					}, 1,
				)
			}

			return preImage, nil
		})

	// assertExpectedPath is a helper function that asserts the returned
	// route properly routes around the failure we've introduced in the
	// graph.
	assertExpectedPath := func(retPreImage [32]byte, route *route.Route) {
		// The route selected should have two hops
		require.Equal(t, 2, len(route.Hops), "incorrect route length")

		// The preimage should match up with the once created above.
		require.Equal(t,
			preImage[:], retPreImage[:], "incorrect preimage used",
		)

		// The route should have satoshi as the first hop.
		require.Equalf(t,
			ctx.aliases["phamnuwen"], route.Hops[0].PubKeyBytes,
			"route should go through phamnuwen as first hop, "+
				"instead passes through: %v",
			getAliasFromPubKey(
				route.Hops[0].PubKeyBytes, ctx.aliases,
			),
		)
	}

	// Send off the payment request to the router, this payment should
	// succeed as we should actually go through Pham Nuwen in order to get
	// to Sophon, even though he has higher fees.
	paymentPreImage, rt, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	assertExpectedPath(paymentPreImage, rt)

	// We'll now modify the error return an IncorrectCltvExpiry error
	// instead, this should result in the same behavior of roasbeef routing
	// around the faulty Son Goku node.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
			if firstHop == roasbeefSongoku {
				return [32]byte{}, htlcswitch.NewForwardingError(
					&lnwire.FailIncorrectCltvExpiry{
						Update: errChanUpdate,
					}, 1,
				)
			}

			return preImage, nil
		})

	// Once again, Roasbeef should route around Goku since they disagree
	// w.r.t to the block height, and instead go through Pham Nuwen. We
	// flip a bit in the payment hash to allow resending this payment.
	payment.paymentHash[1] ^= 1
	paymentPreImage, rt, err = ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	assertExpectedPath(paymentPreImage, rt)
}

// TestSendPaymentErrorPathPruning tests that the send of candidate routes
// properly gets pruned in response to ForwardingError response from the
// underlying SendToSwitch function.
func TestSendPaymentErrorPathPruning(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 1000 satoshis, with a maximum of 1000 satoshis in fees.
	paymentAmt := lnwire.NewMSatFromSatoshis(1000)
	payment := createDummyLightningPayment(
		t, ctx.aliases["sophon"], paymentAmt,
	)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	roasbeefSongoku := lnwire.NewShortChanIDFromInt(
		ctx.getChannelIDFromAlias(t, "roasbeef", "songoku"),
	)
	roasbeefPhanNuwen := lnwire.NewShortChanIDFromInt(
		ctx.getChannelIDFromAlias(t, "roasbeef", "phamnuwen"),
	)

	// First, we'll modify the SendToSwitch method to return an error
	// indicating that the channel from roasbeef to son goku is not operable
	// with an UnknownNextPeer.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefSongoku {
				// We'll first simulate an error from the first
				// hop to simulate the channel from songoku to
				// sophon not having enough capacity.
				return [32]byte{}, htlcswitch.NewForwardingError(
					&lnwire.FailTemporaryChannelFailure{},
					1,
				)
			}

			// Next, we'll create an error from phan nuwen to
			// indicate that the sophon node is not longer online,
			// which should prune out the rest of the routes.
			if firstHop == roasbeefPhanNuwen {
				return [32]byte{}, htlcswitch.NewForwardingError(
					&lnwire.FailUnknownNextPeer{}, 1,
				)
			}

			return preImage, nil
		})

	ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()

	// When we try to dispatch that payment, we should receive an error as
	// both attempts should fail and cause both routes to be pruned.
	_, _, err := ctx.router.SendPayment(payment)
	require.Error(t, err, "payment didn't return error")

	// The final error returned should also indicate that the peer wasn't
	// online (the last error we returned).
	require.Equal(t, channeldb.FailureReasonNoRoute, err)

	// Inspect the two attempts that were made before the payment failed.
	p, err := ctx.router.cfg.Control.FetchPayment(*payment.paymentHash)
	require.NoError(t, err)

	htlcs := p.GetHTLCs()
	require.Equal(t, 2, len(htlcs), "expected two attempts")

	// We expect the first attempt to have failed with a
	// TemporaryChannelFailure, the second with UnknownNextPeer.
	msg := htlcs[0].Failure.Message
	_, ok := msg.(*lnwire.FailTemporaryChannelFailure)
	require.True(t, ok, "unexpected fail message")

	msg = htlcs[1].Failure.Message
	_, ok = msg.(*lnwire.FailUnknownNextPeer)
	require.True(t, ok, "unexpected fail message")

	err = ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()
	require.NoError(t, err, "reset history failed")

	// Next, we'll modify the SendToSwitch method to indicate that the
	// connection between songoku and isn't up.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefSongoku {
				failure := htlcswitch.NewForwardingError(
					&lnwire.FailUnknownNextPeer{}, 1,
				)
				return [32]byte{}, failure
			}

			return preImage, nil
		})

	// This shouldn't return an error, as we'll make a payment attempt via
	// the pham nuwen channel based on the assumption that there might be an
	// intermittent issue with the songoku <-> sophon channel.
	paymentPreImage, rt, err := ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	// This path should go: roasbeef -> pham nuwen -> sophon
	require.Equal(t, 2, len(rt.Hops), "incorrect route length")
	require.Equal(t, preImage[:], paymentPreImage[:], "incorrect preimage")
	require.Equalf(t,
		ctx.aliases["phamnuwen"], rt.Hops[0].PubKeyBytes,
		"route should go through phamnuwen as first hop, "+
			"instead passes through: %v",
		getAliasFromPubKey(rt.Hops[0].PubKeyBytes, ctx.aliases),
	)

	ctx.router.cfg.MissionControl.(*MissionControl).ResetHistory()

	// Finally, we'll modify the SendToSwitch function to indicate that the
	// roasbeef -> luoji channel has insufficient capacity. This should
	// again cause us to instead go via the satoshi route.
	ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
		func(firstHop lnwire.ShortChannelID) ([32]byte, error) {

			if firstHop == roasbeefSongoku {
				// We'll first simulate an error from the first
				// outgoing link to simulate the channel from luo ji to
				// roasbeef not having enough capacity.
				return [32]byte{}, htlcswitch.NewForwardingError(
					&lnwire.FailTemporaryChannelFailure{},
					1,
				)
			}
			return preImage, nil
		})

	// We flip a bit in the payment hash to allow resending this payment.
	payment.paymentHash[1] ^= 1
	paymentPreImage, rt, err = ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	// This should succeed finally.  The route selected should have two
	// hops.
	require.Equal(t, 2, len(rt.Hops), "incorrect route length")

	// The preimage should match up with the once created above.
	require.Equal(t, preImage[:], paymentPreImage[:], "incorrect preimage")

	// The route should have satoshi as the first hop.
	require.Equalf(t,
		ctx.aliases["phamnuwen"], rt.Hops[0].PubKeyBytes,
		"route should go through phamnuwen as first hop, "+
			"instead passes through: %v",
		getAliasFromPubKey(rt.Hops[0].PubKeyBytes, ctx.aliases),
	)
}

// TestFindPathFeeWeighting tests that the findPath method will properly prefer
// routes with lower fees over routes with lower time lock values. This is
// meant to exercise the fact that the internal findPath method ranks edges
// with the square of the total fee in order bias towards lower fees.
func TestFindPathFeeWeighting(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	sourceNode, err := ctx.graph.SourceNode()
	require.NoError(t, err, "unable to fetch source node")

	amt := lnwire.MilliSatoshi(100)

	target := ctx.aliases["luoji"]

	// We'll now attempt a path finding attempt using this set up. Due to
	// the edge weighting, we should select the direct path over the 2 hop
	// path even though the direct path has a higher potential time lock.
	path, err := dbFindPath(
		ctx.graph, nil, &mockBandwidthHints{},
		noRestrictions,
		testPathFindingConfig,
		sourceNode.PubKeyBytes, target, amt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")

	// The route that was chosen should be exactly one hop, and should be
	// directly to luoji.
	require.Len(t, path, 1)
	require.Equal(t, ctx.aliases["luoji"], path[0].policy.ToNodePubKey())
}

// TestEmptyRoutesGenerateSphinxPacket tests that the generateSphinxPacket
// function is able to gracefully handle being passed a nil set of hops for the
// route by the caller.
func TestEmptyRoutesGenerateSphinxPacket(t *testing.T) {
	t.Parallel()

	sessionKey, _ := btcec.NewPrivateKey()
	emptyRoute := &route.Route{}
	_, _, err := generateSphinxPacket(emptyRoute, testHash[:], sessionKey)
	require.ErrorIs(t, err, route.ErrNoRouteHopsProvided)
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

	testGraph, err := createTestGraphFromChannels(
		t, true, testChannels, "a",
	)
	require.NoError(t, err, "unable to create graph")

	const startingBlockHeight = 101
	ctx := createTestCtxFromGraphInstance(t, startingBlockHeight, testGraph)

	// Create a payment to node c.
	payment := createDummyLightningPayment(
		t, ctx.aliases["c"], lnwire.NewMSatFromSatoshis(1000),
	)

	// We'll modify the SendToSwitch method so that it simulates hop b as a
	// node that returns an unparsable failure if approached via the a->b
	// channel.
	dispatcher, ok := ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld) //nolint:lll
	require.True(t, ok)

	dispatcher.setPaymentResult(func(firstHop lnwire.ShortChannelID) (
		[32]byte, error) {

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
	_, _, err = ctx.router.SendPayment(payment)
	require.NoErrorf(t, err, "unable to send payment: %v",
		payment.paymentHash)

	// Next we modify payment result to return an unknown failure.
	dispatcher, ok = ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld) //nolint:lll
	require.True(t, ok)

	dispatcher.setPaymentResult(func(firstHop lnwire.ShortChannelID) (
		[32]byte, error) {

		// If channel a->b is used, simulate that the failure
		// couldn't be decoded (FailureMessage is nil).
		if firstHop.ToUint64() == 2 {
			return [32]byte{},
				htlcswitch.NewUnknownForwardingError(1)
		}

		// Otherwise the payment succeeds.
		return lntypes.Preimage{}, nil
	})

	// Send off the payment request to the router. We expect the payment to
	// fail because both routes have been pruned.
	payment.paymentHash[1] ^= 1
	_, _, err = ctx.router.SendPayment(payment)
	if err == nil {
		t.Fatalf("expected payment to fail")
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

	testGraph, err := createTestGraphFromChannels(t, true, testChannels, "a")
	require.NoError(t, err, "unable to create graph")

	const startingBlockHeight = 101
	ctx := createTestCtxFromGraphInstance(t, startingBlockHeight, testGraph)

	// Set up an init channel for the control tower, such that we can make
	// sure the payment is initiated correctly.
	init := make(chan initArgs, 1)
	ctx.router.cfg.Control.(*mockControlTowerOld).init = init

	// Setup a route from source a to destination c. The route will be used
	// in a call to SendToRoute. SendToRoute also applies channel updates,
	// but it saves us from including RequestRoute in the test scope too.
	const payAmt = lnwire.MilliSatoshi(10000)
	hop1 := ctx.aliases["b"]
	hop2 := ctx.aliases["c"]
	hops := []*route.Hop{
		{
			ChannelID:     1,
			PubKeyBytes:   hop1,
			AmtToForward:  payAmt,
			LegacyPayload: true,
		},
		{
			ChannelID:     2,
			PubKeyBytes:   hop2,
			AmtToForward:  payAmt,
			LegacyPayload: true,
		},
	}

	rt, err := route.NewRouteFromHops(payAmt, 100, ctx.aliases["a"], hops)
	require.NoError(t, err, "unable to create route")

	finalHopIndex := len(hops)
	testCases := map[int]lnwire.FailureMessage{
		finalHopIndex: lnwire.NewFailIncorrectDetails(payAmt, 100),
		1: &lnwire.FailFeeInsufficient{
			Update: lnwire.ChannelUpdate{},
		},
	}

	for failIndex, errorType := range testCases {
		failIndex := failIndex
		errorType := errorType

		t.Run(fmt.Sprintf("%T", errorType), func(t *testing.T) {
			// We'll modify the SendToSwitch method so that it
			// simulates a failed payment with an error originating
			// from the final hop in the route.
			ctx.router.cfg.Payer.(*mockPaymentAttemptDispatcherOld).setPaymentResult(
				func(firstHop lnwire.ShortChannelID) ([32]byte, error) {
					return [32]byte{}, htlcswitch.NewForwardingError(
						errorType, failIndex,
					)
				},
			)

			// The payment parameter is mostly redundant in
			// SendToRoute.  Can be left empty for this test.
			var payment lntypes.Hash

			// Send off the payment request to the router. The
			// specified route should be attempted and the channel
			// update should be received by router and ignored
			// because it is missing a valid
			// signature.
			_, err = ctx.router.SendToRoute(payment, rt)

			fErr, ok := err.(*htlcswitch.ForwardingError)
			require.True(
				t, ok, "expected forwarding error, got: %T", err,
			)

			require.IsType(
				t, errorType, fErr.WireMessage(),
				"expected type %T got %T", errorType,
				fErr.WireMessage(),
			)

			// Check that the correct values were used when
			// initiating the payment.
			select {
			case initVal := <-init:
				if initVal.c.Value != payAmt {
					t.Fatalf("expected %v, got %v", payAmt,
						initVal.c.Value)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("initPayment not called")
			}
		})
	}
}

// TestSendToRouteMaxHops asserts that SendToRoute fails when using a route that
// exceeds the maximum number of hops.
func TestSendToRouteMaxHops(t *testing.T) {
	t.Parallel()

	// Setup a two node network.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
	}

	testGraph, err := createTestGraphFromChannels(t, true, testChannels, "a")
	require.NoError(t, err, "unable to create graph")

	const startingBlockHeight = 101

	ctx := createTestCtxFromGraphInstance(t, startingBlockHeight, testGraph)

	// Create a 30 hop route that exceeds the maximum hop limit.
	const payAmt = lnwire.MilliSatoshi(10000)
	hopA := ctx.aliases["a"]
	hopB := ctx.aliases["b"]

	var hops []*route.Hop
	for i := 0; i < 15; i++ {
		hops = append(hops, &route.Hop{
			ChannelID:     1,
			PubKeyBytes:   hopB,
			AmtToForward:  payAmt,
			LegacyPayload: true,
		})

		hops = append(hops, &route.Hop{
			ChannelID:     1,
			PubKeyBytes:   hopA,
			AmtToForward:  payAmt,
			LegacyPayload: true,
		})
	}

	rt, err := route.NewRouteFromHops(payAmt, 100, ctx.aliases["a"], hops)
	require.NoError(t, err, "unable to create route")

	// Send off the payment request to the router. We expect an error back
	// indicating that the route is too long.
	var payHash lntypes.Hash
	_, err = ctx.router.SendToRoute(payHash, rt)
	if err != route.ErrMaxRouteHopsExceeded {
		t.Fatalf("expected ErrMaxRouteHopsExceeded, but got %v", err)
	}
}

// TestBuildRoute tests whether correct routes are built.
func TestBuildRoute(t *testing.T) {
	// Setup a three node network.
	chanCapSat := btcutil.Amount(100000)
	paymentAddrFeatures := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.PaymentAddrOptional),
		lnwire.Features,
	)
	testChannels := []*testChannel{
		// Create two local channels from a. The bandwidth is estimated
		// in this test as the channel capacity. For building routes, we
		// expected the channel with the largest estimated bandwidth to
		// be selected.
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 20000,
			MinHTLC: lnwire.NewMSatFromSatoshis(5),
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("a", "b", chanCapSat/2, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 20000,
			MinHTLC: lnwire.NewMSatFromSatoshis(5),
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat / 2),
		}, 6),

		// Create two channels from b to c. For building routes, we
		// expect the lowest cost channel to be selected. Note that this
		// isn't a situation that we are expecting in reality. Routing
		// nodes are recommended to keep their channel policies towards
		// the same peer identical.
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:   144,
			FeeRate:  50000,
			MinHTLC:  lnwire.NewMSatFromSatoshis(20),
			MaxHTLC:  lnwire.NewMSatFromSatoshis(120),
			Features: paymentAddrFeatures,
		}, 2),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:   144,
			FeeRate:  60000,
			MinHTLC:  lnwire.NewMSatFromSatoshis(20),
			MaxHTLC:  lnwire.NewMSatFromSatoshis(120),
			Features: paymentAddrFeatures,
		}, 7),

		symmetricTestChannel("a", "e", chanCapSat, &testChannelPolicy{
			Expiry:   144,
			FeeRate:  80000,
			MinHTLC:  lnwire.NewMSatFromSatoshis(5),
			MaxHTLC:  lnwire.NewMSatFromSatoshis(10),
			Features: paymentAddrFeatures,
		}, 5),
		symmetricTestChannel("e", "c", chanCapSat, &testChannelPolicy{
			Expiry:   144,
			FeeRate:  100000,
			MinHTLC:  lnwire.NewMSatFromSatoshis(20),
			MaxHTLC:  lnwire.NewMSatFromSatoshis(chanCapSat),
			Features: paymentAddrFeatures,
		}, 4),
	}

	testGraph, err := createTestGraphFromChannels(t, true, testChannels, "a")
	require.NoError(t, err, "unable to create graph")

	const startingBlockHeight = 101

	ctx := createTestCtxFromGraphInstance(t, startingBlockHeight, testGraph)

	checkHops := func(rt *route.Route, expected []uint64,
		payAddr [32]byte) {

		t.Helper()

		if len(rt.Hops) != len(expected) {
			t.Fatal("hop count mismatch")
		}
		for i, hop := range rt.Hops {
			if hop.ChannelID != expected[i] {
				t.Fatalf("expected channel %v at pos %v, but "+
					"got channel %v",
					expected[i], i, hop.ChannelID)
			}
		}

		lastHop := rt.Hops[len(rt.Hops)-1]
		require.NotNil(t, lastHop.MPP)
		require.Equal(t, lastHop.MPP.PaymentAddr(), payAddr)
	}

	var payAddr [32]byte
	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)

	// Create hop list from the route node pubkeys.
	hops := []route.Vertex{
		ctx.aliases["b"], ctx.aliases["c"],
	}
	amt := lnwire.NewMSatFromSatoshis(100)

	// Build the route for the given amount.
	rt, err := ctx.router.BuildRoute(
		&amt, hops, nil, 40, &payAddr,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Check that we get the expected route back. The total amount should be
	// the amount to deliver to hop c (100 sats) plus the max fee for the
	// connection b->c (6 sats).
	checkHops(rt, []uint64{1, 7}, payAddr)
	if rt.TotalAmount != 106000 {
		t.Fatalf("unexpected total amount %v", rt.TotalAmount)
	}

	// Build the route for the minimum amount.
	rt, err = ctx.router.BuildRoute(
		nil, hops, nil, 40, &payAddr,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Check that we get the expected route back. The minimum that we can
	// send from b to c is 20 sats. Hop b charges 1200 msat for the
	// forwarding. The channel between hop a and b can carry amounts in the
	// range [5, 100], so 21200 msats is the minimum amount for this route.
	checkHops(rt, []uint64{1, 7}, payAddr)
	if rt.TotalAmount != 21200 {
		t.Fatalf("unexpected total amount %v", rt.TotalAmount)
	}

	// Test a route that contains incompatible channel htlc constraints.
	// There is no amount that can pass through both channel 5 and 4.
	hops = []route.Vertex{
		ctx.aliases["e"], ctx.aliases["c"],
	}
	_, err = ctx.router.BuildRoute(
		nil, hops, nil, 40, nil,
	)
	errNoChannel, ok := err.(ErrNoChannel)
	if !ok {
		t.Fatalf("expected incompatible policies error, but got %v",
			err)
	}
	if errNoChannel.position != 0 {
		t.Fatalf("unexpected no channel error position")
	}
	if errNoChannel.fromNode != ctx.aliases["a"] {
		t.Fatalf("unexpected no channel error node")
	}
}

// TestGetPathEdges tests that the getPathEdges function returns the expected
// edges and amount when given a set of unifiers and does not panic.
func TestGetPathEdges(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	testCases := []struct {
		sourceNode     route.Vertex
		amt            lnwire.MilliSatoshi
		unifiers       []*edgeUnifier
		bandwidthHints *bandwidthManager
		hops           []route.Vertex

		expectedEdges []*models.CachedEdgePolicy
		expectedAmt   lnwire.MilliSatoshi
		expectedErr   string
	}{{
		sourceNode: ctx.aliases["roasbeef"],
		unifiers: []*edgeUnifier{
			{
				edges:     []*unifiedEdge{},
				localChan: true,
			},
		},
		expectedErr: fmt.Sprintf("no matching outgoing channel "+
			"available for node 0 (%v)", ctx.aliases["roasbeef"]),
	}}

	for _, tc := range testCases {
		pathEdges, amt, err := getPathEdges(
			tc.sourceNode, tc.amt, tc.unifiers, tc.bandwidthHints,
			tc.hops,
		)

		if tc.expectedErr != "" {
			require.Error(t, err)
			require.ErrorContains(t, err, tc.expectedErr)

			continue
		}

		require.NoError(t, err)
		require.Equal(t, pathEdges, tc.expectedEdges)
		require.Equal(t, amt, tc.expectedAmt)
	}
}

// TestSendToRouteSkipTempErrSuccess validates a successful payment send.
func TestSendToRouteSkipTempErrSuccess(t *testing.T) {
	t.Parallel()

	var (
		payHash lntypes.Hash
		payAmt  = lnwire.MilliSatoshi(10000)
	)

	preimage := lntypes.Preimage{1}
	testAttempt := makeSettledAttempt(t, int(payAmt), preimage)

	node, err := createTestNode()
	require.NoError(t, err)

	// Create a simple 1-hop route.
	hops := []*route.Hop{
		{
			ChannelID:        1,
			PubKeyBytes:      node.PubKeyBytes,
			AmtToForward:     payAmt,
			OutgoingTimeLock: 120,
			MPP:              record.NewMPP(payAmt, [32]byte{}),
		},
	}
	rt, err := route.NewRouteFromHops(payAmt, 100, node.PubKeyBytes, hops)
	require.NoError(t, err)

	// Create mockers.
	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}
	missionControl := &mockMissionControl{}

	// Create the router.
	router := &ChannelRouter{cfg: &Config{
		Control:        controlTower,
		Payer:          payer,
		MissionControl: missionControl,
		Clock:          clock.NewTestClock(time.Unix(1, 0)),
		NextPaymentID: func() (uint64, error) {
			return 0, nil
		},
	}}

	// Register mockers with the expected method calls.
	controlTower.On("InitPayment", payHash, mock.Anything).Return(nil)
	controlTower.On("RegisterAttempt", payHash, mock.Anything).Return(nil)
	controlTower.On("SettleAttempt",
		payHash, mock.Anything, mock.Anything,
	).Return(testAttempt, nil)

	payer.On("SendHTLC",
		mock.Anything, mock.Anything, mock.Anything,
	).Return(nil)

	// Create a buffered chan and it will be returned by GetAttemptResult.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	payer.On("GetAttemptResult",
		mock.Anything, mock.Anything, mock.Anything,
	).Return(resultChan, nil).Run(func(_ mock.Arguments) {
		// Send a successful payment result.
		resultChan <- &htlcswitch.PaymentResult{}
	})

	missionControl.On("ReportPaymentSuccess",
		mock.Anything, rt,
	).Return(nil)

	// Mock the control tower to return the mocked payment.
	payment := &mockMPPayment{}
	controlTower.On("FetchPayment", payHash).Return(payment, nil).Once()

	// Mock the payment to return nil failure reason.
	payment.On("TerminalInfo").Return(nil, nil)

	// Expect a successful send to route.
	attempt, err := router.SendToRouteSkipTempErr(payHash, rt)
	require.NoError(t, err)
	require.Equal(t, testAttempt, attempt)

	// Assert the above methods are called as expected.
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
	missionControl.AssertExpectations(t)
	payment.AssertExpectations(t)
}

// TestSendToRouteSkipTempErrNonMPP checks that an error is return when
// skipping temp error for non-MPP.
func TestSendToRouteSkipTempErrNonMPP(t *testing.T) {
	t.Parallel()

	var (
		payHash lntypes.Hash
		payAmt  = lnwire.MilliSatoshi(10000)
	)

	node, err := createTestNode()
	require.NoError(t, err)

	// Create a simple 1-hop route without the MPP field.
	hops := []*route.Hop{
		{
			ChannelID:    1,
			PubKeyBytes:  node.PubKeyBytes,
			AmtToForward: payAmt,
		},
	}
	rt, err := route.NewRouteFromHops(payAmt, 100, node.PubKeyBytes, hops)
	require.NoError(t, err)

	// Create mockers.
	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}
	missionControl := &mockMissionControl{}

	// Create the router.
	router := &ChannelRouter{cfg: &Config{
		Control:        controlTower,
		Payer:          payer,
		MissionControl: missionControl,
		Clock:          clock.NewTestClock(time.Unix(1, 0)),
		NextPaymentID: func() (uint64, error) {
			return 0, nil
		},
	}}

	// Expect an error to be returned.
	attempt, err := router.SendToRouteSkipTempErr(payHash, rt)
	require.ErrorIs(t, ErrSkipTempErr, err)
	require.Nil(t, attempt)

	// Assert the above methods are not called.
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
	missionControl.AssertExpectations(t)
}

// TestSendToRouteSkipTempErrTempFailure validates a temporary failure won't
// cause the payment to be failed.
func TestSendToRouteSkipTempErrTempFailure(t *testing.T) {
	t.Parallel()

	var (
		payHash lntypes.Hash
		payAmt  = lnwire.MilliSatoshi(10000)
	)

	testAttempt := makeFailedAttempt(t, int(payAmt))
	node, err := createTestNode()
	require.NoError(t, err)

	// Create a simple 1-hop route.
	hops := []*route.Hop{
		{
			ChannelID:        1,
			PubKeyBytes:      node.PubKeyBytes,
			AmtToForward:     payAmt,
			OutgoingTimeLock: 120,
			MPP:              record.NewMPP(payAmt, [32]byte{}),
		},
	}
	rt, err := route.NewRouteFromHops(payAmt, 100, node.PubKeyBytes, hops)
	require.NoError(t, err)

	// Create mockers.
	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}
	missionControl := &mockMissionControl{}

	// Create the router.
	router := &ChannelRouter{cfg: &Config{
		Control:        controlTower,
		Payer:          payer,
		MissionControl: missionControl,
		Clock:          clock.NewTestClock(time.Unix(1, 0)),
		NextPaymentID: func() (uint64, error) {
			return 0, nil
		},
	}}

	// Create the error to be returned.
	tempErr := htlcswitch.NewForwardingError(
		&lnwire.FailTemporaryChannelFailure{}, 1,
	)

	// Register mockers with the expected method calls.
	controlTower.On("InitPayment", payHash, mock.Anything).Return(nil)
	controlTower.On("RegisterAttempt", payHash, mock.Anything).Return(nil)
	controlTower.On("FailAttempt",
		payHash, mock.Anything, mock.Anything,
	).Return(testAttempt, nil)

	payer.On("SendHTLC",
		mock.Anything, mock.Anything, mock.Anything,
	).Return(tempErr)

	// Mock the control tower to return the mocked payment.
	payment := &mockMPPayment{}
	controlTower.On("FetchPayment", payHash).Return(payment, nil).Once()

	// Mock the mission control to return a nil reason from reporting the
	// attempt failure.
	missionControl.On("ReportPaymentFail",
		mock.Anything, rt, mock.Anything, mock.Anything,
	).Return(nil, nil)

	// Mock the payment to return nil failure reason.
	payment.On("TerminalInfo").Return(nil, nil)

	// Expect a failed send to route.
	attempt, err := router.SendToRouteSkipTempErr(payHash, rt)
	require.Equal(t, tempErr, err)
	require.Equal(t, testAttempt, attempt)

	// Assert the above methods are called as expected.
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
	missionControl.AssertExpectations(t)
	payment.AssertExpectations(t)
}

// TestSendToRouteSkipTempErrPermanentFailure validates a permanent failure
// will fail the payment.
func TestSendToRouteSkipTempErrPermanentFailure(t *testing.T) {
	var (
		payHash lntypes.Hash
		payAmt  = lnwire.MilliSatoshi(10000)
	)

	testAttempt := makeFailedAttempt(t, int(payAmt))
	node, err := createTestNode()
	require.NoError(t, err)

	// Create a simple 1-hop route.
	hops := []*route.Hop{
		{
			ChannelID:        1,
			PubKeyBytes:      node.PubKeyBytes,
			AmtToForward:     payAmt,
			OutgoingTimeLock: 120,
			MPP:              record.NewMPP(payAmt, [32]byte{}),
		},
	}
	rt, err := route.NewRouteFromHops(payAmt, 100, node.PubKeyBytes, hops)
	require.NoError(t, err)

	// Create mockers.
	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}
	missionControl := &mockMissionControl{}

	// Create the router.
	router := &ChannelRouter{cfg: &Config{
		Control:        controlTower,
		Payer:          payer,
		MissionControl: missionControl,
		Clock:          clock.NewTestClock(time.Unix(1, 0)),
		NextPaymentID: func() (uint64, error) {
			return 0, nil
		},
	}}

	// Create the error to be returned.
	permErr := htlcswitch.NewForwardingError(
		&lnwire.FailIncorrectDetails{}, 1,
	)

	// Register mockers with the expected method calls.
	controlTower.On("InitPayment", payHash, mock.Anything).Return(nil)
	controlTower.On("RegisterAttempt", payHash, mock.Anything).Return(nil)

	controlTower.On("FailAttempt",
		payHash, mock.Anything, mock.Anything,
	).Return(testAttempt, nil)

	// Expect the payment to be failed.
	controlTower.On("FailPayment", payHash, mock.Anything).Return(nil)

	// Mock an error to be returned from sending the htlc.
	payer.On("SendHTLC",
		mock.Anything, mock.Anything, mock.Anything,
	).Return(permErr)

	failureReason := channeldb.FailureReasonPaymentDetails
	missionControl.On("ReportPaymentFail",
		mock.Anything, rt, mock.Anything, mock.Anything,
	).Return(&failureReason, nil)

	// Mock the control tower to return the mocked payment.
	payment := &mockMPPayment{}
	controlTower.On("FetchPayment", payHash).Return(payment, nil).Once()

	// Mock the payment to return a failure reason.
	payment.On("TerminalInfo").Return(nil, &failureReason)

	// Expect a failed send to route.
	attempt, err := router.SendToRouteSkipTempErr(payHash, rt)
	require.Equal(t, permErr, err)
	require.Equal(t, testAttempt, attempt)

	// Assert the above methods are called as expected.
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
	missionControl.AssertExpectations(t)
	payment.AssertExpectations(t)
}

// TestSendToRouteTempFailure validates a temporary failure will cause the
// payment to be failed.
func TestSendToRouteTempFailure(t *testing.T) {
	var (
		payHash lntypes.Hash
		payAmt  = lnwire.MilliSatoshi(10000)
	)

	testAttempt := makeFailedAttempt(t, int(payAmt))
	node, err := createTestNode()
	require.NoError(t, err)

	// Create a simple 1-hop route.
	hops := []*route.Hop{
		{
			ChannelID:        1,
			PubKeyBytes:      node.PubKeyBytes,
			AmtToForward:     payAmt,
			OutgoingTimeLock: 120,
			MPP:              record.NewMPP(payAmt, [32]byte{}),
		},
	}
	rt, err := route.NewRouteFromHops(payAmt, 100, node.PubKeyBytes, hops)
	require.NoError(t, err)

	// Create mockers.
	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}
	missionControl := &mockMissionControl{}

	// Create the router.
	router := &ChannelRouter{cfg: &Config{
		Control:        controlTower,
		Payer:          payer,
		MissionControl: missionControl,
		Clock:          clock.NewTestClock(time.Unix(1, 0)),
		NextPaymentID: func() (uint64, error) {
			return 0, nil
		},
	}}

	// Create the error to be returned.
	tempErr := htlcswitch.NewForwardingError(
		&lnwire.FailTemporaryChannelFailure{}, 1,
	)

	// Register mockers with the expected method calls.
	controlTower.On("InitPayment", payHash, mock.Anything).Return(nil)
	controlTower.On("RegisterAttempt", payHash, mock.Anything).Return(nil)
	controlTower.On("FailAttempt",
		payHash, mock.Anything, mock.Anything,
	).Return(testAttempt, nil)

	// Expect the payment to be failed.
	controlTower.On("FailPayment", payHash, mock.Anything).Return(nil)

	payer.On("SendHTLC",
		mock.Anything, mock.Anything, mock.Anything,
	).Return(tempErr)

	// Mock the control tower to return the mocked payment.
	payment := &mockMPPayment{}
	controlTower.On("FetchPayment", payHash).Return(payment, nil).Once()

	// Mock the payment to return nil failure reason.
	payment.On("TerminalInfo").Return(nil, nil)

	// Return a nil reason to mock a temporary failure.
	missionControl.On("ReportPaymentFail",
		mock.Anything, rt, mock.Anything, mock.Anything,
	).Return(nil, nil)

	// Expect a failed send to route.
	attempt, err := router.SendToRoute(payHash, rt)
	require.Equal(t, tempErr, err)
	require.Equal(t, testAttempt, attempt)

	// Assert the above methods are called as expected.
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
	missionControl.AssertExpectations(t)
	payment.AssertExpectations(t)
}

// TestNewRouteRequest tests creation of route requests for blinded and
// unblinded routes.
func TestNewRouteRequest(t *testing.T) {
	t.Parallel()

	//nolint:lll
	source, err := route.NewVertexFromStr("0367cec75158a4129177bfb8b269cb586efe93d751b43800d456485e81c2620ca6")
	require.NoError(t, err)
	sourcePubkey, err := btcec.ParsePubKey(source[:])
	require.NoError(t, err)

	//nolint:lll
	v1, err := route.NewVertexFromStr("026c43a8ac1cd8519985766e90748e1e06871dab0ff6b8af27e8c1a61640481318")
	require.NoError(t, err)
	pubkey1, err := btcec.ParsePubKey(v1[:])
	require.NoError(t, err)

	//nolint:lll
	v2, err := route.NewVertexFromStr("03c19f0027ffbb0ae0e14a4d958788793f9d74e107462473ec0c3891e4feb12e99")
	require.NoError(t, err)
	pubkey2, err := btcec.ParsePubKey(v2[:])
	require.NoError(t, err)

	var (
		unblindedCltv uint16 = 500
		blindedCltv   uint16 = 1000
	)

	blindedSelfIntro := &BlindedPayment{
		CltvExpiryDelta: blindedCltv,
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: sourcePubkey,
			BlindedHops:       []*sphinx.BlindedHopInfo{{}},
		},
	}

	blindedOtherIntro := &BlindedPayment{
		CltvExpiryDelta: blindedCltv,
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: pubkey1,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{},
			},
		},
	}

	blindedMultiHop := &BlindedPayment{
		CltvExpiryDelta: blindedCltv,
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: pubkey1,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{},
				{
					BlindedNodePub: pubkey2,
				},
			},
		},
	}

	testCases := []struct {
		name           string
		target         *route.Vertex
		routeHints     RouteHints
		blindedPayment *BlindedPayment
		finalExpiry    uint16

		expectedTarget route.Vertex
		expectedCltv   uint16
		err            error
	}{
		{
			name:           "blinded and target",
			target:         &v1,
			blindedPayment: blindedOtherIntro,
			err:            ErrTargetAndBlinded,
		},
		{
			// For single-hop blinded we have a final cltv.
			name:           "blinded intro node only",
			blindedPayment: blindedOtherIntro,
			expectedTarget: v1,
			expectedCltv:   blindedCltv,
			err:            nil,
		},
		{
			// For multi-hop blinded, we have no final cltv.
			name:           "blinded multi-hop",
			blindedPayment: blindedMultiHop,
			expectedTarget: v2,
			expectedCltv:   0,
			err:            nil,
		},
		{
			name:           "unblinded",
			target:         &v2,
			finalExpiry:    unblindedCltv,
			expectedTarget: v2,
			expectedCltv:   unblindedCltv,
			err:            nil,
		},
		{
			name:           "source node intro",
			blindedPayment: blindedSelfIntro,
			err:            ErrSelfIntro,
		},
		{
			name:           "hints and blinded",
			blindedPayment: blindedMultiHop,
			routeHints: make(
				map[route.Vertex][]AdditionalEdge,
			),
			err: ErrHintsAndBlinded,
		},
		{
			name:           "expiry and blinded",
			blindedPayment: blindedMultiHop,
			finalExpiry:    unblindedCltv,
			err:            ErrExpiryAndBlinded,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var (
				blindedPathInfo *BlindedPaymentPathSet
				expectedTarget  = testCase.expectedTarget
			)
			if testCase.blindedPayment != nil {
				blindedPathInfo, err = NewBlindedPaymentPathSet(
					[]*BlindedPayment{
						testCase.blindedPayment,
					},
				)
				require.NoError(t, err)

				expectedTarget = route.NewVertex(
					blindedPathInfo.TargetPubKey(),
				)
			}

			req, err := NewRouteRequest(
				source, testCase.target, 1000, 0, nil, nil,
				testCase.routeHints, blindedPathInfo,
				testCase.finalExpiry,
			)
			require.ErrorIs(t, err, testCase.err)

			// Skip request validation if we got a non-nil error.
			if err != nil {
				return
			}

			require.Equal(t, req.Target, expectedTarget)
			require.Equal(
				t, req.FinalExpiry, testCase.expectedCltv,
			)
		})
	}
}

// TestAddEdgeUnknownVertexes tests that if an edge is added that contains two
// vertexes which we don't know of, the edge should be available for use
// regardless. This is due to the fact that we don't actually need node
// announcements for the channel vertexes to be able to use the channel.
func TestAddEdgeUnknownVertexes(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	var pub1 [33]byte
	copy(pub1[:], priv1.PubKey().SerializeCompressed())

	var pub2 [33]byte
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	// The two nodes we are about to add should not exist yet.
	_, exists1, err := ctx.graph.HasLightningNode(pub1)
	require.NoError(t, err, "unable to query graph")
	require.False(t, exists1)

	_, exists2, err := ctx.graph.HasLightningNode(pub2)
	require.NoError(t, err, "unable to query graph")
	require.False(t, exists2)

	// Add the edge between the two unknown nodes to the graph, and check
	// that the nodes are found after the fact.
	_, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500,
	)
	require.NoError(t, err, "unable to create channel edge")

	edge := &models.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
	}
	require.NoError(t, ctx.graph.AddChannelEdge(edge))

	// We must add the edge policy to be able to use the edge for route
	// finding.
	edgePolicy := &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		ToNode:                    edge.NodeKey2Bytes,
	}
	edgePolicy.ChannelFlags = 0

	require.NoError(t, ctx.graph.UpdateEdgePolicy(edgePolicy))

	// Create edge in the other direction as well.
	edgePolicy = &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		ToNode:                    edge.NodeKey1Bytes,
	}
	edgePolicy.ChannelFlags = 1

	require.NoError(t, ctx.graph.UpdateEdgePolicy(edgePolicy))

	// After adding the edge between the two previously unknown nodes, they
	// should have been added to the graph.
	_, exists1, err = ctx.graph.HasLightningNode(pub1)
	require.NoError(t, err, "unable to query graph")
	require.True(t, exists1)

	_, exists2, err = ctx.graph.HasLightningNode(pub2)
	require.NoError(t, err, "unable to query graph")
	require.True(t, exists2)

	// We will connect node1 to the rest of the test graph, and make sure
	// we can find a route to node2, which will use the just added channel
	// edge.

	// We will connect node 1 to "sophon"
	connectNode := ctx.aliases["sophon"]
	connectNodeKey, err := btcec.ParsePubKey(connectNode[:])
	require.NoError(t, err)

	var (
		pubKey1 *btcec.PublicKey
		pubKey2 *btcec.PublicKey
	)
	node1Bytes := priv1.PubKey().SerializeCompressed()
	node2Bytes := connectNode
	if bytes.Compare(node1Bytes, node2Bytes[:]) == -1 {
		pubKey1 = priv1.PubKey()
		pubKey2 = connectNodeKey
	} else {
		pubKey1 = connectNodeKey
		pubKey2 = priv1.PubKey()
	}

	_, _, chanID, err = createChannelEdge(
		pubKey1.SerializeCompressed(), pubKey2.SerializeCompressed(),
		10000, 510)
	require.NoError(t, err, "unable to create channel edge")

	edge = &models.ChannelEdgeInfo{
		ChannelID: chanID.ToUint64(),
		AuthProof: nil,
	}
	copy(edge.NodeKey1Bytes[:], node1Bytes)
	edge.NodeKey2Bytes = node2Bytes
	copy(edge.BitcoinKey1Bytes[:], node1Bytes)
	edge.BitcoinKey2Bytes = node2Bytes

	require.NoError(t, ctx.graph.AddChannelEdge(edge))

	edgePolicy = &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		ToNode:                    edge.NodeKey2Bytes,
	}
	edgePolicy.ChannelFlags = 0

	require.NoError(t, ctx.graph.UpdateEdgePolicy(edgePolicy))

	edgePolicy = &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		ToNode:                    edge.NodeKey1Bytes,
	}
	edgePolicy.ChannelFlags = 1

	require.NoError(t, ctx.graph.UpdateEdgePolicy(edgePolicy))

	// We should now be able to find a route to node 2.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	targetNode := priv2.PubKey()
	var targetPubKeyBytes route.Vertex
	copy(targetPubKeyBytes[:], targetNode.SerializeCompressed())

	req, err := NewRouteRequest(
		ctx.router.cfg.SelfNode, &targetPubKeyBytes,
		paymentAmt, 0, noRestrictions, nil, nil, nil, MinCLTVDelta,
	)
	require.NoError(t, err, "invalid route request")
	_, _, err = ctx.router.FindRoute(req)
	require.NoError(t, err, "unable to find any routes")

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

	require.NoError(t, ctx.graph.AddLightningNode(n1))

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

	require.NoError(t, ctx.graph.AddLightningNode(n2))

	// Should still be able to find the route, and the info should be
	// updated.
	req, err = NewRouteRequest(
		ctx.router.cfg.SelfNode, &targetPubKeyBytes,
		paymentAmt, 0, noRestrictions, nil, nil, nil, MinCLTVDelta,
	)
	require.NoError(t, err, "invalid route request")

	_, _, err = ctx.router.FindRoute(req)
	require.NoError(t, err, "unable to find any routes")

	copy1, err := ctx.graph.FetchLightningNode(pub1)
	require.NoError(t, err, "unable to fetch node")

	require.Equal(t, n1.Alias, copy1.Alias)

	copy2, err := ctx.graph.FetchLightningNode(pub2)
	require.NoError(t, err, "unable to fetch node")

	require.Equal(t, n2.Alias, copy2.Alias)
}

func createDummyLightningPayment(t *testing.T,
	target route.Vertex, amt lnwire.MilliSatoshi) *LightningPayment {

	var preImage lntypes.Preimage
	_, err := rand.Read(preImage[:])
	require.NoError(t, err, "unable to generate preimage")

	payHash := preImage.Hash()

	return &LightningPayment{
		Target:      target,
		Amount:      amt,
		FeeLimit:    noFeeLimit,
		paymentHash: &payHash,
	}
}

type mockGraphBuilder struct {
	rejectUpdate bool
	updateEdge   func(update *models.ChannelEdgePolicy) error
}

func newMockGraphBuilder(graph graph.DB) *mockGraphBuilder {
	return &mockGraphBuilder{
		updateEdge: func(update *models.ChannelEdgePolicy) error {
			return graph.UpdateEdgePolicy(update)
		},
	}
}

func (m *mockGraphBuilder) setNextReject(reject bool) {
	m.rejectUpdate = reject
}

func (m *mockGraphBuilder) ApplyChannelUpdate(msg *lnwire.ChannelUpdate) bool {
	if m.rejectUpdate {
		return false
	}

	err := m.updateEdge(&models.ChannelEdgePolicy{
		SigBytes:                  msg.Signature.ToSignatureBytes(),
		ChannelID:                 msg.ShortChannelID.ToUint64(),
		LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
		MessageFlags:              msg.MessageFlags,
		ChannelFlags:              msg.ChannelFlags,
		TimeLockDelta:             msg.TimeLockDelta,
		MinHTLC:                   msg.HtlcMinimumMsat,
		MaxHTLC:                   msg.HtlcMaximumMsat,
		FeeBaseMSat:               lnwire.MilliSatoshi(msg.BaseFee),
		FeeProportionalMillionths: lnwire.MilliSatoshi(msg.FeeRate),
		ExtraOpaqueData:           msg.ExtraOpaqueData,
	})

	return err == nil
}

type mockChain struct {
	lnwallet.BlockChainIO

	blocks           map[chainhash.Hash]*wire.MsgBlock
	blockIndex       map[uint32]chainhash.Hash
	blockHeightIndex map[chainhash.Hash]uint32

	utxos map[wire.OutPoint]wire.TxOut

	bestHeight int32

	sync.RWMutex
}

func newMockChain(currentHeight uint32) *mockChain {
	return &mockChain{
		bestHeight:       int32(currentHeight),
		blocks:           make(map[chainhash.Hash]*wire.MsgBlock),
		utxos:            make(map[wire.OutPoint]wire.TxOut),
		blockIndex:       make(map[uint32]chainhash.Hash),
		blockHeightIndex: make(map[chainhash.Hash]uint32),
	}
}

func (m *mockChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	m.RLock()
	defer m.RUnlock()

	blockHash := m.blockIndex[uint32(m.bestHeight)]

	return &blockHash, m.bestHeight, nil
}

func createChannelEdge(bitcoinKey1, bitcoinKey2 []byte,
	chanValue btcutil.Amount, fundingHeight uint32) (*wire.MsgTx,
	*wire.OutPoint, *lnwire.ShortChannelID, error) {

	fundingTx := wire.NewMsgTx(2)
	_, tx, err := input.GenFundingPkScript(
		bitcoinKey1,
		bitcoinKey2,
		int64(chanValue),
	)
	if err != nil {
		return nil, nil, nil, err
	}

	fundingTx.TxOut = append(fundingTx.TxOut, tx)
	chanUtxo := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 0,
	}

	// Our fake channel will be "confirmed" at height 101.
	chanID := &lnwire.ShortChannelID{
		BlockHeight: fundingHeight,
		TxIndex:     0,
		TxPosition:  0,
	}

	return fundingTx, &chanUtxo, chanID, nil
}

// TestFindBlindedPathsWithMC tests that the FindBlindedPaths method correctly
// selects a set of blinded paths by using mission control data to select the
// paths with the highest success probability.
func TestFindBlindedPathsWithMC(t *testing.T) {
	t.Parallel()

	rbFeatureBits := []lnwire.FeatureBit{
		lnwire.RouteBlindingOptional,
	}

	// Create the following graph and let all the nodes advertise support
	// for blinded paths.
	//
	//			  C
	//			/   \
	//		       /     \
	//		E -- A -- F -- D
	//		       \     /
	//			\   /
	//		          B
	//
	featuresWithRouteBlinding := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(rbFeatureBits...), lnwire.Features,
	)

	policyWithRouteBlinding := &testChannelPolicy{
		Expiry:   144,
		FeeRate:  400,
		MinHTLC:  1,
		MaxHTLC:  100000000,
		Features: featuresWithRouteBlinding,
	}

	testChannels := []*testChannel{
		symmetricTestChannel(
			"eve", "alice", 100000, policyWithRouteBlinding, 1,
		),
		symmetricTestChannel(
			"alice", "charlie", 100000, policyWithRouteBlinding, 2,
		),
		symmetricTestChannel(
			"alice", "bob", 100000, policyWithRouteBlinding, 3,
		),
		symmetricTestChannel(
			"charlie", "dave", 100000, policyWithRouteBlinding, 4,
		),
		symmetricTestChannel(
			"bob", "dave", 100000, policyWithRouteBlinding, 5,
		),
		symmetricTestChannel(
			"alice", "frank", 100000, policyWithRouteBlinding, 6,
		),
		symmetricTestChannel(
			"frank", "dave", 100000, policyWithRouteBlinding, 7,
		),
	}

	testGraph, err := createTestGraphFromChannels(
		t, true, testChannels, "dave", rbFeatureBits...,
	)
	require.NoError(t, err)

	ctx := createTestCtxFromGraphInstance(t, 101, testGraph)

	var (
		alice   = ctx.aliases["alice"]
		bob     = ctx.aliases["bob"]
		charlie = ctx.aliases["charlie"]
		dave    = ctx.aliases["dave"]
		eve     = ctx.aliases["eve"]
		frank   = ctx.aliases["frank"]
	)

	// Create a mission control store which initially sets the success
	// probability of each node pair to 1.
	missionControl := map[route.Vertex]map[route.Vertex]float64{
		eve: {alice: 1},
		alice: {
			charlie: 1,
			bob:     1,
			frank:   1,
		},
		charlie: {dave: 1},
		bob:     {dave: 1},
		frank:   {dave: 1},
	}

	// probabilitySrc is a helper that returns the mission control success
	// probability of a forward between two vertices.
	probabilitySrc := func(from route.Vertex, to route.Vertex,
		amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64 {

		return missionControl[from][to]
	}

	// All the probabilities are set to 1. So if we restrict the path length
	// to 2 and allow a max of 3 routes, then we expect three paths here.
	routes, err := ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 2,
			NumHops:                  2,
			MaxNumPaths:              3,
		},
	)
	require.NoError(t, err)
	require.Len(t, routes, 3)

	// assertPaths checks that the resulting set of paths is equal to the
	// expected set and that the order of the paths is correct.
	assertPaths := func(paths []*route.Route, expectedPaths []string) {
		require.Len(t, paths, len(expectedPaths))

		var actualPaths []string
		for _, path := range paths {
			label := getAliasFromPubKey(
				path.SourcePubKey, ctx.aliases,
			) + ","

			for _, hop := range path.Hops {
				label += getAliasFromPubKey(
					hop.PubKeyBytes, ctx.aliases,
				) + ","
			}

			actualPaths = append(
				actualPaths, strings.TrimRight(label, ","),
			)
		}

		for i, path := range expectedPaths {
			require.Equal(t, expectedPaths[i], path)
		}
	}

	// Now, let's lower the MC probability of the B-D to 0.5 and F-D link to
	// 0.25. We will leave the MaxNumPaths as 3 and so all paths should
	// still be returned but the order should be:
	// 1) A -> C -> D
	// 2) A -> B -> D
	// 3) A -> F -> D
	missionControl[bob][dave] = 0.5
	missionControl[frank][dave] = 0.25
	routes, err = ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 2,
			NumHops:                  2,
			MaxNumPaths:              3,
		},
	)
	require.NoError(t, err)
	assertPaths(routes, []string{
		"alice,charlie,dave",
		"alice,bob,dave",
		"alice,frank,dave",
	})

	// Just to show that the above result was not a fluke, let's change
	// the C->D link to be the weak one.
	missionControl[charlie][dave] = 0.125
	routes, err = ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 2,
			NumHops:                  2,
			MaxNumPaths:              3,
		},
	)
	require.NoError(t, err)
	assertPaths(routes, []string{
		"alice,bob,dave",
		"alice,frank,dave",
		"alice,charlie,dave",
	})

	// Change the MaxNumPaths to 1 to assert that only the best route is
	// returned.
	routes, err = ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 2,
			NumHops:                  2,
			MaxNumPaths:              1,
		},
	)
	require.NoError(t, err)
	assertPaths(routes, []string{
		"alice,bob,dave",
	})

	// Test the edge case where Dave, the recipient, is also the
	// introduction node.
	routes, err = ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 0,
			NumHops:                  0,
			MaxNumPaths:              1,
		},
	)
	require.NoError(t, err)
	assertPaths(routes, []string{
		"dave",
	})

	// Finally, we make one of the routes have a probability less than the
	// minimum. This means we expect that route not to be chosen.
	missionControl[charlie][dave] = DefaultMinRouteProbability
	routes, err = ctx.router.FindBlindedPaths(
		dave, 1000, probabilitySrc, &BlindedPathRestrictions{
			MinDistanceFromIntroNode: 2,
			NumHops:                  2,
			MaxNumPaths:              3,
		},
	)
	require.NoError(t, err)
	assertPaths(routes, []string{
		"alice,bob,dave",
		"alice,frank,dave",
	})
}
