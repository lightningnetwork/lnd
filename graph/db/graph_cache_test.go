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

// TestGraphCacheDisabledPoliciesRegression is a regression test for the bug
// where channels with both policies disabled were not added to the graph cache
// during population, preventing future policy updates from working.
//
// The bug flow was:
// 1. Channel with both policies disabled exists in DB.
// 2. populateCache skips adding it to graph cache entirely.
// 3. Later, a policy update arrives enabling one direction.
// 4. UpdateEdgePolicy updates the DB successfully.
// 5. UpdateEdgePolicy tries to update graph cache but channel not found.
// 6. Channel never becomes usable for routing.
func TestGraphCacheDisabledPoliciesRegression(t *testing.T) {
	t.Parallel()

	// Create a simple cache instance.
	cache := NewGraphCache(10)

	// Simulate a channel with both policies disabled.
	chanID := uint64(12345)
	node1 := pubKey1
	node2 := pubKey2

	edgeInfo := &models.CachedEdgeInfo{
		ChannelID:     chanID,
		NodeKey1Bytes: node1,
		NodeKey2Bytes: node2,
		Capacity:      1000000,
	}

	// Create two disabled policies.
	disabledPolicy1 := &models.CachedEdgePolicy{
		ChannelID:    chanID,
		ChannelFlags: lnwire.ChanUpdateDisabled,
	}
	disabledPolicy2 := &models.CachedEdgePolicy{
		ChannelID: chanID,
		ChannelFlags: lnwire.ChanUpdateDisabled |
			lnwire.ChanUpdateDirection,
	}

	// Add the channel with both policies disabled (simulating
	// populateCache).
	cache.AddChannel(edgeInfo, disabledPolicy1, disabledPolicy2)

	// Verify the channel structure was added to cache.
	var foundChannels []*DirectedChannel
	err := cache.ForEachChannel(node1, func(c *DirectedChannel) error {
		if c.ChannelID == chanID {
			foundChannels = append(foundChannels, c)
		}

		return nil
	})
	require.NoError(t, err)
	require.Len(t, foundChannels, 1,
		"channel structure should be in cache even when both "+
			"policies are disabled")

	// Verify policies were NOT added (both disabled).
	require.False(t, foundChannels[0].OutPolicySet,
		"disabled outgoing policy should not be set in cache")
	require.Nil(t, foundChannels[0].InPolicy,
		"disabled incoming policy should not be set in cache")

	// Now simulate receiving a fresh update enabling one direction.
	enabledPolicy1 := &models.CachedEdgePolicy{
		ChannelID:     chanID,
		ChannelFlags:  0, // NOT disabled anymore
		TimeLockDelta: 40,
		MinHTLC:       lnwire.MilliSatoshi(1000),
	}

	// Update the policy (simulating what UpdateEdgePolicy does).
	cache.UpdatePolicy(enabledPolicy1, node1, node2)

	// Verify the policy update succeeded. Before the fix, UpdatePolicy
	// would log "Channel not found in graph cache" and return early,
	// so the policy would never be added.
	foundChannels = nil
	err = cache.ForEachChannel(node1, func(c *DirectedChannel) error {
		if c.ChannelID == chanID {
			foundChannels = append(foundChannels, c)
		}

		return nil
	})
	require.NoError(t, err)
	require.Len(t, foundChannels, 1)

	// The policy should now be set.
	require.True(t, foundChannels[0].OutPolicySet,
		"REGRESSION: policy update should work even for channels that "+
			"had both policies disabled initially")

	// Verify we can also see it from node2's perspective.
	foundChannels = nil
	err = cache.ForEachChannel(node2, func(c *DirectedChannel) error {
		if c.ChannelID == chanID {
			foundChannels = append(foundChannels, c)
		}

		return nil
	})
	require.NoError(t, err)
	require.Len(t, foundChannels, 1)
	require.NotNil(t, foundChannels[0].InPolicy,
		"incoming policy should be set after policy update")
	require.Equal(t, uint16(40), foundChannels[0].InPolicy.TimeLockDelta)
}
