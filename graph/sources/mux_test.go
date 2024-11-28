package sources

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	selfNode   = genPub()
	selfVertex = route.NewVertex(selfNode)

	node1  = genPub()
	node1V = route.NewVertex(node1)

	node2 = genPub()
	node3 = genPub()
	node4 = genPub()

	testChanPoint = &wire.OutPoint{
		Hash: chainhash.Hash{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4,
		},
		Index: 1,
	}
)

// TestMuxForEachNodeChannel tests the ability of the mux to query both the
// local and remote graph sources for the channels in the graph.
func TestMuxForEachNodeChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("self node", func(t *testing.T) {
		kit := newTestKit(t)
		localChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
		}

		kit.local.On(
			"ForEachNodeChannel", mock.Anything, selfVertex,
			mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(2).(func(
				info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range localChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		var collectedChans []*models.ChannelEdgeInfo
		err := kit.mux.ForEachNodeChannel(
			ctx, selfVertex, func(info *models.ChannelEdgeInfo,
				p1 *models.ChannelEdgePolicy,
				p2 *models.ChannelEdgePolicy) error {

				collectedChans = append(collectedChans, info)
				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, localChans, collectedChans)
	})

	t.Run("merge results", func(t *testing.T) {
		kit := newTestKit(t)

		localChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
			{ChannelID: 2},
		}
		remoteChans := []*models.ChannelEdgeInfo{
			{ChannelID: 2},
			{ChannelID: 3},
		}

		expectedChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
			{ChannelID: 2},
			{ChannelID: 3},
		}

		kit.local.On(
			"ForEachNodeChannel", mock.Anything, node1V,
			mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(2).(func(
				info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range localChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		kit.remote.On(
			"ForEachNodeChannel", mock.Anything, node1V,
			mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(2).(func(
				info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range remoteChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		var collectedChans []*models.ChannelEdgeInfo
		err := kit.mux.ForEachNodeChannel(
			ctx, node1V, func(info *models.ChannelEdgeInfo,
				p1 *models.ChannelEdgePolicy,
				p2 *models.ChannelEdgePolicy) error {

				collectedChans = append(collectedChans, info)
				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, expectedChans, collectedChans)
	})
}

// TestMuxForEachChannel tests the ability of the mux to query both the local
// and remote graph sources for the channels in the graph.
func TestMuxForEachChannel(t *testing.T) {
	t.Parallel()

	t.Run("local and remote node have local channel", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)
		)

		localChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
		}

		remoteChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
			{ChannelID: 2},
		}

		kit.local.On(
			"ForEachNodeChannel", mock.Anything, selfVertex,
			mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(2).(func(
				info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range localChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		kit.remote.On(
			"ForEachChannel", mock.Anything, mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(1).(func(
				info *models.ChannelEdgeInfo,
				_, _ *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range remoteChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		var collectedChans []*models.ChannelEdgeInfo
		err := kit.mux.ForEachChannel(
			ctx, func(info *models.ChannelEdgeInfo,
				_, _ *models.ChannelEdgePolicy) error {

				collectedChans = append(collectedChans, info)
				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, remoteChans, collectedChans)
	})

	t.Run("remote doesnt have local channel", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)
		)

		localChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
		}

		remoteChans := []*models.ChannelEdgeInfo{
			{ChannelID: 2},
		}

		expectedChans := []*models.ChannelEdgeInfo{
			{ChannelID: 1},
			{ChannelID: 2},
		}

		kit.local.On(
			"ForEachNodeChannel", mock.Anything, selfVertex,
			mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(2).(func(
				info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range localChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		kit.remote.On(
			"ForEachChannel", mock.Anything, mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(1).(func(
				info *models.ChannelEdgeInfo,
				_, _ *models.ChannelEdgePolicy) error)
			require.True(t, ok)

			for _, chanInfo := range remoteChans {
				require.NoError(t, cb(chanInfo, nil, nil))
			}
		}).Return(nil)

		var collectedChans []*models.ChannelEdgeInfo
		err := kit.mux.ForEachChannel(
			ctx, func(info *models.ChannelEdgeInfo,
				_, _ *models.ChannelEdgePolicy) error {

				collectedChans = append(collectedChans, info)
				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, expectedChans, collectedChans)
	})
}

// TestMuxForEachNodeDirectedChannel tests the ability of the mux to query
// both the local and remote graph sources for the directed channels of a node.
func TestMuxForEachNodeDirectedChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// If the node in question is our node, we have all the data locally
	// and should only need to therefore query our local graph source.
	t.Run("local node", func(t *testing.T) {
		kit := newTestKit(t)

		localChanDB := []*graphdb.DirectedChannel{
			{ChannelID: 1},
			{ChannelID: 2},
		}

		kit.local.On(
			"ForEachNodeDirectedChannel", mock.Anything,
			mock.Anything, selfVertex, mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(3).(func(
				*graphdb.DirectedChannel) error)
			require.True(t, ok)

			for _, chanInfo := range localChanDB {
				require.NoError(t, cb(chanInfo))
			}
		}).Return(nil)

		var collectedChans []*graphdb.DirectedChannel
		err := kit.mux.ForEachNodeDirectedChannel(ctx, nil, selfVertex,
			func(chanInfo *graphdb.DirectedChannel) error {
				collectedChans = append(
					collectedChans, chanInfo,
				)

				return nil
			},
		)
		require.NoError(t, err)

		require.Equal(t, localChanDB, collectedChans)
	})

	// The node in question is not our node, but when we check our DB we
	// see that it is our direct channel peer, and so we account for the
	// channel directly. We'll also let the remote source hold a different
	// channel for this peer that we don't know about. We'll also let there
	// be a known channel that both DBs have in common. This should only be
	// accounted for once.
	t.Run("local node peer", func(t *testing.T) {
		kit := newTestKit(t)

		localChanDB := []*graphdb.DirectedChannel{
			// A channel between us and the node in question that
			// the remote node doesn't know about (private channel).
			{
				ChannelID: 1,
				OtherNode: route.NewVertex(selfNode),
			},
			// A channel that both us and the remote node know
			// about.
			{
				ChannelID: 2,
				OtherNode: route.NewVertex(node1),
			},
		}

		remoteChanDB := []*graphdb.DirectedChannel{
			// A channel that both us and the remote node know
			// about.
			{
				ChannelID: 2,
				OtherNode: route.NewVertex(node1),
			},
			// A channel that only the remote node knows about.
			{
				ChannelID: 3,
				OtherNode: route.NewVertex(node2),
			},
		}

		expectedList := []*graphdb.DirectedChannel{
			{
				ChannelID: 1,
				OtherNode: route.NewVertex(selfNode),
			},
			{
				ChannelID: 2,
				OtherNode: route.NewVertex(node1),
			},
			{
				ChannelID: 3,
				OtherNode: route.NewVertex(node2),
			},
		}

		kit.local.On(
			"ForEachNodeDirectedChannel", mock.Anything,
			mock.Anything, route.NewVertex(node3), mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(3).(func(
				*graphdb.DirectedChannel) error)
			require.True(t, ok)

			for _, chanInfo := range localChanDB {
				require.NoError(t, cb(chanInfo))
			}
		}).Return(nil)

		kit.remote.On(
			"ForEachNodeDirectedChannel", mock.Anything,
			mock.Anything, route.NewVertex(node3), mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(3).(func(
				*graphdb.DirectedChannel) error)
			require.True(t, ok)

			for _, chanInfo := range remoteChanDB {
				require.NoError(t, cb(chanInfo))
			}
		}).Return(nil)

		var collectedChans []*graphdb.DirectedChannel
		err := kit.mux.ForEachNodeDirectedChannel(
			ctx, nil, route.NewVertex(node3),
			func(chanInfo *graphdb.DirectedChannel) error {
				collectedChans = append(
					collectedChans, chanInfo,
				)

				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, expectedList, collectedChans)
	})

	// The node in question is completely unknown to us, but the remote node
	// knows about a channel with this node.
	t.Run("unknown to local", func(t *testing.T) {
		kit := newTestKit(t)

		remoteChanDB := []*graphdb.DirectedChannel{
			{
				ChannelID: 3,
				OtherNode: route.NewVertex(node2),
			},
		}

		kit.local.On(
			"ForEachNodeDirectedChannel", mock.Anything,
			mock.Anything, route.NewVertex(node3), mock.Anything,
		).Return(nil)

		kit.remote.On(
			"ForEachNodeDirectedChannel", mock.Anything,
			mock.Anything, route.NewVertex(node3), mock.Anything,
		).Run(func(args mock.Arguments) {
			cb, ok := args.Get(3).(func(
				*graphdb.DirectedChannel) error)
			require.True(t, ok)

			for _, chanInfo := range remoteChanDB {
				require.NoError(t, cb(chanInfo))
			}
		}).Return(nil)

		var collectedChans []*graphdb.DirectedChannel
		err := kit.mux.ForEachNodeDirectedChannel(
			ctx, nil, route.NewVertex(node3),
			func(chanInfo *graphdb.DirectedChannel) error {
				collectedChans = append(
					collectedChans, chanInfo,
				)

				return nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, remoteChanDB, collectedChans)
	})
}

// TestMuxFetchNodeFeatures tests the ability of the mux to query both the local
// and remote graph sources for the features of a node.
func TestMuxFetchNodeFeatures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		feat1 = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(1),
			map[lnwire.FeatureBit]string{
				1: "feat_1",
			},
		)
	)

	// If the node in question is the local node, then only the local source
	// is queried.
	t.Run("local node", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchNodeFeatures", ctx, mock.Anything, selfVertex,
		).Return(feat1, nil)

		fts, err := kit.mux.FetchNodeFeatures(ctx, nil, selfVertex)
		require.NoError(t, err)
		require.Equal(t, feat1, fts)
	})

	// If the node in question is not the local node, then we first query
	// remote source. If the feature vector returned is not empty, then we
	// are done.
	t.Run("remote knows", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On(
			"FetchNodeFeatures", ctx, mock.Anything, node1V,
		).Return(feat1, nil)

		fts, err := kit.mux.FetchNodeFeatures(ctx, nil, node1V)
		require.NoError(t, err)
		require.Equal(t, feat1, fts)
	})

	// The node in question is not the local node, so we first query the
	// remote source but the remote source returns an empty vector so we
	// query the local source as well.
	t.Run("remote returns empty vector", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On(
			"FetchNodeFeatures", ctx, mock.Anything, node1V,
		).Return(lnwire.EmptyFeatureVector(), nil)

		kit.local.On(
			"FetchNodeFeatures", ctx, mock.Anything, node1V,
		).Return(feat1, nil)

		fts, err := kit.mux.FetchNodeFeatures(ctx, nil, node1V)
		require.NoError(t, err)
		require.Equal(t, feat1, fts)
	})
}

