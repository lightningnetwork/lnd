package autopilot

import (
	"fmt"
	"testing"
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

// Test betweenness centrality calculating using an example graph.
func TestBetweennessCentralityWithNonEmptyGraph(t *testing.T) {
	workers := []int{1, 3, 9, 100}

	results := []struct {
		normalize  bool
		centrality []float64
	}{
		{
			normalize:  true,
			centrality: normalizedTestGraphCentrality,
		},
		{
			normalize:  false,
			centrality: testGraphCentrality,
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

				graphNodes := buildTestGraph(t1, graph, centralityTestGraph)
				if err := centralityMetric.Refresh(graph); err != nil {
					t1.Fatalf("error while calculating betweeness centrality")
				}
				for _, expected := range results {
					expected := expected
					centrality := centralityMetric.GetMetric(expected.normalize)

					if len(centrality) != centralityTestGraph.nodes {
						t.Fatalf("expected %v values, got: %v",
							centralityTestGraph.nodes, len(centrality))
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
