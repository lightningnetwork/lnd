package routing

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/wire"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
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

		sourceNode, err = graph.SourceNode()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to fetch source node: %v", err)
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
			_ *lnwire.UpdateAddHTLC) ([32]byte, error) {
			return [32]byte{}, nil
		},
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
	const paymentAmt = btcutil.Amount(100)
	target := ctx.aliases["luoji"]
	routes, err := ctx.router.FindRoutes(target, paymentAmt)
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
		Amount:      btcutil.Amount(1000),
		PaymentHash: payHash,
	}

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{9}, 32))

	// We'll modify the SendToSwitch method that's been set within the
	// router's configuration to ignore the path that has luo ji as the
	// first hop. This should force the router to instead take the
	// available two hop path (through satoshi).
	ctx.router.cfg.SendToSwitch = func(n *btcec.PublicKey,
		_ *lnwire.UpdateAddHTLC) ([32]byte, error) {

		if ctx.aliases["luoji"].IsEqual(n) {
			return [32]byte{}, errors.New("send error")
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

// TestAddProof checks that we can update the channel proof after channel
// info was added to the database.
func TestAddProof(t *testing.T) {
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
	if err := ctx.router.AddNode(node1); err != nil {
		t.Fatal(err)
	}
	node2, err := createTestNode()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.router.AddNode(node2); err != nil {
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

// TestAddEdgeUnknownVertexes tests that if an edge is added that contains two
// vertex which we don't know of, then the edge is rejected.
func TestAddEdgeUnknownVertexes(t *testing.T) {
	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	_, _, chanID, err := createChannelEdge(ctx,
		bitcoinKey1.SerializeCompressed(), bitcoinKey2.SerializeCompressed(),
		10000, 500)
	if err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	edge := &channeldb.ChannelEdgeInfo{
		ChannelID:   chanID.ToUint64(),
		NodeKey1:    priv1.PubKey(),
		NodeKey2:    priv1.PubKey(),
		BitcoinKey1: bitcoinKey1,
		BitcoinKey2: bitcoinKey2,
		AuthProof:   nil,
	}
	if err := ctx.router.AddEdge(edge); err == nil {
		t.Fatal("edge should have been rejected due to unknown " +
			"vertexes, but wasn't")
	}
}
