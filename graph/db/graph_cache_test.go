package graphdb

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand"
	"testing"
	"time"

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

	pubKey3Bytes, _ = hex.DecodeString(
		"02a1633e42df2134a2b0330159d66575316471583448590688c7a1418da7" +
			"33d9e4",
	)

	pubKey1, _ = route.NewVertexFromBytes(pubKey1Bytes)
	pubKey2, _ = route.NewVertexFromBytes(pubKey2Bytes)
	pubKey3, _ = route.NewVertexFromBytes(pubKey3Bytes)
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

// TestGraphCacheCleanupZombies tests that the cleanupZombies function correctly
// removes zombie channels from the cache.
func TestGraphCacheCleanupZombies(t *testing.T) {
	t.Parallel()

	cache := NewGraphCache(10)

	// Add a channel between two nodes. This will be the zombie.
	info1 := &models.CachedEdgeInfo{
		ChannelID:     1000,
		NodeKey1Bytes: pubKey1,
		NodeKey2Bytes: pubKey2,
		Capacity:      500,
	}
	cache.AddChannel(info1, nil, nil)

	// Add a second channel, which will NOT be a zombie.
	info2 := &models.CachedEdgeInfo{
		ChannelID:     1001,
		NodeKey1Bytes: pubKey2,
		NodeKey2Bytes: pubKey3,
		Capacity:      500,
	}

	cache.AddChannel(info2, nil, nil)
	zeroVertex := route.Vertex{}

	// Try to remove the channel, which will mark it as a zombie.
	cache.RemoveChannel(zeroVertex, zeroVertex, 1000)
	require.Equal(t, 1, len(cache.zombieIndex))

	// Now, run the cleanup function.
	cache.cleanupZombies()

	// Check that the zombie channel has been removed from the first node.
	require.Empty(t, cache.nodeChannels[pubKey1])

	// Check that the second node still has the non-zombie channel.
	require.Len(t, cache.nodeChannels[pubKey2], 1)
	_, ok := cache.nodeChannels[pubKey2][1001]
	require.True(t, ok)

	// And the third node should also have the non-zombie channel.
	require.Len(t, cache.nodeChannels[pubKey3], 1)
	_, ok = cache.nodeChannels[pubKey3][1001]
	require.True(t, ok)

	// And the zombie index should be cleared.
	require.Empty(t, cache.zombieIndex)
}

// TestGraphCacheZombieCleanerLifeCycle tests that the zombie cleaner's Start
// and Stop functions work correctly.
func TestGraphCacheZombieCleanerLifeCycle(t *testing.T) {
	t.Parallel()

	cache := NewGraphCache(10)
	cache.zombieCleanerInterval = 50 * time.Millisecond
	zeroVertex := route.Vertex{}
	cache.RemoveChannel(zeroVertex, zeroVertex, 123)
	cache.Start()

	// Wait for the cleaner to run and clean up the zombie index.
	require.Eventually(t, func() bool {
		cache.mtx.RLock()
		defer cache.mtx.RUnlock()
		return len(cache.zombieIndex) == 0
	}, time.Second, 10*time.Millisecond)

	// Stop the cleaner. This will block until the goroutine has exited.
	// If it doesn't exit, the test will time out.
	cache.Stop()
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

// BenchmarkGraphCacheCleanupZombies benchmarks the cleanupZombies function
// with a large number of nodes and channels. It creates a graph cache with
// 50,000 nodes and 500,000 channels, marks 10% of the channels as zombies,
// and then runs the cleanup function.
func BenchmarkGraphCacheCleanupZombies(b *testing.B) {
	const (
		numNodes          = 50_000
		numChannels       = 500_000
		zombiePercentage  = 10
		channelCapacity   = 1_000_000
		pubkeyMultiplier  = 100
		channelMultiplier = 1000
	)

	zeroVertex := route.Vertex{}

	// Generate test nodes with deterministic pubkeys
	nodes := make([]route.Vertex, numNodes)
	for i := range numNodes {
		var pubkeyBytes [33]byte
		binary.LittleEndian.PutUint64(pubkeyBytes[:],
			uint64(i*pubkeyMultiplier))

		node, err := route.NewVertexFromBytes(pubkeyBytes[:])
		if err != nil {
			b.Fatalf("unable to create pubkey for node %d: %v", i,
				err)
		}

		nodes[i] = node
	}

	for range b.N {
		// Create and populate graph cache with random channels
		cache := NewGraphCache(numNodes)
		for i := range numChannels {
			randomNode1 := nodes[rand.Intn(numNodes)]
			randomNode2 := nodes[rand.Intn(numNodes)]
			channelID := uint64(i * channelMultiplier)

			channelInfo := &models.CachedEdgeInfo{
				ChannelID:     channelID,
				NodeKey1Bytes: randomNode1,
				NodeKey2Bytes: randomNode2,
				Capacity:      channelCapacity,
			}

			cache.AddChannel(channelInfo, nil, nil)
		}

		// Mark 10% of channels as zombies by removing them
		// This creates both valid zombie entries and invalid ones
		zombieChannelCount := numChannels / zombiePercentage
		for j := range zombieChannelCount {
			// Remove existing channel (creates valid zombie entry)
			existingChannelID := uint64(j * channelMultiplier *
				zombiePercentage)
			cache.RemoveChannel(zeroVertex, zeroVertex,
				existingChannelID)

			// Remove non-existing channel (creates invalid zombie
			// entry)
			invalidChannelID := existingChannelID + 5
			cache.RemoveChannel(zeroVertex, zeroVertex,
				invalidChannelID)
		}

		// Benchmark the cleanup operation
		b.StartTimer()
		cache.cleanupZombies()
		b.StopTimer()
	}

	// Report benchmark metrics
	b.ReportAllocs()
	b.ReportMetric(
		float64(b.Elapsed().Milliseconds())/float64(b.N),
		"ms/op",
	)
}
