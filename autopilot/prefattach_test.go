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
)

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

// TestPrefAttachmentSelectEmptyGraph ensures that when passed an
// empty graph, the NodeSores function always returns a score of 0.
func TestPrefAttachmentSelectEmptyGraph(t *testing.T) {
	const (
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
	)

	prefAttach := NewPrefAttachment()

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

// TestPrefAttachmentSelectTwoVertexes ensures that when passed a
// graph with only two eligible vertexes, then both are given the same score,
// and the funds are appropriately allocated across each peer.
func TestPrefAttachmentSelectTwoVertexes(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
	)

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewPrefAttachment()

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
			candidates, err := prefAttach.NodeScores(graph, nil,
				maxChanSize, nodes)
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

				// Since each of the nodes has 1 channel, out
				// of only one channel in the graph, we expect
				// their score to be 0.5.
				expScore := float64(0.5)
				if candidate.Score != expScore {
					t1.Fatalf("expected candidate score "+
						"to be %v, instead was %v",
						expScore, candidate.Score)
				}
			}
		})
		if !success {
			break
		}
	}
}

// TestPrefAttachmentSelectGreedyAllocation tests that if upon
// returning node scores, the NodeScores method will attempt to greedily
// allocate all funds to each vertex (up to the max channel size).
func TestPrefAttachmentSelectGreedyAllocation(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
	)

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewPrefAttachment()

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
			scores, err := prefAttach.NodeScores(graph, nil,
				maxChanSize, nodes)
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
			}
		})
		if !success {
			break
		}
	}
}

// TestPrefAttachmentSelectSkipNodes ensures that if a node was
// already selected as a channel counterparty, then that node will get a score
// of zero during scoring.
func TestPrefAttachmentSelectSkipNodes(t *testing.T) {
	t.Parallel()

	prand.Seed(time.Now().Unix())

	const (
		maxChanSize = btcutil.Amount(btcutil.SatoshiPerBitcoin)
	)

	for _, graph := range chanGraphs {
		success := t.Run(graph.name, func(t1 *testing.T) {
			graph, cleanup, err := graph.genFunc()
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			prefAttach := NewPrefAttachment()

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
			scores, err := prefAttach.NodeScores(graph, nil,
				maxChanSize, nodes)
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
				maxChanSize, nodes)
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
