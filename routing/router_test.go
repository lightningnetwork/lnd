package routing

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/roasbeef/btcd/wire"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
)

type testCtx struct {
	router *ChannelRouter

	graph *channeldb.ChannelGraph

	aliases map[string]*btcec.PublicKey

	chain *mockChain

	chainView *mockChainView
}

func createTestCtx(startingHeight uint32, testGraph ...string) (*testCtx, func(), error) {
	var (
		graph      *channeldb.ChannelGraph
		sourceNode *channeldb.LightningNode
		cleanup    func()
		err        error
	)

	aliasMap := make(map[string]*btcec.PublicKey)

	// If the testGraph isn't set, then we'll create an empty graph to
	// start out with. Our usage of a variadic parameter allows caller to
	// omit the testGraph argument all together if they wish to start with
	// a blank graph.
	if testGraph == nil {
		// First we'll set up a test graph for usage within the test.
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
	} else {
		// Otherwise, we'll attempt to locate and parse out the file
		// that encodes the graph that our tests should be run against.
		graph, cleanup, aliasMap, err = parseTestGraph(testGraph[0])
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create test graph: %v", err)
		}
	}

	// Next we'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send messages won't
	// be populated.
	chain := newMockChain(startingHeight)
	chainView := newMockChainView()
	router, err := New(Config{
		Graph:     graph,
		Chain:     chain,
		ChainView: chainView,
		SendToSwitch: func(_ *btcec.PublicKey,
			_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {
			return [32]byte{}, nil
		},
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create router %v", err)
	}
	if err := router.Start(); err != nil {
		return nil, nil, fmt.Errorf("unable to start router: %v", err)
	}

	cleanUp := func() {
		router.Stop()
		cleanup()
	}

	return &testCtx{
		router:    router,
		graph:     graph,
		aliases:   aliasMap,
		chain:     chain,
		chainView: chainView,
	}, cleanUp, nil
}

// TestFindRoutesFeeSorting asserts that routes found by the FindRoutes method
// within the channel router are properly returned in a sorted order, with the
// lowest fee route coming first.
func TestFindRoutesFeeSorting(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight, basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// In this test we'd like to ensure proper integration of the various
	// functions that are involved in path finding, and also route
	// selection.

	// Execute a query for all possible routes between roasbeef and luo ji.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.aliases["luoji"]
	routes, err := ctx.router.FindRoutes(target, paymentAmt,
		DefaultFinalCLTVDelta)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}

	// Exactly, two such paths should be found.
	if len(routes) != 2 {
		t.Fatalf("2 routes shouldn't been selected, instead %v were: %v",
			len(routes), spew.Sdump(routes))
	}

	// The paths should properly be ranked according to their total fee
	// rate.
	if routes[0].TotalFees > routes[1].TotalFees {
		t.Fatalf("routes not ranked by total fee: %v",
			spew.Sdump(routes))
	}
}

