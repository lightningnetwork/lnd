package routing

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

type testCtx struct {
	router *ChannelRouter

	graph *channeldb.ChannelGraph

	aliases map[string]*btcec.PublicKey

	chain *mockChain

	notifier *mockNotifier
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

		sourceNode, err = createGraphNode()
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
	notifier := newMockNotifier()
	router, err := New(Config{
		Graph:    graph,
		Chain:    chain,
		Notifier: notifier,
		Broadcast: func(_ *btcec.PublicKey, msg ...lnwire.Message) error {
			return nil
		},
		SendMessages: func(_ *btcec.PublicKey, msg ...lnwire.Message) error {
			return nil
		},
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
		router:   router,
		graph:    graph,
		aliases:  aliasMap,
		chain:    chain,
		notifier: notifier,
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
		t.Fatalf("2 routes shouldn't been selected, instead %v were: ",
			len(routes))
	}

	// The paths should properly be ranked according to their total fee
	// rate.
	if routes[0].TotalFees > routes[1].TotalFees {
		t.Fatalf("routes not ranked by total fee: %v",
			spew.Sdump(routes))
	}
}

