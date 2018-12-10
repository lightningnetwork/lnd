package autopilot

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"
	"time"

	prand "math/rand"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestConstrainedPrefAttachmentNeedMoreChan(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)

		chanLimit = 3

		threshold = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}

	randChanID := func() lnwire.ShortChannelID {
		return lnwire.NewShortChanIDFromInt(uint64(prand.Int63()))
	}

	testCases := []struct {
		channels  []Channel
		walletAmt btcutil.Amount

		needMore     bool
		amtAvailable btcutil.Amount
		numMore      uint32
	}{
		// Many available funds, but already have too many active open
		// channels.
		{
			[]Channel{
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(prand.Int31()),
				},
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(prand.Int31()),
				},
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(prand.Int31()),
				},
			},
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 10),
			false,
			0,
			0,
		},

		// Ratio of funds in channels and total funds meets the
		// threshold.
		{
			[]Channel{
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
			},
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 2),
			false,
			0,
			0,
		},

		// Ratio of funds in channels and total funds is below the
		// threshold. We have 10 BTC allocated amongst channels and
		// funds, atm. We're targeting 50%, so 5 BTC should be
		// allocated. Only 1 BTC is atm, so 4 BTC should be
		// recommended. We should also request 2 more channels as the
		// limit is 3.
		{
			[]Channel{
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
			},
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 9),
			true,
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 4),
			2,
		},

		// Ratio of funds in channels and total funds is below the
		// threshold. We have 14 BTC total amongst the wallet's
		// balance, and our currently opened channels. Since we're
		// targeting a 50% allocation, we should commit 7 BTC. The
		// current channels commit 4 BTC, so we should expected 3 BTC
		// to be committed. We should only request a single additional
		// channel as the limit is 3.
		{
			[]Channel{
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin * 3),
				},
			},
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 10),
			true,
			btcutil.Amount(btcutil.SatoshiPerBitcoin * 3),
			1,
		},

		// Ratio of funds in channels and total funds is above the
		// threshold.
		{
			[]Channel{
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
				{
					ChanID:   randChanID(),
					Capacity: btcutil.Amount(btcutil.SatoshiPerBitcoin),
				},
			},
			btcutil.Amount(btcutil.SatoshiPerBitcoin),
			false,
			0,
			0,
		},
	}

	prefAttach := NewConstrainedPrefAttachment(constraints)

	for i, testCase := range testCases {
		amtToAllocate, numMore, needMore := prefAttach.NeedMoreChans(
			testCase.channels, testCase.walletAmt,
		)

		if amtToAllocate != testCase.amtAvailable {
			t.Fatalf("test #%v: expected %v, got %v",
				i, testCase.amtAvailable, amtToAllocate)
		}
		if needMore != testCase.needMore {
			t.Fatalf("test #%v: expected %v, got %v",
				i, testCase.needMore, needMore)
		}
		if numMore != testCase.numMore {
			t.Fatalf("test #%v: expected %v, got %v",
				i, testCase.numMore, numMore)
		}
	}
}

type genGraphFunc func() (testGraph, func(), error)

type testGraph interface {
	ChannelGraph

	addRandChannel(*btcec.PublicKey, *btcec.PublicKey,
		btcutil.Amount) (*ChannelEdge, *ChannelEdge, error)

	addRandNode() (*btcec.PublicKey, error)
}

func newDiskChanGraph() (testGraph, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return &databaseChannelGraph{
		db: cdb.ChannelGraph(),
	}, cleanUp, nil
}

var _ testGraph = (*databaseChannelGraph)(nil)

func newMemChanGraph() (testGraph, func(), error) {
	return newMemChannelGraph(), nil, nil
}

var _ testGraph = (*memChannelGraph)(nil)

var chanGraphs = []struct {
	name    string
	genFunc genGraphFunc
}{
	{
		name:    "disk_graph",
		genFunc: newDiskChanGraph,
	},
	{
		name:    "mem_graph",
		genFunc: newMemChanGraph,
	},
}