// TestMuxForEachNode tests the ability of the mux to query both the local and
// remote graph sources for the nodes in the graph.
func TestMuxForEachNode(t *testing.T) {
	t.Parallel()

	var (
		ctx = context.Background()
		kit = newTestKit(t)
	)

	localNodes := []*models.LightningNode{
		{PubKeyBytes: selfVertex},
		{PubKeyBytes: route.NewVertex(node1)},
		{PubKeyBytes: route.NewVertex(node2)},
	}

	remoteNodes := []*models.LightningNode{
		{PubKeyBytes: route.NewVertex(node2)},
		{PubKeyBytes: route.NewVertex(node3)},
	}

	expectedNodes := []*models.LightningNode{
		// The remote source is addressed first.
		{PubKeyBytes: route.NewVertex(node2)},
		{PubKeyBytes: route.NewVertex(node3)},
		// Then the local source is addressed.
		{PubKeyBytes: selfVertex},
		{PubKeyBytes: route.NewVertex(node1)},
	}

	kit.local.On(
		"ForEachNode", mock.Anything, mock.Anything,
	).Run(func(args mock.Arguments) {
		cb, ok := args.Get(1).(func(*models.LightningNode) error)
		require.True(t, ok)

		for _, node := range localNodes {
			require.NoError(t, cb(node))
		}
	}).Return(nil)

	kit.remote.On(
		"ForEachNode", mock.Anything, mock.Anything,
	).Run(func(args mock.Arguments) {
		cb, ok := args.Get(1).(func(*models.LightningNode) error)
		require.True(t, ok)

		for _, node := range remoteNodes {
			require.NoError(t, cb(node))
		}
	}).Return(nil)

	var collectedNodes []*models.LightningNode
	err := kit.mux.ForEachNode(
		ctx, func(node *models.LightningNode) error {
			collectedNodes = append(collectedNodes, node)
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, expectedNodes, collectedNodes)
}

// TestMuxFetchChannelEdgesByID tests the ability of the mux to query both the
// local and remote graph sources for the channel edge info of a channel.
func TestMuxFetchChannelEdgesByID(t *testing.T) {
	t.Parallel()

	var (
		ctx = context.Background()

		localChan = &models.ChannelEdgeInfo{
			ChannelID:     1,
			NodeKey1Bytes: selfVertex,
		}
		chan2 = &models.ChannelEdgeInfo{
			ChannelID:     2,
			NodeKey1Bytes: node1V,
		}
		chan3 = &models.ChannelEdgeInfo{
			ChannelID:     3,
			NodeKey1Bytes: node1V,
		}
	)

	t.Run("local node's channel", func(t *testing.T) {
		kit := newTestKit(t)

		// Local node is queried for a channel owned by it. So no other
		// queries are needed.
		kit.local.On("FetchChannelEdgesByID", ctx, uint64(1)).Return(
			localChan, nil, nil, nil,
		)

		edge, _, _, err := kit.mux.FetchChannelEdgesByID(ctx, uint64(1))
		require.NoError(t, err)
		require.Equal(t, localChan, edge)
	})

	t.Run("local node does know but remote is preferred",
		func(t *testing.T) {
			kit := newTestKit(t)

			kit.local.On(
				"FetchChannelEdgesByID", ctx, uint64(1),
			).Return(chan2, nil, nil, nil)

			kit.remote.On(
				"FetchChannelEdgesByID", ctx, uint64(1),
			).Return(chan3, nil, nil, nil)

			edge, _, _, err := kit.mux.FetchChannelEdgesByID(
				ctx, uint64(1),
			)
			require.NoError(t, err)
			require.Equal(t, chan3, edge)
		},
	)

	t.Run("local node does not know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		kit.remote.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(chan3, nil, nil, nil)

		edge, _, _, err := kit.mux.FetchChannelEdgesByID(
			ctx, uint64(1),
		)
		require.NoError(t, err)
		require.Equal(t, chan3, edge)
	})

	t.Run("remote does not know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(chan2, nil, nil, nil)

		kit.remote.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		edge, _, _, err := kit.mux.FetchChannelEdgesByID(
			ctx, uint64(1),
		)
		require.NoError(t, err)
		require.Equal(t, chan2, edge)
	})

	t.Run("neither know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		kit.remote.On(
			"FetchChannelEdgesByID", ctx, uint64(1),
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		_, _, _, err := kit.mux.FetchChannelEdgesByID(
			ctx, uint64(1),
		)
		require.ErrorIs(t, err, graphdb.ErrEdgeNotFound)
	})
}

