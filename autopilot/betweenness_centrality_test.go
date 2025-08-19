package autopilot

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBetweennessCentralityMetricConstruction(t *testing.T) {
	failing := []int{-1, 0}
	ok := []int{1, 10}

	for _, workers := range failing {
		m, err := NewBetweennessCentralityMetric(workers)
		require.Error(
			t, err, "construction must fail with <= 0 workers",
		)
		require.Nil(t, m)
	}

	for _, workers := range ok {
		m, err := NewBetweennessCentralityMetric(workers)
		require.NoError(
			t, err, "construction must succeed with >= 1 workers",
		)
		require.NotNil(t, m)
	}
}

// Tests that empty graph results in empty centrality result.
func TestBetweennessCentralityEmptyGraph(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	centralityMetric, err := NewBetweennessCentralityMetric(1)
	require.NoError(
		t, err,
		"construction must succeed with positive number of workers",
	)

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph
		graph, err := chanGraph.genFunc(t)
		require.NoError(t, err, "unable to create graph")

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			err = centralityMetric.Refresh(ctx, graph)
			require.NoError(t1, err)

			centrality := centralityMetric.GetMetric(false)
			require.Equal(t1, 0, len(centrality))

			centrality = centralityMetric.GetMetric(true)
			require.Equal(t1, 0, len(centrality))
		})
		if !success {
			break
		}
	}
}

// Test betweenness centrality calculating using an example graph.
func TestBetweennessCentralityWithNonEmptyGraph(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	workers := []int{1, 3, 9, 100}

	tests := []struct {
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
			chanGraph := chanGraph
			numWorkers := numWorkers
			graph, err := chanGraph.genFunc(t)
			require.NoError(t, err, "unable to create graph")

			testName := fmt.Sprintf(
				"%v %d workers", chanGraph.name, numWorkers,
			)

			success := t.Run(testName, func(t1 *testing.T) {
				metric, err := NewBetweennessCentralityMetric(
					numWorkers,
				)
				require.NoError(
					t1, err,
					"construction must succeed with "+
						"positive number of workers",
				)

				graphNodes := buildTestGraph(
					t1, graph, centralityTestGraph,
				)

				err = metric.Refresh(ctx, graph)
				require.NoError(t1, err)

				for _, expected := range tests {
					expected := expected
					centrality := metric.GetMetric(
						expected.normalize,
					)

					require.Equal(
						t1, centralityTestGraph.nodes,
						len(centrality),
					)

					for i, c := range expected.centrality {
						nodeID := NewNodeID(
							graphNodes[i],
						)
						result, ok := centrality[nodeID]
						require.True(t1, ok)
						require.Equal(t1, c, result)
					}
				}
			})
			if !success {
				break
			}
		}
	}
}
