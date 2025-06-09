package graphdb

import (
	"encoding/hex"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	pubKey1Bytes, _ = hex.DecodeString(
		"0248f5cba4c6da2e4c9e01e81d1404dfac0cbaf3ee934a4fc117d2ea9a64" +
			"22c91d",
	)
	pubKey2Bytes, _ = hex.DecodeString(
		"038155ba86a8d3b23c806c855097ca5c9fa0f87621f1e7a7d2835ad057f6" +
			"f4484f",
	)

	pubKey1, _ = route.NewVertexFromBytes(pubKey1Bytes)
	pubKey2, _ = route.NewVertexFromBytes(pubKey2Bytes)
)

// TestGraphCacheAddNode tests that a channel going from node A to node B can be
// cached correctly, independent of the direction we add the channel as.
func TestGraphCacheAddNode(t *testing.T) {
	t.Parallel()

	runTest := func(nodeA, nodeB route.Vertex) {
		t.Helper()

		channelFlagA, channelFlagB := 0, 1
		if nodeA == pubKey2 {
			channelFlagA, channelFlagB = 1, 0
		}

		inboundFee := lnwire.Fee{
			BaseFee: 10,
			FeeRate: 20,
		}

		outPolicy1 := &models.CachedEdgePolicy{
			ChannelID:    1000,
			ChannelFlags: lnwire.ChanUpdateChanFlags(channelFlagA),
			ToNodePubKey: func() route.Vertex {
				return nodeB
			},
			// Define an inbound fee.
			InboundFee: fn.Some(inboundFee),
		}
		inPolicy1 := &models.CachedEdgePolicy{
			ChannelID:    1000,
			ChannelFlags: lnwire.ChanUpdateChanFlags(channelFlagB),
			ToNodePubKey: func() route.Vertex {
				return nodeA
			},
		}
		cache := NewGraphCache(10)
		cache.AddNodeFeatures(nodeA, lnwire.EmptyFeatureVector())
		cache.AddChannel(&models.CachedEdgeInfo{
			ChannelID: 1000,
			// Those are direction independent!
			NodeKey1Bytes: pubKey1,
			NodeKey2Bytes: pubKey2,
			Capacity:      500,
		}, outPolicy1, inPolicy1)

		var fromChannels, toChannels []*DirectedChannel
		_ = cache.ForEachChannel(nodeA, func(c *DirectedChannel) error {
			fromChannels = append(fromChannels, c)
			return nil
		})
		_ = cache.ForEachChannel(nodeB, func(c *DirectedChannel) error {
			toChannels = append(toChannels, c)
			return nil
		})

		require.Len(t, fromChannels, 1)
		require.Len(t, toChannels, 1)

		require.Equal(t, outPolicy1 != nil, fromChannels[0].OutPolicySet)
		assertCachedPolicyEqual(t, inPolicy1, fromChannels[0].InPolicy)

		require.Equal(t, inPolicy1 != nil, toChannels[0].OutPolicySet)
		assertCachedPolicyEqual(t, outPolicy1, toChannels[0].InPolicy)

		// Now that we've inserted two nodes into the graph, check that
		// we'll recover the same set of channels during forEachNode.
		nodes := make(map[route.Vertex]struct{})
		chans := make(map[uint64]struct{})
		_ = cache.ForEachNode(func(node route.Vertex,
			edges map[uint64]*DirectedChannel) error {

			nodes[node] = struct{}{}
			for chanID, directedChannel := range edges {
				chans[chanID] = struct{}{}

				if node == nodeA {
					require.NotZero(
						t, directedChannel.InboundFee,
					)
				} else {
					require.Zero(
						t, directedChannel.InboundFee,
					)
				}
			}

			return nil
		})

		require.Len(t, nodes, 2)
		require.Len(t, chans, 1)
	}

	runTest(pubKey1, pubKey2)
	runTest(pubKey2, pubKey1)
}

func assertCachedPolicyEqual(t *testing.T, original,
	cached *models.CachedEdgePolicy) {

	require.Equal(t, original.ChannelID, cached.ChannelID)
	require.Equal(t, original.MessageFlags, cached.MessageFlags)
	require.Equal(t, original.ChannelFlags, cached.ChannelFlags)
	require.Equal(t, original.TimeLockDelta, cached.TimeLockDelta)
	require.Equal(t, original.MinHTLC, cached.MinHTLC)
	require.Equal(t, original.MaxHTLC, cached.MaxHTLC)
	require.Equal(t, original.FeeBaseMSat, cached.FeeBaseMSat)
	require.Equal(
		t, original.FeeProportionalMillionths,
		cached.FeeProportionalMillionths,
	)
	if original.ToNodePubKey != nil {
		require.Equal(t, original.ToNodePubKey(), cached.ToNodePubKey())
	}
}