// TestMuxFetchChannelEdgesByOutpoint tests the ability of the mux to query both
// the local and remote graph sources for the channel edge info of a channel.
func TestMuxFetchChannelEdgesByOutpoint(t *testing.T) {
	t.Parallel()

	var (
		ctx       = context.Background()
		localChan = &models.ChannelEdgeInfo{
			ChannelID:     1,
			NodeKey1Bytes: selfVertex,
		}
		chan1 = &models.ChannelEdgeInfo{
			ChannelID:     2,
			NodeKey1Bytes: node1V,
		}
		chan2 = &models.ChannelEdgeInfo{
			ChannelID:     3,
			NodeKey1Bytes: node1V,
		}
	)

	t.Run("local node's channel", func(t *testing.T) {
		kit := newTestKit(t)

		// Local node is queried for a channel owned by it. So no other
		// queries are needed.
		kit.local.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(localChan, nil, nil, nil)

		edge, _, _, err := kit.mux.FetchChannelEdgesByOutpoint(
			ctx, testChanPoint,
		)
		require.NoError(t, err)
		require.Equal(t, localChan, edge)
	})

	t.Run("local knows but remote is preferred", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(chan1, nil, nil, nil)

		kit.remote.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(chan2, nil, nil, nil)

		edge, _, _, err := kit.mux.FetchChannelEdgesByOutpoint(
			ctx, testChanPoint,
		)
		require.NoError(t, err)
		require.Equal(t, chan2, edge)
	})

	t.Run("local does not know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		kit.remote.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(chan2, nil, nil, nil)

		edge, _, _, err := kit.mux.FetchChannelEdgesByOutpoint(
			ctx, testChanPoint,
		)
		require.NoError(t, err)
		require.Equal(t, chan2, edge)
	})

	t.Run("remote does not know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(chan1, nil, nil, nil)

		kit.remote.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		edge, _, _, err := kit.mux.FetchChannelEdgesByOutpoint(
			ctx, testChanPoint,
		)
		require.NoError(t, err)
		require.Equal(t, chan1, edge)
	})

	t.Run("neither know", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		kit.remote.On(
			"FetchChannelEdgesByOutpoint", ctx, testChanPoint,
		).Return(nil, nil, nil, graphdb.ErrEdgeNotFound)

		_, _, _, err := kit.mux.FetchChannelEdgesByOutpoint(
			ctx, testChanPoint,
		)
		require.ErrorIs(t, err, graphdb.ErrEdgeNotFound)
	})
}

