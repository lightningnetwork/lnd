package autopilot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	testShortestPathLengths = map[int]uint32{
		0: 0,
		1: 1,
		2: 1,
		3: 1,
		4: 2,
		5: 2,
		6: 3,
		7: 3,
		8: 4,
	}
	testNodeEccentricities = map[int]uint32{
		0: 4,
		1: 5,
		2: 4,
		3: 3,
		4: 3,
		5: 3,
		6: 4,
		7: 4,
		8: 5,
	}
)

// NewTestSimpleGraph is a helper that generates a SimpleGraph from a test
// graph description.
// Assumes that the graph description is internally consistent, i.e. edges are
// not repeatedly defined.
func NewTestSimpleGraph(graph testGraphDesc) SimpleGraph {
	// We convert the test graph description into an adjacency list.
	adjList := make([][]int, graph.nodes)
	for node, neighbors := range graph.edges {
		for _, neighbor := range neighbors {
			adjList[node] = append(adjList[node], neighbor)
			adjList[neighbor] = append(adjList[neighbor], node)
		}
	}

	return SimpleGraph{Adj: adjList}
}

func TestShortestPathLengths(t *testing.T) {
	simpleGraph := NewTestSimpleGraph(centralityTestGraph)

	// Test the shortest path lengths from node 0 to all other nodes.
	shortestPathLengths := simpleGraph.shortestPathLengths(0)
	require.Equal(t, shortestPathLengths, testShortestPathLengths)
}

func TestEccentricities(t *testing.T) {
	simpleGraph := NewTestSimpleGraph(centralityTestGraph)

	// Test the node eccentricities for all nodes.
	nodes := make([]int, len(simpleGraph.Adj))
	for a := range nodes {
		nodes[a] = a
	}
	nodeEccentricities := simpleGraph.nodeEccentricities(nodes)
	require.Equal(t, nodeEccentricities, testNodeEccentricities)
}

func TestDiameterExact(t *testing.T) {
	simpleGraph := NewTestSimpleGraph(centralityTestGraph)

	// Test the diameter in a brute-force manner.
	diameter := simpleGraph.Diameter()
	require.Equal(t, uint32(5), diameter)
}

func TestDiameterCutoff(t *testing.T) {
	simpleGraph := NewTestSimpleGraph(centralityTestGraph)

	// Test the diameter by cutting out the inside of the graph.
	diameter := simpleGraph.DiameterRadialCutoff()
	require.Equal(t, uint32(5), diameter)
}

func BenchmarkShortestPathOpt(b *testing.B) {
	// TODO: a method that generates a huge graph is needed
	simpleGraph := NewTestSimpleGraph(centralityTestGraph)

	for n := 0; n < b.N; n++ {
		_ = simpleGraph.shortestPathLengths(0)
	}
}
