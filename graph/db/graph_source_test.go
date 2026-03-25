package graphdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestDBGraphSourceSourceNodeFallback ensures the graph source falls back to
// the next supported gossip version when the preferred version does not yet
// have a source node persisted.
func TestDBGraphSourceSourceNodeFallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	graph := MakeTestGraph(t)
	source := NewDBGraphSource(graph)

	_, err := source.SourceNode(ctx)
	require.ErrorIs(t, err, ErrSourceNodeNotSet)

	node := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, graph.SetSourceNode(ctx, node))

	sourceNode, err := source.SourceNode(ctx)
	require.NoError(t, err)
	compareNodes(t, node, sourceNode)
}