// TestMuxIsPublicNode tests the ability of the mux to query both the local and
// remote graph sources for the public status of a node.
func TestMuxIsPublicNode(t *testing.T) {
	t.Parallel()

	var (
		ctx = context.Background()
		kit = newTestKit(t)

		node1B = [33]byte(route.NewVertex(node1))
		node2B = [33]byte(route.NewVertex(node2))
		node3B = [33]byte(route.NewVertex(node3))
		node4B = [33]byte(route.NewVertex(node4))
	)

	// Local source knows about node 1. Remote does not need to be queried.
	kit.local.On("IsPublicNode", ctx, node1B).Return(true, nil)

	public, err := kit.mux.IsPublicNode(ctx, node1B)
	require.NoError(t, err)
	require.True(t, public)

	// Local knows the node but doesn't see it as public. In this case we
	// want to also check the remote source.
	kit.local.On("IsPublicNode", ctx, node2B).Return(false, nil)
	kit.remote.On("IsPublicNode", ctx, node2B).Return(true, nil)

	public, err = kit.mux.IsPublicNode(ctx, node2B)
	require.NoError(t, err)
	require.True(t, public)

	// Local source does not know node. Remote result should be used.
	kit.local.On("IsPublicNode", ctx, node3B).Return(
		false, graphdb.ErrGraphNodeNotFound,
	)
	kit.remote.On("IsPublicNode", ctx, node3B).Return(true, nil)

	public, err = kit.mux.IsPublicNode(ctx, node3B)
	require.NoError(t, err)
	require.True(t, public)

	// Both local and remote don't know about the node.
	kit.local.On("IsPublicNode", ctx, node4B).Return(
		false, graphdb.ErrGraphNodeNotFound,
	)
	kit.remote.On("IsPublicNode", ctx, node4B).Return(
		false, graphdb.ErrGraphNodeNotFound,
	)

	_, err = kit.mux.IsPublicNode(ctx, node4B)
	require.ErrorIs(t, err, graphdb.ErrGraphNodeNotFound)
}

