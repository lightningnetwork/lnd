//go:build test_db_postgres || test_db_sqlite

package graphdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNodeIsPublicCacheInvalidation ensures that we invalidate correctly our
// cache we use when determing if a node is public or not.
func TestNodeIsPublicCacheInvalidation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	require.NoError(t, graph.AddNode(ctx, node1))
	require.NoError(t, graph.AddNode(ctx, node2))

	edge, _ := createEdge(10, 0, 0, 0, node1, node2)
	require.NoError(t, graph.AddChannelEdge(ctx, &edge))

	// First IsPublic call should populate cache.
	isPublic1, err := graph.IsPublicNode(node1.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, isPublic1)

	// Test invalidation scenarios:

	// 1. DeleteChannelEdges:
	// Above, the channel being public should be cached, but we expect that
	// DeleteChannelEdge will invalidate the cache for both nodes else when
	// we call IsPublic, we will hit the cache.
	err = graph.DeleteChannelEdges(false, true, edge.ChannelID)
	require.NoError(t, err)
	isPublic1, err = graph.IsPublicNode(node1.PubKeyBytes)
	require.NoError(t, err)
	require.False(t, isPublic1)

	isPublic2, err := graph.IsPublicNode(node2.PubKeyBytes)
	require.NoError(t, err)
	require.False(t, isPublic2)

	// 2. AddChannelEdge:
	// Now we know that the last `IsPublicNode` call above will cache our
	// nodes with `isPublic` = false. But add a new channel edge should
	// invalidate the cache such that when we call `IsPublic` it should
	// return `True`.
	edge2, _ := createEdge(10, 1, 0, 1, node1, node2)
	require.NoError(t, graph.AddChannelEdge(ctx, &edge2))
	isPublic1, err = graph.IsPublicNode(node1.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, isPublic1)

	isPublic2, err = graph.IsPublicNode(node2.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, isPublic2)

	// 3. DeleteNode:
	// Again, the last two sets of `IsPublic` should have cached our nodes
	// as `True`. Now we can delete a node and expect the next call to be
	// False.
	//
	// NOTE: We don't get an error calling `IsPublicNode` because of how the
	// SQL query is implemented to check for the existence of public nodes.
	require.NoError(t, graph.DeleteNode(ctx, node1.PubKeyBytes))
	isPublic1, err = graph.IsPublicNode(node1.PubKeyBytes)
	require.NoError(t, err)
	require.False(t, isPublic1)
}
