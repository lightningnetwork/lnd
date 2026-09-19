package lnd

import (
	"testing"
	"time"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestDescribeGraphCacheKeyFields makes additions to ChannelGraphRequest fail
// loudly until the cache key is reviewed and updated for the new request
// shape.
func TestDescribeGraphCacheKeyFields(t *testing.T) {
	t.Parallel()

	fields := (&lnrpc.ChannelGraphRequest{}).
		ProtoReflect().Descriptor().Fields()
	require.Equal(t, 2, fields.Len(),
		"update describeGraphCacheKey for ChannelGraphRequest changes")
}

// TestDescribeGraphCacheByRequest verifies that DescribeGraph itself stores
// and retrieves independent cached responses for distinct request shapes.
func TestDescribeGraphCacheByRequest(t *testing.T) {
	t.Parallel()

	graph := graphdb.NewVersionedGraph(
		graphdb.MakeTestGraph(t), lnwire.GossipVersion1,
	)
	require.NoError(t, graph.Start())
	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	r := &rpcServer{
		cfg: &Config{
			Caches: &lncfg.Caches{
				RPCGraphCacheDuration: time.Hour,
			},
		},
		server: &server{v1Graph: graph},
	}

	plainReq := &lnrpc.ChannelGraphRequest{}
	plainResp, err := r.DescribeGraph(t.Context(), plainReq)
	require.NoError(t, err)

	plainResp.Nodes = []*lnrpc.LightningNode{{Alias: "cached-sentinel"}}

	privateReq := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	privateResp, err := r.DescribeGraph(t.Context(), privateReq)
	require.NoError(t, err)
	require.Empty(t, privateResp.Nodes)
	require.NotSame(t, plainResp, privateResp)

	cachedResp, err := r.DescribeGraph(t.Context(), plainReq)
	require.NoError(t, err)
	require.Same(t, plainResp, cachedResp)
	require.Equal(t, "cached-sentinel", cachedResp.Nodes[0].Alias)
}