// TestConstrainedPrefAttachmentSelectEmptyGraph ensures that when passed an
// empty graph, the NodeSores function always returns a score of 0.
func TestConstrainedPrefAttachmentSelectEmptyGraph(t *testing.T) {
	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
		chanLimit   = 3
		threshold   = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}

	prefAttach := NewConstrainedPrefAttachment(constraints)

	// Create a random public key, which we will query to get a score for.
	pub, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}

	nodes := map[NodeID]struct{}{
		NewNodeID(pub): {},
	}

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			// With the necessary state initialized, we'll now
			// attempt to get the score for this one node.
			const walletFunds = btcutil.SatoshiPerBitcoin
			scores, err := prefAttach.NodeScores(graph, nil,
				walletFunds, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			// Since the graph is empty, we expect the score to be
			// 0, giving an empty return map.
			if len(scores) != 0 {
				t1.Fatalf("expected empty score map, "+
					"instead got %v ", len(scores))
			}
		})
		if !success {
			break
		}
	}
}

// completeGraph is a helper method that adds numNodes fully connected nodes to
// the graph.
func completeGraph(t *testing.T, g testGraph, numNodes int) {
	const chanCapacity = btcutil.SatoshiPerBitcoin
	nodes := make(map[int]*btcec.PublicKey)
	for i := 0; i < numNodes; i++ {
		for j := i + 1; j < numNodes; j++ {

			node1 := nodes[i]
			node2 := nodes[j]
			edge1, edge2, err := g.addRandChannel(
				node1, node2, chanCapacity)
			if err != nil {
				t.Fatalf("unable to generate channel: %v", err)
			}

			if node1 == nil {
				pubKeyBytes := edge1.Peer.PubKey()
				nodes[i], err = btcec.ParsePubKey(
					pubKeyBytes[:], btcec.S256(),
				)
				if err != nil {
					t.Fatalf("unable to parse pubkey: %v",
						err)
				}
			}

			if node2 == nil {
				pubKeyBytes := edge2.Peer.PubKey()
				nodes[j], err = btcec.ParsePubKey(
					pubKeyBytes[:], btcec.S256(),
				)
				if err != nil {
					t.Fatalf("unable to parse pubkey: %v",
						err)
				}
			}
		}
	}
}

// TestConstrainedPrefAttachmentSelectTwoVertexes ensures that when passed a
// graph with only two eligible vertexes, then both are given the same score,
// and the funds are appropriately allocated across each peer.
func TestConstrainedPrefAttachmentSelectTwoVertexes(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
		chanLimit   = 3
		threshold   = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}
	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewConstrainedPrefAttachment(constraints)

			// For this set, we'll load the memory graph with two
			// nodes, and a random channel connecting them.
			const chanCapacity = btcutil.SatoshiPerBitcoin
			edge1, edge2, err := graph.addRandChannel(nil, nil, chanCapacity)
			if err != nil {
				t1.Fatalf("unable to generate channel: %v", err)
			}

			// We also add a third, non-connected node to the graph.
			_, err = graph.addRandNode()
			if err != nil {
				t1.Fatalf("unable to add random node: %v", err)
			}

			// Get the score for all nodes found in the graph at
			// this point.
			nodes := make(map[NodeID]struct{})
			if err := graph.ForEachNode(func(n Node) error {
				nodes[n.PubKey()] = struct{}{}
				return nil
			}); err != nil {
				t1.Fatalf("unable to traverse graph: %v", err)
			}

			if len(nodes) != 3 {
				t1.Fatalf("expected 2 nodes, found %d", len(nodes))
			}

			// With the necessary state initialized, we'll now
			// attempt to get our candidates channel score given
			// the current state of the graph.
			const walletFunds = btcutil.SatoshiPerBitcoin * 10
			candidates, err := prefAttach.NodeScores(graph, nil,
				walletFunds, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			// We expect two candidates, since one of the nodes
			// doesn't have any channels.
			if len(candidates) != 2 {
				t1.Fatalf("2 nodes should be scored, "+
					"instead %v were", len(candidates))
			}

			// The candidates should be amongst the two edges
			// created above.
			for nodeID, candidate := range candidates {
				edge1Pub := edge1.Peer.PubKey()
				edge2Pub := edge2.Peer.PubKey()

				switch {
				case bytes.Equal(nodeID[:], edge1Pub[:]):
				case bytes.Equal(nodeID[:], edge2Pub[:]):
				default:
					t1.Fatalf("attached to unknown node: %x",
						nodeID[:])
				}

				// As the number of funds available exceed the
				// max channel size, both edges should consume
				// the maximum channel size.
				if candidate.ChanAmt != maxChanSize {
					t1.Fatalf("max channel size should be "+
						"allocated, instead %v was: ",
						maxChanSize)
				}

				// Since each of the nodes has 1 channel, out
				// of only one channel in the graph, we expect
				// their score to be 0.5.
				expScore := float64(0.5)
				if candidate.Score != expScore {
					t1.Fatalf("expected candidate score "+
						"to be %v, instead was %v",
						expScore, candidate.Score)
				}

				if len(candidate.Addrs) == 0 {
					t1.Fatalf("expected node to have " +
						"available addresses, didn't")
				}
			}
		})
		if !success {
			break
		}
	}
}

