package onionmessage

import (
	"context"
	"testing"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// mockNodeTraverser implements graphdb.NodeTraverser for testing the BFS
// pathfinding algorithm.
type mockNodeTraverser struct {
	// edges maps each node to its list of channel neighbors.
	edges map[route.Vertex][]route.Vertex

	// features maps each node to its advertised feature vector.
	features map[route.Vertex]*lnwire.FeatureVector
}

// newMockNodeTraverser creates a new mockNodeTraverser with initialized maps.
func newMockNodeTraverser() *mockNodeTraverser {
	return &mockNodeTraverser{
		edges:    make(map[route.Vertex][]route.Vertex),
		features: make(map[route.Vertex]*lnwire.FeatureVector),
	}
}

// addNode adds a node with the given features to the mock graph.
func (m *mockNodeTraverser) addNode(v route.Vertex,
	features *lnwire.FeatureVector) {

	m.features[v] = features
}

// addEdge adds a bidirectional edge between two nodes.
func (m *mockNodeTraverser) addEdge(a, b route.Vertex) {
	m.edges[a] = append(m.edges[a], b)
	m.edges[b] = append(m.edges[b], a)
}

// ForEachNodeDirectedChannel calls the callback for every channel neighbor of
// the given node.
func (m *mockNodeTraverser) ForEachNodeDirectedChannel(
	_ context.Context, nodePub route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error,
	reset func()) error {

	neighbors, ok := m.edges[nodePub]
	if !ok {
		return nil
	}

	for _, neighbor := range neighbors {
		err := cb(&graphdb.DirectedChannel{
			OtherNode: neighbor,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// FetchNodeFeatures returns the features of the given node. Returns an
// EmptyFeatureVector (nil error) when the node is absent, matching the
// production DB behaviour of graphdb.nodeTraverserSession.
func (m *mockNodeTraverser) FetchNodeFeatures(
	_ context.Context,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	features, ok := m.features[nodePub]
	if !ok {
		return lnwire.EmptyFeatureVector(), nil
	}

	return features, nil
}

// vertexFromByte creates a test Vertex from a single byte for readability.
func vertexFromByte(b byte) route.Vertex {
	var v route.Vertex
	v[0] = b

	return v
}

// onionFeatures returns a feature vector with the OnionMessagesOptional bit
// set.
func onionFeatures() *lnwire.FeatureVector {
	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.OnionMessagesOptional),
		nil,
	)
}

// noOnionFeatures returns a feature vector that has some bits set (e.g.
// data-loss-protect) but NOT onion message support. This simulates a node
// that is present in the graph but does not advertise onion messages,
// as distinct from a node that is absent from the graph entirely (which
// returns a zero-bit EmptyFeatureVector).
func noOnionFeatures() *lnwire.FeatureVector {
	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.DataLossProtectOptional),
		nil,
	)
}

// TestFindPathDirectNeighbor tests pathfinding when destination is a direct
// neighbor.
func TestFindPathDirectNeighbor(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	dest := vertexFromByte(2)

	graph.addNode(source, onionFeatures())
	graph.addNode(dest, onionFeatures())
	graph.addEdge(source, dest)

	path, err := FindPath(t.Context(), graph, source, dest, 20)
	require.NoError(t, err)
	require.Len(t, path, 1)
	require.Equal(t, dest, path[0])
}

// TestFindPathMultiHop tests pathfinding across multiple hops.
func TestFindPathMultiHop(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	hop1 := vertexFromByte(2)
	hop2 := vertexFromByte(3)
	dest := vertexFromByte(4)

	graph.addNode(source, onionFeatures())
	graph.addNode(hop1, onionFeatures())
	graph.addNode(hop2, onionFeatures())
	graph.addNode(dest, onionFeatures())

	graph.addEdge(source, hop1)
	graph.addEdge(hop1, hop2)
	graph.addEdge(hop2, dest)

	path, err := FindPath(t.Context(), graph, source, dest, 20)
	require.NoError(t, err)
	require.Len(t, path, 3)
	require.Equal(t, hop1, path[0])
	require.Equal(t, hop2, path[1])
	require.Equal(t, dest, path[2])
}

// TestFindPathFeatureFiltering tests that nodes without onion message support
// are skipped, finding a longer path through supporting nodes.
func TestFindPathFeatureFiltering(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	noOnion := vertexFromByte(2)
	withOnion := vertexFromByte(3)
	withOnion2 := vertexFromByte(4)
	dest := vertexFromByte(5)

	graph.addNode(source, onionFeatures())
	graph.addNode(noOnion, noOnionFeatures())
	graph.addNode(withOnion, onionFeatures())
	graph.addNode(withOnion2, onionFeatures())
	graph.addNode(dest, onionFeatures())

	// Direct path through noOnion (shorter).
	graph.addEdge(source, noOnion)
	graph.addEdge(noOnion, dest)

	// Alternate path through withOnion (longer).
	graph.addEdge(source, withOnion)
	graph.addEdge(withOnion, withOnion2)
	graph.addEdge(withOnion2, dest)

	path, err := FindPath(t.Context(), graph, source, dest, 20)
	require.NoError(t, err)
	require.Len(t, path, 3)
	require.Equal(t, withOnion, path[0])
	require.Equal(t, withOnion2, path[1])
	require.Equal(t, dest, path[2])
}

// TestFindPathNoPathExists tests that ErrNoPathFound is returned when the
// graph is disconnected.
func TestFindPathNoPathExists(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	dest := vertexFromByte(2)

	graph.addNode(source, onionFeatures())
	graph.addNode(dest, onionFeatures())

	// No edges between source and dest.
	_, err := FindPath(t.Context(), graph, source, dest, 20)
	require.ErrorIs(t, err, ErrNoPathFound)
}

// TestFindPathDestinationNotInGraph tests that ErrNodeNotFound is returned when
// the destination has no entry in the graph (empty feature vector).
func TestFindPathDestinationNotInGraph(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	dest := vertexFromByte(2)

	// dest not added to graph; FetchNodeFeatures returns
	// EmptyFeatureVector.
	graph.addNode(source, onionFeatures())

	_, err := FindPath(t.Context(), graph, source, dest, 20)
	require.ErrorIs(t, err, ErrNodeNotFound)
}

// TestFindPathDestinationNoOnionSupport tests that
// ErrDestinationNoOnionSupport is returned when the destination doesn't
// support onion messages.
func TestFindPathDestinationNoOnionSupport(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	dest := vertexFromByte(2)

	graph.addNode(source, onionFeatures())
	graph.addNode(dest, noOnionFeatures())
	graph.addEdge(source, dest)

	_, err := FindPath(t.Context(), graph, source, dest, 20)
	require.ErrorIs(t, err, ErrDestinationNoOnionSupport)
}

// TestFindPathMaxHopsExceeded tests that ErrNoPathFound is returned when the
// path exceeds the maximum number of hops.
func TestFindPathMaxHopsExceeded(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	hop1 := vertexFromByte(2)
	hop2 := vertexFromByte(3)
	dest := vertexFromByte(4)

	graph.addNode(source, onionFeatures())
	graph.addNode(hop1, onionFeatures())
	graph.addNode(hop2, onionFeatures())
	graph.addNode(dest, onionFeatures())

	graph.addEdge(source, hop1)
	graph.addEdge(hop1, hop2)
	graph.addEdge(hop2, dest)

	// Path requires 3 hops, but maxHops is 2.
	_, err := FindPath(t.Context(), graph, source, dest, 2)
	require.ErrorIs(t, err, ErrNoPathFound)
}

// TestFindPathWithCycles tests that BFS correctly handles cycles in the graph.
func TestFindPathWithCycles(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	a := vertexFromByte(2)
	b := vertexFromByte(3)
	c := vertexFromByte(4)
	dest := vertexFromByte(5)

	graph.addNode(source, onionFeatures())
	graph.addNode(a, onionFeatures())
	graph.addNode(b, onionFeatures())
	graph.addNode(c, onionFeatures())
	graph.addNode(dest, onionFeatures())

	// Create a cycle: source -> a -> b -> c -> a
	graph.addEdge(source, a)
	graph.addEdge(a, b)
	graph.addEdge(b, c)
	graph.addEdge(c, a)

	// Path to dest through b.
	graph.addEdge(b, dest)

	path, err := FindPath(t.Context(), graph, source, dest, 20)
	require.NoError(t, err)
	require.Len(t, path, 3)
	require.Equal(t, a, path[0])
	require.Equal(t, b, path[1])
	require.Equal(t, dest, path[2])
}

// TestFindPathShortestPath tests that BFS finds the shortest path when
// multiple paths of different lengths exist.
func TestFindPathShortestPath(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	source := vertexFromByte(1)
	a := vertexFromByte(2)
	b := vertexFromByte(3)
	c := vertexFromByte(4)
	dest := vertexFromByte(5)

	graph.addNode(source, onionFeatures())
	graph.addNode(a, onionFeatures())
	graph.addNode(b, onionFeatures())
	graph.addNode(c, onionFeatures())
	graph.addNode(dest, onionFeatures())

	// Long path: source -> a -> b -> c -> dest (4 hops).
	graph.addEdge(source, a)
	graph.addEdge(a, b)
	graph.addEdge(b, c)
	graph.addEdge(c, dest)

	// Short path: source -> b -> dest (2 hops).
	graph.addEdge(source, b)
	graph.addEdge(b, dest)

	path, err := FindPath(t.Context(), graph, source, dest, 20)
	require.NoError(t, err)
	require.Len(t, path, 2)
	require.Equal(t, b, path[0])
	require.Equal(t, dest, path[1])
}

// TestFindPathSameSourceAndDest tests that finding a path from a node to
// itself returns an empty path.
func TestFindPathSameSourceAndDest(t *testing.T) {
	t.Parallel()

	graph := newMockNodeTraverser()

	node := vertexFromByte(1)
	graph.addNode(node, onionFeatures())

	path, err := FindPath(t.Context(), graph, node, node, 20)
	require.NoError(t, err)
	require.Len(t, path, 0)
}