// TestMuxAddrsForNodes tests the ability of the mux to query both the local and
// remote graph sources for the addresses of a node.
func TestMuxAddrsForNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	addr1, err := net.ResolveTCPAddr("tcp", "localhost:1")
	require.NoError(t, err)

	addr2, err := net.ResolveTCPAddr("tcp", "localhost:2")
	require.NoError(t, err)

	addr3, err := net.ResolveTCPAddr("tcp", "localhost:3")
	require.NoError(t, err)

	t.Run("neither source knows the node", func(t *testing.T) {
		var (
			kit = newTestKit(t)
		)

		kit.local.On("AddrsForNode", ctx, selfNode).Return(
			false, nil, nil,
		)
		kit.remote.On("AddrsForNode", ctx, selfNode).Return(
			false, nil, nil,
		)

		known, _, err := kit.mux.AddrsForNode(ctx, selfNode)
		require.NoError(t, err)
		require.False(t, known)
	})

	t.Run("local source knows node. Remote does not", func(t *testing.T) {
		var (
			kit = newTestKit(t)
		)

		kit.local.On("AddrsForNode", ctx, selfNode).Return(
			true, []net.Addr{addr1}, nil,
		)
		kit.remote.On("AddrsForNode", ctx, selfNode).Return(
			false, nil, nil,
		)

		known, addrs, err := kit.mux.AddrsForNode(ctx, selfNode)
		require.NoError(t, err)
		require.True(t, known)
		require.EqualValues(t, []net.Addr{addr1}, addrs)
	})

	t.Run("remote source knows node. Local does not", func(t *testing.T) {
		var (
			kit = newTestKit(t)
		)

		kit.local.On("AddrsForNode", ctx, selfNode).Return(
			false, nil, nil,
		)
		kit.remote.On("AddrsForNode", ctx, selfNode).Return(
			true, []net.Addr{addr1}, nil,
		)

		known, addrs, err := kit.mux.AddrsForNode(ctx, selfNode)
		require.NoError(t, err)
		require.True(t, known)
		require.EqualValues(t, []net.Addr{addr1}, addrs)
	})

	t.Run("both sources know the node", func(t *testing.T) {
		var (
			kit = newTestKit(t)
		)

		kit.local.On("AddrsForNode", ctx, selfNode).Return(
			true, []net.Addr{addr1, addr2}, nil,
		)
		kit.remote.On("AddrsForNode", ctx, selfNode).Return(
			true, []net.Addr{addr2, addr3}, nil,
		)

		known, addrs, err := kit.mux.AddrsForNode(ctx, selfNode)
		require.NoError(t, err)
		require.True(t, known)
		require.ElementsMatch(t, []net.Addr{addr1, addr2, addr3}, addrs)
	})

	t.Run("local errors", func(t *testing.T) {
		var kit = newTestKit(t)

		kit.local.On("AddrsForNode", ctx, selfNode).Return(
			false, nil, fmt.Errorf("err"),
		)

		_, _, err := kit.mux.AddrsForNode(ctx, selfNode)
		require.Error(t, err)
	})
}

