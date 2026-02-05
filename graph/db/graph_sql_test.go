//go:build test_db_postgres || test_db_sqlite

package graphdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNodeIsPublicCache verifies that once a node is observed as public, it
// remains public on cache hit even if it later has zero public channels in the
// DB.
//
// NOTE: Once a pubkey is leaked, we don't invalidate the key from the cache
// even if it has no public channels anymore. This is because once leaked as
// public, the node cannot undo the state.
func TestNodeIsPublicCache(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	alice := createTestVertex(t)
	bob := createTestVertex(t)
	carol := createTestVertex(t)

	require.NoError(t, graph.SetSourceNode(ctx, alice))

	alice.LastUpdate = nextUpdateTime()
	bob.LastUpdate = nextUpdateTime()
	carol.LastUpdate = nextUpdateTime()

	require.NoError(t, graph.AddNode(ctx, alice))
	require.NoError(t, graph.AddNode(ctx, bob))
	require.NoError(t, graph.AddNode(ctx, carol))

	// Carol has no public channels, so she should be private.
	isPublic, err := graph.IsPublicNode(carol.PubKeyBytes)
	require.NoError(t, err)
	require.False(t, isPublic)

	// Add a public edge so Alice becomes public and is cached as such.
	edge, _ := createEdge(10, 0, 0, 0, alice, bob)
	require.NoError(t, graph.AddChannelEdge(ctx, &edge))

	isPublic, err = graph.IsPublicNode(alice.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, isPublic)

	// Delete Alice's only public edge. Since we're using a public node
	// cache, Alice is still treated as public until she is evicted from
	// the cache.
	require.NoError(t, graph.DeleteChannelEdges(false, true, edge.ChannelID))

	isPublic, err = graph.IsPublicNode(alice.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, isPublic)
}
