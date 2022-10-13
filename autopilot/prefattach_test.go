package autopilot

import (
	"bytes"
	prand "math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

type genGraphFunc func(t *testing.T) (testGraph, error)

type testGraph interface {
	ChannelGraph

	addRandChannel(*btcec.PublicKey, *btcec.PublicKey,
		btcutil.Amount) (*ChannelEdge, *ChannelEdge, error)

	addRandNode() (*btcec.PublicKey, error)
}

func newDiskChanGraph(t *testing.T) (testGraph, error) {
	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(t.TempDir())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		require.NoError(t, cdb.Close())
	})

	return &databaseChannelGraph{
		db: cdb.ChannelGraph(),
	}, nil
}

var _ testGraph = (*databaseChannelGraph)(nil)

func newMemChanGraph(_ *testing.T) (testGraph, error) {
	return newMemChannelGraph(), nil
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
	prefAttach := NewPrefAttachment()

	// Create a random public key, which we will query to get a score for.
	pub, err := randKey()
	require.NoError(t, err, "unable to generate key")

	nodes := map[NodeID]struct{}{
		NewNodeID(pub): {},
	}

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph
		graph, err := chanGraph.genFunc(t)
		require.NoError(t, err, "unable to create graph")

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			// With the necessary state initialized, we'll now
			// attempt to get the score for this one node.
			const walletFunds = btcutil.SatoshiPerBitcoin
			scores, err := prefAttach.NodeScores(
				graph, nil, walletFunds, nodes,
			)
			require.NoError(t1, err)

			// Since the graph is empty, we expect the score to be
			// 0, giving an empty return map.
			require.Empty(t1, scores)
		})
		if !success {
			break
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

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph
		graph, err := chanGraph.genFunc(t)
		require.NoError(t, err, "unable to create graph")

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			prefAttach := NewPrefAttachment()

			// For this set, we'll load the memory graph with two
			// nodes, and a random channel connecting them.
			const chanCapacity = btcutil.SatoshiPerBitcoin
			edge1, edge2, err := graph.addRandChannel(
				nil, nil, chanCapacity,
			)
			require.NoError(t1, err)

			// We also add a third, non-connected node to the graph.
			_, err = graph.addRandNode()
			require.NoError(t1, err)

			// Get the score for all nodes found in the graph at
			// this point.
			nodes := make(map[NodeID]struct{})
			err = graph.ForEachNode(func(n Node) error {
				nodes[n.PubKey()] = struct{}{}
				return nil
			})
			require.NoError(t1, err)

			require.Len(t1, nodes, 3)

			// With the necessary state initialized, we'll now
			// attempt to get our candidates channel score given
			// the current state of the graph.
			candidates, err := prefAttach.NodeScores(
				graph, nil, maxChanSize, nodes,
			)
			require.NoError(t1, err)

			// We expect two candidates, since one of the nodes
			// doesn't have any channels.
			require.Len(t1, candidates, 2)

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
				// their score to be 1.0.
				require.EqualValues(t1, 1, candidate.Score)
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

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph
		graph, err := chanGraph.genFunc(t)
		require.NoError(t, err, "unable to create graph")

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			prefAttach := NewPrefAttachment()

			const chanCapacity = btcutil.SatoshiPerBitcoin

			// Next, we'll add 3 nodes to the graph, creating an
			// "open triangle topology".
			edge1, _, err := graph.addRandChannel(
				nil, nil, chanCapacity,
			)
			require.NoError(t1, err)

			peerPubBytes := edge1.Peer.PubKey()
			peerPub, err := btcec.ParsePubKey(peerPubBytes[:])
			require.NoError(t1, err)

			_, _, err = graph.addRandChannel(
				peerPub, nil, chanCapacity,
			)
			require.NoError(t1, err)

			// At this point, there should be three nodes in the
			// graph, with node having two edges.
			numNodes := 0
			twoChans := false
			nodes := make(map[NodeID]struct{})
			err = graph.ForEachNode(func(n Node) error {
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
			})
			require.NoError(t1, err)

			require.EqualValues(t1, 3, numNodes)
			require.True(t1, twoChans, "have two chans")

			// We'll now begin our test, modeling the available
			// wallet balance to be 5.5 BTC. We're shooting for a
			// 50/50 allocation, and have 3 BTC in channels. As a
			// result, the heuristic should try to greedily
			// allocate funds to channels.
			scores, err := prefAttach.NodeScores(
				graph, nil, maxChanSize, nodes,
			)
			require.NoError(t1, err)

			require.Equal(t1, len(nodes), len(scores))

			// The candidates should have a non-zero score, and
			// have the max chan size funds recommended channel
			// size.
			for _, candidate := range scores {
				require.NotZero(t1, candidate.Score)
			}

			// Imagine a few channels are being opened, and there's
			// only 0.5 BTC left. That should leave us with channel
			// candidates of that size.
			const remBalance = btcutil.SatoshiPerBitcoin * 0.5
			scores, err = prefAttach.NodeScores(
				graph, nil, remBalance, nodes,
			)
			require.NoError(t1, err)

			require.Equal(t1, len(nodes), len(scores))

			// Check that the recommended channel sizes are now the
			// remaining channel balance.
			for _, candidate := range scores {
				require.NotZero(t1, candidate.Score)
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

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph
		graph, err := chanGraph.genFunc(t)
		require.NoError(t, err, "unable to create graph")

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			prefAttach := NewPrefAttachment()

			// Next, we'll create a simple topology of two nodes,
			// with a single channel connecting them.
			const chanCapacity = btcutil.SatoshiPerBitcoin
			_, _, err = graph.addRandChannel(nil, nil, chanCapacity)
			require.NoError(t1, err)

			nodes := make(map[NodeID]struct{})
			err = graph.ForEachNode(func(n Node) error {
				nodes[n.PubKey()] = struct{}{}
				return nil
			})
			require.NoError(t1, err)

			require.Len(t1, nodes, 2)

			// With our graph created, we'll now get the scores for
			// all nodes in the graph.
			scores, err := prefAttach.NodeScores(
				graph, nil, maxChanSize, nodes,
			)
			require.NoError(t1, err)

			require.Equal(t1, len(nodes), len(scores))

			// THey should all have a score, and a maxChanSize
			// channel size recommendation.
			for _, candidate := range scores {
				require.NotZero(t1, candidate.Score)
			}

			// We'll simulate a channel update by adding the nodes
			// to our set of channels.
			var chans []LocalChannel
			for _, candidate := range scores {
				chans = append(chans,
					LocalChannel{
						Node: candidate.NodeID,
					},
				)
			}

			// If we attempt to make a call to the NodeScores
			// function, without providing any new information,
			// then all nodes should have a score of zero, since we
			// already got channels to them.
			scores, err = prefAttach.NodeScores(
				graph, chans, maxChanSize, nodes,
			)
			require.NoError(t1, err)

			// Since all should be given a score of 0, the map
			// should be empty.
			require.Empty(t1, scores)
		})
		if !success {
			break
		}
	}
}