// TestMuxHasLightningNode tests the ability of the mux to query both the local
// and remote graph sources for the existence of a lightning node.
func TestMuxHasLightningNode(t *testing.T) {
	t.Parallel()

	t.Run("self node", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)
		)

		kit.local.On(
			"HasLightningNode", ctx, [33]byte(selfVertex),
		).Return(
			time.Unix(1, 0), true, nil,
		)

		ts, known, err := kit.mux.HasLightningNode(ctx, selfVertex)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, time.Unix(1, 0), ts)
	})

	// If the remote source knows about the node, then there is no need to
	// query the local source.
	t.Run("known to remote", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)
		)

		kit.remote.On(
			"HasLightningNode", ctx, [33]byte(node1V),
		).Return(time.Unix(1, 0), true, nil)

		ts, known, err := kit.mux.HasLightningNode(ctx, node1V)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, time.Unix(1, 0), ts)
	})

	// If the remote source doesn't know about the node, then the local
	// source is queried.
	t.Run("known to local", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)

			node1V = route.NewVertex(node1)
		)

		kit.remote.On(
			"HasLightningNode", ctx, [33]byte(node1V),
		).Return(time.Time{}, false, nil)

		kit.local.On(
			"HasLightningNode", ctx, [33]byte(node1V),
		).Return(time.Unix(1, 0), true, nil)

		ts, known, err := kit.mux.HasLightningNode(ctx, node1V)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, time.Unix(1, 0), ts)
	})

	t.Run("known to neither", func(t *testing.T) {
		var (
			ctx = context.Background()
			kit = newTestKit(t)

			node1V = route.NewVertex(node1)
		)

		kit.remote.On(
			"HasLightningNode", ctx, [33]byte(node1V),
		).Return(time.Time{}, false, nil)

		kit.local.On(
			"HasLightningNode", ctx, [33]byte(node1V),
		).Return(time.Time{}, false, nil)

		_, known, err := kit.mux.HasLightningNode(ctx, node1V)
		require.NoError(t, err)
		require.False(t, known)
	})
}