// TestSendPaymentRouteFailureFallback tests that when sending a payment, if
// one of the target routes is seen as unavailable, then the next route in the
// queue is used instead. This process should continue until either a payment
// succeeds, or all routes have been exhausted.
func TestSendPaymentRouteFailureFallback(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight, basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 100 satoshis.
	var payHash [32]byte
	payment := LightningPayment{
		Target:      ctx.aliases["luoji"],
		Amount:      lnwire.NewMSatFromSatoshis(1000),
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	sourceNode := ctx.router.selfNode

	// We'll modify the SendToSwitch method that's been set within the
	// router's configuration to ignore the path that has luo ji as the
	// first hop. This should force the router to instead take the
	// available two hop path (through satoshi).
	ctx.router.cfg.SendToSwitch = func(n *btcec.PublicKey,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if ctx.aliases["luoji"].IsEqual(n) {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource: sourceNode.PubKey,
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
	if route.Hops[0].Channel.Node.Alias != "satoshi" {
		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			route.Hops[0].Channel.Node.Alias)
	}
}

// TestSendPaymentErrorPathPruning tests that the send of candidate routes
// properly gets pruned in response to ForwardingError response from the
// underlying SendToSwitch function.
func TestSendPaymentErrorPathPruning(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight, basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Craft a LightningPayment struct that'll send a payment from roasbeef
	// to luo ji for 100 satoshis.
	var payHash [32]byte
	payment := LightningPayment{
		Target:      ctx.aliases["luoji"],
		Amount:      lnwire.NewMSatFromSatoshis(1000),
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	sourceNode, err := ctx.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// First, we'll modify the SendToSwitch method to return an error
	// indicating that the channel from roasbeef to luoji is not operable
	// with an UnknownNextPeer.
	//
	// TODO(roasbeef): filtering should be intelligent enough so just not
	// go through satoshi at all at this point.
	ctx.router.cfg.SendToSwitch = func(n *btcec.PublicKey,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if ctx.aliases["luoji"].IsEqual(n) {
			// We'll first simulate an error from the first
			// outgoing link to simulate the channel from luo ji to
			// roasbeef not having enough capacity.
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourceNode.PubKey,
				FailureMessage: &lnwire.FailTemporaryChannelFailure{},
			}
		}

		// Next, we'll create an error from satoshi to indicate
		// that the luoji node is not longer online, which should
		// prune out the rest of the routes.
		if ctx.aliases["satoshi"].IsEqual(n) {
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
	ctx.router.cfg.SendToSwitch = func(n *btcec.PublicKey,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {

		if ctx.aliases["luoji"].IsEqual(n) {
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourceNode.PubKey,
				FailureMessage: &lnwire.FailUnknownNextPeer{},
			}
		}

		return preImage, nil
	}

	// The final error returned should also indicate that the peer wasn't
	// online (the last error we returned).
	_, _, err = ctx.router.SendPayment(&payment)
	if err == nil {
		t.Fatalf("payment didn't return error")
	}
	if !strings.Contains(err.Error(), "UnknownNextPeer") {
		t.Fatalf("expected UnknownNextPeer instead got: %v", err)
	}

	ctx.router.missionControl.ResetHistory()

	// Finally, we'll modify the SendToSwitch function to indicate that the
	// roasbeef -> luoji channel has insufficient capacity.
	ctx.router.cfg.SendToSwitch = func(n *btcec.PublicKey,
		_ *lnwire.UpdateAddHTLC, _ *sphinx.Circuit) ([32]byte, error) {
		if ctx.aliases["luoji"].IsEqual(n) {
			// We'll first simulate an error from the first
			// outgoing link to simulate the channel from luo ji to
			// roasbeef not having enough capacity.
			return [32]byte{}, &htlcswitch.ForwardingError{
				ErrorSource:    sourceNode.PubKey,
				FailureMessage: &lnwire.FailTemporaryChannelFailure{},
			}
		}
		return preImage, nil
	}

	paymentPreImage, route, err := ctx.router.SendPayment(&payment)
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
	if route.Hops[0].Channel.Node.Alias != "satoshi" {
		t.Fatalf("route should go through satoshi as first hop, "+
			"instead passes through: %v",
			route.Hops[0].Channel.Node.Alias)
	}
}

// TestAddProof checks that we can update the channel proof after channel
// info was added to the database.
func TestAddProof(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(0)
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
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight)

	// After utxo was recreated adding the edge without the proof.
	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:   chanID.ToUint64(),
		NodeKey1:    node1.PubKey,
		NodeKey2:    node2.PubKey,
		BitcoinKey1: bitcoinKey1,
		BitcoinKey2: bitcoinKey2,
		AuthProof:   nil,
	}

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
	ctx, cleanUp, err := createTestCtx(startingBlockHeight,
		basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	node := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		PubKey:               priv1.PubKey(),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node11",
		AuthSig:              testSig,
		Features:             testFeatures,
	}

	err = ctx.router.AddNode(node)
	if !IsError(err, ErrIgnored) {
		t.Fatalf("expected to get ErrIgnore, instead got: %v", err)
	}
}

// TestAddEdgeUnknownVertexes tests that if an edge is added that contains two
// vertexes which we don't know of, the edge should be available for use
// regardless. This is due to the fact that we don't actually need node
// announcements for the channel vertexes to be able to use the channel.
func TestAddEdgeUnknownVertexes(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight,
		basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// The two nodes we are about to add should not exist yet.
	_, exists1, err := ctx.graph.HasLightningNode(priv1.PubKey())
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if exists1 {
		t.Fatalf("node already existed")
	}
	_, exists2, err := ctx.graph.HasLightningNode(priv2.PubKey())
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
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight)

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:   chanID.ToUint64(),
		NodeKey1:    priv1.PubKey(),
		NodeKey2:    priv2.PubKey(),
		BitcoinKey1: bitcoinKey1,
		BitcoinKey2: bitcoinKey2,
		AuthProof:   nil,
	}
	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("expected to be able to add edge to the channel graph,"+
			" even though the vertexes were unknown: %v.", err)
	}

	// We must add the edge policy to be able to use the edge for route
	// finding.
	edgePolicy := &channeldb.ChannelEdgePolicy{
		Signature:                 testSig,
		ChannelID:                 edge.ChannelID,
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.Flags = 0

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// Create edge in the other direction as well.
	edgePolicy = &channeldb.ChannelEdgePolicy{
		Signature:                 testSig,
		ChannelID:                 edge.ChannelID,
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.Flags = 1

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// After adding the edge between the two previously unknown nodes, they
	// should have been added to the graph.
	_, exists1, err = ctx.graph.HasLightningNode(priv1.PubKey())
	if err != nil {
		t.Fatalf("unable to query graph: %v", err)
	}
	if !exists1 {
		t.Fatalf("node1 was not added to the graph")
	}
	_, exists2, err = ctx.graph.HasLightningNode(priv2.PubKey())
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
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight)

	edge = &channeldb.ChannelEdgeInfo{
		ChannelID:   chanID.ToUint64(),
		NodeKey1:    pubKey1,
		NodeKey2:    pubKey2,
		BitcoinKey1: pubKey1,
		BitcoinKey2: pubKey2,
		AuthProof:   nil,
	}

	if err := ctx.router.AddEdge(edge); err != nil {
		t.Fatalf("unable to add edge to the channel graph: %v.", err)
	}

	edgePolicy = &channeldb.ChannelEdgePolicy{
		Signature:                 testSig,
		ChannelID:                 edge.ChannelID,
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.Flags = 0

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	edgePolicy = &channeldb.ChannelEdgePolicy{
		Signature:                 testSig,
		ChannelID:                 edge.ChannelID,
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.Flags = 1

	if err := ctx.router.UpdateEdge(edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// We should now be able to find one route to node 2.
	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	targetNode := priv2.PubKey()
	routes, err := ctx.router.FindRoutes(targetNode, paymentAmt,
		DefaultFinalCLTVDelta)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected to find 1 route, found: %v", len(routes))
	}

	// Now check that we can update the node info for the partial node
	// without messing up the channel graph.
	n1 := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		PubKey:               priv1.PubKey(),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node11",
		AuthSig:              testSig,
		Features:             testFeatures,
	}

	if err := ctx.router.AddNode(n1); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	n2 := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(123, 0),
		Addresses:            testAddrs,
		PubKey:               priv2.PubKey(),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "node22",
		AuthSig:              testSig,
		Features:             testFeatures,
	}

	if err := ctx.router.AddNode(n2); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	// Should still be able to find the route, and the info should be
	// updated.
	routes, err = ctx.router.FindRoutes(targetNode, paymentAmt,
		DefaultFinalCLTVDelta)
	if err != nil {
		t.Fatalf("unable to find any routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected to find 1 route, found: %v", len(routes))
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
