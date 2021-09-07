package channeldb

import (
	"encoding/hex"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
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

type node struct {
	pubKey   route.Vertex
	features *lnwire.FeatureVector

	edgeInfos   []*ChannelEdgeInfo
	outPolicies []*ChannelEdgePolicy
	inPolicies  []*ChannelEdgePolicy
}

func (n *node) PubKey() route.Vertex {
	return n.pubKey
}
func (n *node) Features() *lnwire.FeatureVector {
	return n.features
}

func (n *node) ForEachChannel(tx kvdb.RTx,
	cb func(kvdb.RTx, *ChannelEdgeInfo, *ChannelEdgePolicy,
		*ChannelEdgePolicy) error) error {

	for idx := range n.edgeInfos {
		err := cb(
			tx, n.edgeInfos[idx], n.outPolicies[idx],
			n.inPolicies[idx],
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// TestGraphCacheAddNode tests that a channel going from node A to node B can be
// cached correctly, independent of the direction we add the channel as.
func TestGraphCacheAddNode(t *testing.T) {
	runTest := func(nodeA, nodeB route.Vertex) {
		t.Helper()

		outPolicy1 := &ChannelEdgePolicy{
			ChannelID:    1000,
			ChannelFlags: 0,
			Node: &LightningNode{
				PubKeyBytes: nodeB,
			},
		}
		inPolicy1 := &ChannelEdgePolicy{
			ChannelID:    1000,
			ChannelFlags: 1,
			Node: &LightningNode{
				PubKeyBytes: nodeA,
			},
		}
		node := &node{
			pubKey:   nodeA,
			features: lnwire.EmptyFeatureVector(),
			edgeInfos: []*ChannelEdgeInfo{{
				ChannelID: 1000,
				// Those are direction independent!
				NodeKey1Bytes: pubKey1,
				NodeKey2Bytes: pubKey2,
				Capacity:      500,
			}},
			outPolicies: []*ChannelEdgePolicy{outPolicy1},
			inPolicies:  []*ChannelEdgePolicy{inPolicy1},
		}
		cache := NewGraphCache()
		require.NoError(t, cache.AddNode(nil, node))

		fromChannels := cache.nodeChannels[nodeA]
		toChannels := cache.nodeChannels[nodeB]

		require.Len(t, fromChannels, 1)
		require.Len(t, toChannels, 1)

		require.Equal(t, outPolicy1, fromChannels[0].OutPolicy)
		require.Equal(t, inPolicy1, fromChannels[0].InPolicy)

		require.Equal(t, inPolicy1, toChannels[0].OutPolicy)
		require.Equal(t, outPolicy1, toChannels[0].InPolicy)
	}
	runTest(pubKey1, pubKey2)
	runTest(pubKey2, pubKey1)
}