// TestConstrainedPrefAttachmentSelectInsufficientFunds ensures that if the
// balance of the backing wallet is below the set min channel size, then it
// never recommends candidates to attach to.
func TestConstrainedPrefAttachmentSelectInsufficientFunds(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
		chanLimit   = 3
		threshold   = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			// Add 10 nodes to the graph, with channels between
			// them.
			completeGraph(t, graph, 10)

			prefAttach := NewConstrainedPrefAttachment(constraints)

			nodes := make(map[NodeID]struct{})
			if err := graph.ForEachNode(func(n Node) error {
				nodes[n.PubKey()] = struct{}{}
				return nil
			}); err != nil {
				t1.Fatalf("unable to traverse graph: %v", err)
			}

			// With the necessary state initialized, we'll now
			// attempt to get the score for our list of nodes,
			// passing zero for the amount of wallet funds. This
			// should return an all-zero score set.
			scores, err := prefAttach.NodeScores(graph, nil,
				0, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			// Since all should be given a score of 0, the map
			// should be empty.
			if len(scores) != 0 {
				t1.Fatalf("expected empty score map, "+
					"instead got %v ", len(scores))
			}
		})
		if !success {
			break
		}
	}
}

// TestConstrainedPrefAttachmentSelectGreedyAllocation tests that if upon
// returning node scores, the NodeScores method will attempt to greedily
// allocate all funds to each vertex (up to the max channel size).
func TestConstrainedPrefAttachmentSelectGreedyAllocation(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
		chanLimit   = 3
		threshold   = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewConstrainedPrefAttachment(constraints)

			const chanCapacity = btcutil.SatoshiPerBitcoin

			// Next, we'll add 3 nodes to the graph, creating an
			// "open triangle topology".
			edge1, _, err := graph.addRandChannel(nil, nil,
				chanCapacity)
			if err != nil {
				t1.Fatalf("unable to create channel: %v", err)
			}
			peerPubBytes := edge1.Peer.PubKey()
			peerPub, err := btcec.ParsePubKey(
				peerPubBytes[:], btcec.S256(),
			)
			if err != nil {
				t.Fatalf("unable to parse pubkey: %v", err)
			}
			_, _, err = graph.addRandChannel(
				peerPub, nil, chanCapacity,
			)
			if err != nil {
				t1.Fatalf("unable to create channel: %v", err)
			}

			// At this point, there should be three nodes in the
			// graph, with node node having two edges.
			numNodes := 0
			twoChans := false
			nodes := make(map[NodeID]struct{})
			if err := graph.ForEachNode(func(n Node) error {
				numNodes++
				nodes[n.PubKey()] = struct{}{}
				numChans := 0
				err := n.ForEachChannel(func(c ChannelEdge) error {
					numChans++
					return nil
				})
				if err != nil {
					return err
				}

				twoChans = twoChans || (numChans == 2)

				return nil
			}); err != nil {
				t1.Fatalf("unable to traverse graph: %v", err)
			}
			if numNodes != 3 {
				t1.Fatalf("expected 3 nodes, instead have: %v",
					numNodes)
			}
			if !twoChans {
				t1.Fatalf("expected node to have two channels")
			}

			// We'll now begin our test, modeling the available
			// wallet balance to be 5.5 BTC. We're shooting for a
			// 50/50 allocation, and have 3 BTC in channels. As a
			// result, the heuristic should try to greedily
			// allocate funds to channels.
			const availableBalance = btcutil.SatoshiPerBitcoin * 2.5
			scores, err := prefAttach.NodeScores(graph, nil,
				availableBalance, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			if len(scores) != len(nodes) {
				t1.Fatalf("all nodes should be scored, "+
					"instead %v were", len(scores))
			}

			// The candidates should have a non-zero score, and
			// have the max chan size funds recommended channel
			// size.
			for _, candidate := range scores {
				if candidate.Score == 0 {
					t1.Fatalf("Expected non-zero score")
				}

				if candidate.ChanAmt != maxChanSize {
					t1.Fatalf("expected recommendation "+
						"of %v, instead got %v",
						maxChanSize, candidate.ChanAmt)
				}

				if len(candidate.Addrs) == 0 {
					t1.Fatalf("expected node to have " +
						"available addresses, didn't")
				}
			}

			// Imagine a few channels are being opened, and there's
			// only 0.5 BTC left. That should leave us with channel
			// candidates of that size.
			const remBalance = btcutil.SatoshiPerBitcoin * 0.5
			scores, err = prefAttach.NodeScores(graph, nil,
				remBalance, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			if len(scores) != len(nodes) {
				t1.Fatalf("all nodes should be scored, "+
					"instead %v were", len(scores))
			}

			// Check that the recommended channel sizes are now the
			// remaining channel balance.
			for _, candidate := range scores {
				if candidate.Score == 0 {
					t1.Fatalf("Expected non-zero score")
				}

				if candidate.ChanAmt != remBalance {
					t1.Fatalf("expected recommendation "+
						"of %v, instead got %v",
						remBalance, candidate.ChanAmt)
				}

				if len(candidate.Addrs) == 0 {
					t1.Fatalf("expected node to have " +
						"available addresses, didn't")
				}
			}
		})
		if !success {
			break
		}
	}
}