// TestMuxLookupAlias tests the ability of the mux to query both the local and
// remote graph sources for the alias of a node.
func TestMuxLookupAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("self node", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On("LookupAlias", ctx, selfNode).Return("Alice", nil)

		alias, err := kit.mux.LookupAlias(ctx, selfNode)
		require.NoError(t, err)
		require.Equal(t, "Alice", alias)
	})

	t.Run("remote knows node", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("LookupAlias", ctx, node1).Return("Bob", nil)

		alias, err := kit.mux.LookupAlias(ctx, node1)
		require.NoError(t, err)
		require.Equal(t, "Bob", alias)
	})

	t.Run("local knows node", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("LookupAlias", ctx, node1).Return(
			"", graphdb.ErrNodeAliasNotFound,
		)
		kit.local.On("LookupAlias", ctx, node1).Return("Charlie", nil)

		alias, err := kit.mux.LookupAlias(ctx, node1)
		require.NoError(t, err)
		require.Equal(t, "Charlie", alias)
	})

	t.Run("neither source knows", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("LookupAlias", ctx, node1).Return(
			"", graphdb.ErrNodeAliasNotFound,
		)
		kit.local.On("LookupAlias", ctx, node1).Return(
			"", graphdb.ErrNodeAliasNotFound,
		)

		_, err := kit.mux.LookupAlias(ctx, node1)
		require.Equal(t, err, graphdb.ErrNodeAliasNotFound)
	})
}

// TestMuxFetchLightningNode tests the ability of the mux to query both the
// local and remote graph sources for a lightning node.
func TestMuxFetchLightningNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("self node", func(t *testing.T) {
		kit := newTestKit(t)

		kit.local.On("FetchLightningNode", ctx, selfVertex).Return(
			&models.LightningNode{
				PubKeyBytes: selfVertex,
			}, nil,
		)

		node, err := kit.mux.FetchLightningNode(ctx, selfVertex)
		require.NoError(t, err)
		require.NotNil(t, node)
	})

	t.Run("remote knows", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("FetchLightningNode", ctx, node1V).Return(
			&models.LightningNode{
				PubKeyBytes: node1V,
			}, nil,
		)

		node, err := kit.mux.FetchLightningNode(ctx, node1V)
		require.NoError(t, err)
		require.NotNil(t, node)
	})

	t.Run("local fallback", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("FetchLightningNode", ctx, node1V).Return(
			nil, graphdb.ErrGraphNodeNotFound,
		)
		kit.local.On("FetchLightningNode", ctx, node1V).Return(
			&models.LightningNode{
				PubKeyBytes: node1V,
			}, nil,
		)

		node, err := kit.mux.FetchLightningNode(ctx, node1V)
		require.NoError(t, err)
		require.NotNil(t, node)
	})

	t.Run("neither source knows", func(t *testing.T) {
		kit := newTestKit(t)

		kit.remote.On("FetchLightningNode", ctx, node1V).Return(
			nil, graphdb.ErrGraphNodeNotFound,
		)
		kit.local.On("FetchLightningNode", ctx, node1V).Return(
			nil, graphdb.ErrGraphNodeNotFound,
		)

		_, err := kit.mux.FetchLightningNode(ctx, node1V)
		require.Error(t, err, graphdb.ErrGraphNodeNotFound)
	})
}

type testKit struct {
	local  *MockGraphSource
	remote *MockGraphSource

	mux *Mux
}

func newTestKit(t *testing.T) *testKit {
	var (
		local  MockGraphSource
		remote MockGraphSource
	)
	t.Cleanup(func() {
		local.AssertExpectations(t)
		remote.AssertExpectations(t)
	})

	return &testKit{
		local:  &local,
		remote: &remote,
		mux: NewMux(&local, &remote, func() (route.Vertex,
			error) {

			return route.NewVertex(selfNode), nil
		}),
	}
}

func genPub() *btcec.PublicKey {
	key, err := btcec.NewPrivateKey()
	if err != nil {
		panic(err)
	}

	return key.PubKey()
}
