package autopilot

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
)

func TestBetweennessCentralityMetricConstruction(t *testing.T) {
	failing := []int{-1, 0}
	ok := []int{1, 10}

	for _, workers := range failing {
		m, err := NewBetweennessCentralityMetric(workers)
		if m != nil || err == nil {
			t.Fatalf("construction must fail with <= 0 workers")
		}
	}

	for _, workers := range ok {
		m, err := NewBetweennessCentralityMetric(workers)
		if m == nil || err != nil {
			t.Fatalf("construction must succeed with >= 1 workers")
		}
	}
}

// Tests that empty graph results in empty centrality result.
func TestBetweennessCentralityEmptyGraph(t *testing.T) {
	centralityMetric, err := NewBetweennessCentralityMetric(1)
	if err != nil {
		t.Fatalf("construction must succeed with positive number of workers")
	}

	for _, chanGraph := range chanGraphs {
		graph, cleanup, err := chanGraph.genFunc()
		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			if err != nil {
				t1.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			if err := centralityMetric.Refresh(graph); err != nil {
				t.Fatalf("unexpected failure during metric refresh: %v", err)
			}

			centrality := centralityMetric.GetMetric(false)
			if len(centrality) > 0 {
				t.Fatalf("expected empty metric, got: %v", len(centrality))
			}

			centrality = centralityMetric.GetMetric(true)
			if len(centrality) > 0 {
				t.Fatalf("expected empty metric, got: %v", len(centrality))
			}

		})
		if !success {
			break
		}
	}
}

// testGraphDesc is a helper type to describe a test graph.
type testGraphDesc struct {
	nodes int
	edges map[int][]int
}

// buildTestGraph builds a test graph from a passed graph desriptor.
func buildTestGraph(t *testing.T,
	graph testGraph, desc testGraphDesc) map[int]*btcec.PublicKey {

	nodes := make(map[int]*btcec.PublicKey)

	for i := 0; i < desc.nodes; i++ {
		key, err := graph.addRandNode()
		if err != nil {
			t.Fatalf("cannot create random node")
		}

		nodes[i] = key
	}

	const chanCapacity = btcutil.SatoshiPerBitcoin
	for u, neighbors := range desc.edges {
		for _, v := range neighbors {
			_, _, err := graph.addRandChannel(nodes[u], nodes[v], chanCapacity)
			if err != nil {
				t.Fatalf("unexpected error adding random channel: %v", err)
			}
		}
	}

	return nodes
}

// Test betweenness centrality calculating using an example graph.
func TestBetweennessCentralityWithNonEmptyGraph(t *testing.T) {
	graphDesc := testGraphDesc{
		nodes: 9,
		edges: map[int][]int{
			0: {1, 2, 3},
			1: {2},
			2: {3},
			3: {4, 5},
			4: {5, 6, 7},
			5: {6, 7},
			6: {7, 8},
		},
	}

	workers := []int{1, 3, 9, 100}

	results := []struct {
		normalize  bool
		centrality []float64
	}{
		{
			normalize: true,
			centrality: []float64{
				0.2, 0.0, 0.2, 1.0, 0.4, 0.4, 7.0 / 15.0, 0.0, 0.0,
			},
		},
		{
			normalize: false,
			centrality: []float64{
				3.0, 0.0, 3.0, 15.0, 6.0, 6.0, 7.0, 0.0, 0.0,
			},
		},
	}

	for _, numWorkers := range workers {
		for _, chanGraph := range chanGraphs {
			numWorkers := numWorkers
			graph, cleanup, err := chanGraph.genFunc()
			if err != nil {
				t.Fatalf("unable to create graph: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}

			testName := fmt.Sprintf("%v %d workers", chanGraph.name, numWorkers)
			success := t.Run(testName, func(t1 *testing.T) {
				centralityMetric, err := NewBetweennessCentralityMetric(
					numWorkers,
				)
				if err != nil {
					t.Fatalf("construction must succeed with " +
						"positive number of workers")
				}

				graphNodes := buildTestGraph(t1, graph, graphDesc)
				if err := centralityMetric.Refresh(graph); err != nil {
					t1.Fatalf("error while calculating betweeness centrality")
				}
				for _, expected := range results {
					expected := expected
					centrality := centralityMetric.GetMetric(expected.normalize)

					if len(centrality) != graphDesc.nodes {
						t.Fatalf("expected %v values, got: %v",
							graphDesc.nodes, len(centrality))
					}

					for node, nodeCentrality := range expected.centrality {
						nodeID := NewNodeID(graphNodes[node])
						calculatedCentrality, ok := centrality[nodeID]
						if !ok {
							t1.Fatalf("no result for node: %x (%v)",
								nodeID, node)
						}

						if nodeCentrality != calculatedCentrality {
							t1.Errorf("centrality for node: %v "+
								"should be %v, got: %v",
								node, nodeCentrality, calculatedCentrality)
						}
					}
				}
			})
			if !success {
				break
			}
		}
	}
}