// TestConstrainedPrefAttachmentSelectSkipNodes ensures that if a node was
// already selected as a channel counterparty, then that node will get a score
// of zero during scoring.
func TestConstrainedPrefAttachmentSelectSkipNodes(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		minChanSize = 0
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
		chanLimit   = 3
		threshold   = 0.5
	)

	constraints := &HeuristicConstraints{
		MinChanSize: minChanSize,
		MaxChanSize: maxChanSize,
		ChanLimit:   chanLimit,
		Allocation:  threshold,
	}

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewConstrainedPrefAttachment(constraints)

			// Next, we'll create a simple topology of two nodes,
			// with a single channel connecting them.
			const chanCapacity = btcutil.SatoshiPerBitcoin
			_, _, err = graph.addRandChannel(nil, nil,
				chanCapacity)
			if err != nil {
				t1.Fatalf("unable to create channel: %v", err)
			}

			nodes := make(map[NodeID]struct{})
			if err := graph.ForEachNode(func(n Node) error {
				nodes[n.PubKey()] = struct{}{}
				return nil
			}); err != nil {
				t1.Fatalf("unable to traverse graph: %v", err)
			}

			if len(nodes) != 2 {
				t1.Fatalf("expected 2 nodes, found %d", len(nodes))
			}

			// With our graph created, we'll now get the scores for
			// all nodes in the graph.
			const availableBalance = btcutil.SatoshiPerBitcoin * 2.5
			scores, err := prefAttach.NodeScores(graph, nil,
				availableBalance, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			if len(scores) != len(nodes) {
				t1.Fatalf("all nodes should be scored, "+
					"instead %v were", len(scores))
			}

			// THey should all have a score, and a maxChanSize
			// channel size recommendation.
			for _, candidate := range scores {
				if candidate.Score == 0 {
					t1.Fatalf("Expected non-zero score")
				}

				if candidate.ChanAmt != maxChanSize {
					t1.Fatalf("expected recommendation "+
						"of %v, instead got %v",
						maxChanSize, candidate.ChanAmt)
				}

				if len(candidate.Addrs) == 0 {
					t1.Fatalf("expected node to have " +
						"available addresses, didn't")
				}
			}

			// We'll simulate a channel update by adding the nodes
			// to our set of channels.
			var chans []Channel
			for _, candidate := range scores {
				chans = append(chans,
					Channel{
						Node: candidate.NodeID,
					},
				)
			}

			// If we attempt to make a call to the NodeScores
			// function, without providing any new information,
			// then all nodes should have a score of zero, since we
			// already got channels to them.
			scores, err = prefAttach.NodeScores(graph, chans,
				availableBalance, nodes)
			if err != nil {
				t1.Fatalf("unable to select attachment "+
					"directives: %v", err)
			}

			// Since all should be given a score of 0, the map
			// should be empty.
			if len(scores) != 0 {
				t1.Fatalf("expected empty score map, "+
					"instead got %v ", len(scores))
			}
		})
		if !success {
			break
		}
	}
}
