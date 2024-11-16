package graphrpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"image/color"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/autopilot"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	testChanPoint1 = wire.OutPoint{
		Hash: chainhash.Hash{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4,
		},
		Index: 1,
	}

	testChanPoint2 = wire.OutPoint{
		Hash: chainhash.Hash{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4,
		},
		Index: 2,
	}

	chanPoint1 = testChanPoint1.String()
	chanPoint2 = testChanPoint2.String()

	inboundFeeRecord = map[uint64][]byte{
		55555: {0, 0, 0, 10, 0, 0, 0, 20},
	}

	testFeature = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(0),
		map[lnwire.FeatureBit]string{
			0: "feature1",
		},
	)

	testPolicy = &models.CachedEdgePolicy{
		ChannelID:      5678,
		TimeLockDelta:  4,
		ChannelFlags:   lnwire.ChanUpdateDirection,
		ToNodeFeatures: testFeature,
	}

	testAddr1 = &lnrpc.NodeAddress{
		Network: "tcp",
		Addr:    "localhost:34",
	}

	testAddr2 = &lnrpc.NodeAddress{
		Network: "tcp4",
		Addr:    "197.0.0.1:5",
	}

	nonNilProof = &models.ChannelAuthProof{}
	direction   = lnwire.ChanUpdateDirection
)

// TestGraphClient tests the various calls to the graph client and that they
// behave correctly given various server responses.
func TestGraphClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		node1Pub, node1ID, node1Hex = genPub(t)
		node2Pub, node2ID, node2Hex = genPub(t)
		node3Pub, node3ID, node3Hex = genPub(t)
		_, node4ID, node4Hex        = genPub(t)
	)

	addr1, err := net.ResolveTCPAddr("tcp", "localhost:34")
	require.NoError(t, err)

	addr2, err := net.ResolveTCPAddr("tcp4", "197.0.0.1:5")
	require.NoError(t, err)

	t.Run("BetweennessCentrality", func(t *testing.T) {
		kit := newTestKit(t)

		kit.graphConn.On(
			"BetweennessCentrality", mock.Anything, mock.Anything,
			mock.Anything,
		).Return(&BetweennessCentralityResp{
			NodeBetweenness: []*BetweennessCentrality{
				{
					Node:          node1ID[:],
					Normalized:    0.1,
					NonNormalized: 0.2,
				},
				{
					Node:          node2ID[:],
					Normalized:    1.555,
					NonNormalized: 3,
				},
			},
		}, nil)

		resp, err := kit.client.BetweennessCentrality(ctx)
		require.NoError(t, err)
		require.Len(t, resp, 2)
		require.EqualValues(t, &models.BetweennessCentrality{
			Normalized:    0.1,
			NonNormalized: 0.2,
		}, resp[node1ID])
		require.EqualValues(t, &models.BetweennessCentrality{
			Normalized:    1.555,
			NonNormalized: 3,
		}, resp[node2ID])
	})

	t.Run("GraphBootstrapper", func(t *testing.T) {
		kit := newTestKit(t)

		// Obtaining the bootstrapper via GraphBootstrapper will make
		// a single call to the server's BootstrapperName method.
		kit.graphConn.On(
			"BootstrapperName", mock.Anything, mock.Anything,
			mock.Anything,
		).Return(&BoostrapperNameResp{
			Name: "bootstrapper_name",
		}, nil)

		b, err := kit.client.GraphBootstrapper(ctx)
		require.NoError(t, err)

		name := b.Name(ctx)
		require.Equal(t, "bootstrapper_name", name)

		// A call to the returned bootstrapper's SampleNodeAddrs method
		// will then make a further call to the server's BootstrapAddrs
		// method.
		kit.graphConn.On(
			"BootstrapAddrs", mock.Anything, &BootstrapAddrsReq{
				NumAddrs: 5,
				IgnoreNodes: [][]byte{
					node2ID[:],
				},
			}, mock.Anything,
		).Return(&BootstrapAddrsResp{
			Addresses: map[string]*Addresses{
				node1Hex: {
					Addresses: []*lnrpc.NodeAddress{
						testAddr1,
						testAddr2,
					},
				},
			},
		}, nil)

		addrs, err := b.SampleNodeAddrs(
			ctx, 5, map[autopilot.NodeID]struct{}{
				node2ID: {},
			},
		)
		require.NoError(t, err)
		require.EqualValues(t, []*lnwire.NetAddress{
			{
				IdentityKey: node1Pub,
				Address:     addr1,
			},
			{
				IdentityKey: node1Pub,
				Address:     addr2,
			},
		}, addrs)
	})

	t.Run("NetworkStats", func(t *testing.T) {
		kit := newTestKit(t)
		kit.lnConn.On(
			"GetNetworkInfo", mock.Anything,
			&lnrpc.NetworkInfoRequest{},
			mock.Anything,
		).Return(&lnrpc.NetworkInfo{
			GraphDiameter:        1,
			AvgOutDegree:         2.2,
			MaxOutDegree:         3,
			NumNodes:             4,
			NumChannels:          5,
			TotalNetworkCapacity: 6,
			AvgChannelSize:       7,
			MinChannelSize:       8,
			MaxChannelSize:       9,
			MedianChannelSizeSat: 10,
			NumZombieChans:       11,
		}, nil)

		stats, err := kit.client.NetworkStats(ctx)
		require.NoError(t, err)
		require.Equal(t, &models.NetworkStats{
			Diameter:             1,
			MaxChanOut:           3,
			NumNodes:             4,
			NumChannels:          5,
			TotalNetworkCapacity: 6,
			MinChanSize:          8,
			MaxChanSize:          9,
			MedianChanSize:       10,
			NumZombies:           11,
		}, stats)
	})

	t.Run("ForEachNodeDirectedChannel", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: true,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				Features: map[uint32]*lnrpc.Feature{
					0: {Name: "feature1"},
				},
			},
			Channels: []*lnrpc.ChannelEdge{
				{
					ChannelId: 1234,
					ChanPoint: chanPoint1,
					Node1Pub:  node1Hex,
					Node2Pub:  node2Hex,
					Capacity:  50000,
					Node1Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
						CustomRecords: inboundFeeRecord,
					},
				},
				{
					ChannelId: 5678,
					ChanPoint: chanPoint2,
					Node1Pub:  node3Hex,
					Node2Pub:  node1Hex,
					Capacity:  100000,
					Node2Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
					},
				},
			},
		}, nil)

		// Collect all the channels passed to the call-back.
		var collectedChans []*graphdb.DirectedChannel

		err := kit.client.ForEachNodeDirectedChannel(ctx, nil,
			route.NewVertex(node1Pub),
			func(channel *graphdb.DirectedChannel) error {
				// Set the call back to nil so we can do an
				// equals assertion later.
				if channel.InPolicy != nil {
					channel.InPolicy.ToNodePubKey = nil
				}

				collectedChans = append(collectedChans, channel)

				return nil
			},
		)
		require.NoError(t, err)

		// Verify that the call-back was passed all the expected
		// channels.
		require.EqualValues(t, []*graphdb.DirectedChannel{
			{
				ChannelID:    1234,
				IsNode1:      true,
				OtherNode:    route.NewVertex(node2Pub),
				Capacity:     50000,
				OutPolicySet: true,
				InPolicy:     nil,
				InboundFee: lnwire.Fee{
					BaseFee: 10,
					FeeRate: 20,
				},
			},
			{
				ChannelID:    5678,
				IsNode1:      false,
				OtherNode:    route.NewVertex(node3Pub),
				Capacity:     100000,
				OutPolicySet: false,
				InPolicy:     testPolicy,
			},
		}, collectedChans)

		// If the node is not found, no error should be returned.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: true,
			}, mock.Anything,
		).Return(nil, status.Error(codes.NotFound,
			graphdb.ErrGraphNodeNotFound.Error(),
		))

		err = kit.client.ForEachNodeDirectedChannel(
			ctx, nil, route.NewVertex(node2Pub),
			func(channel *graphdb.DirectedChannel) error {
				return nil
			},
		)
		require.NoError(t, err)
	})

	t.Run("FetchNodeFeatures", func(t *testing.T) {
		kit := newTestKit(t)

		// First, we'll test the case where the node is found, and we
		// receive the expected features.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				PubKey: node1Hex,
				Features: map[uint32]*lnrpc.Feature{
					0: {Name: "feature1"},
				},
			},
		}, nil)

		features, err := kit.client.FetchNodeFeatures(
			ctx, nil, route.NewVertex(node1Pub),
		)
		require.NoError(t, err)
		require.Equal(t, testFeature, features)

		// Now we test the case where the node is not found, in which
		// case we should receive an empty feature vector.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, status.Error(codes.NotFound,
			graphdb.ErrEdgeNotFound.Error(),
		))

		features, err = kit.client.FetchNodeFeatures(
			ctx, nil, route.NewVertex(node2Pub),
		)
		require.NoError(t, err)
		require.Equal(t, lnwire.EmptyFeatureVector(), features)
	})

	t.Run("ForEachNode", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"DescribeGraph", mock.Anything,
			&lnrpc.ChannelGraphRequest{IncludeUnannounced: true},
			mock.Anything,
		).Return(&lnrpc.ChannelGraph{
			Nodes: []*lnrpc.LightningNode{
				{
					PubKey: node1Hex,
					Alias:  "Node1",
					Addresses: []*lnrpc.NodeAddress{
						testAddr1,
					},
					LastUpdate: 1,
					Features: make(
						map[uint32]*lnrpc.Feature,
					),
					CustomRecords: map[uint64][]byte{
						1: {1, 3, 4},
					},
				},
				{
					PubKey: node2Hex,
				},
			},
		}, nil)

		// Collect the nodes passed to the call-back.
		var nodes []*models.LightningNode
		err := kit.client.ForEachNode(
			ctx, func(node *models.LightningNode) error {
				nodes = append(nodes, node)

				return nil
			},
		)
		require.NoError(t, err)

		// Verify that the call-back was passed all the expected nodes.
		require.EqualValues(t, []*models.LightningNode{
			{
				PubKeyBytes:          node1ID,
				HaveNodeAnnouncement: true,
				LastUpdate:           time.Unix(1, 0),
				Addresses:            []net.Addr{addr1},
				Color:                color.RGBA{},
				Alias:                "Node1",
				AuthSigBytes:         nil,
				Features: lnwire.NewFeatureVector(
					nil, map[lnwire.FeatureBit]string{},
				),
				ExtraOpaqueData: []byte{1, 3, 1, 3, 4},
			},
			{
				PubKeyBytes:          node2ID,
				HaveNodeAnnouncement: false,
				LastUpdate:           time.Unix(0, 0),
				Features: lnwire.NewFeatureVector(
					nil, map[lnwire.FeatureBit]string{},
				),
				Addresses: make([]net.Addr, 0),
			},
		}, nodes)
	})

	t.Run("ForEachChannel", func(t *testing.T) {
		kit := newTestKit(t)
		kit.lnConn.On(
			"DescribeGraph", mock.Anything,
			&lnrpc.ChannelGraphRequest{IncludeUnannounced: true},
			mock.Anything,
		).Return(&lnrpc.ChannelGraph{
			Edges: []*lnrpc.ChannelEdge{
				{
					ChannelId: 1,
					ChanPoint: chanPoint1,
					Node1Pub:  node1Hex,
					Node2Pub:  node2Hex,
					Capacity:  300,
					Node1Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
						CustomRecords: inboundFeeRecord,
					},
					Announced: false,
				},
				{
					ChannelId: 2,
					ChanPoint: chanPoint2,
					Node1Pub:  node3Hex,
					Node2Pub:  node2Hex,
					Capacity:  500,
					Node2Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
					},
					Announced: true,
				},
			},
		}, nil)

		type chanInfo struct {
			info    *models.ChannelEdgeInfo
			policy1 *models.ChannelEdgePolicy
			policy2 *models.ChannelEdgePolicy
		}

		// Collect the channels passed to the call-back.
		var chans []*chanInfo
		err := kit.client.ForEachChannel(
			ctx, func(info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error {

				chans = append(chans, &chanInfo{
					info:    info,
					policy1: policy,
					policy2: policy2,
				})

				return nil
			},
		)
		require.NoError(t, err)

		// Verify that the call-back was passed all the expected
		// channels.
		require.EqualValues(t, []*chanInfo{
			{
				info: &models.ChannelEdgeInfo{
					ChannelID:     1,
					NodeKey1Bytes: node1ID,
					NodeKey2Bytes: node2ID,
					ChannelPoint:  testChanPoint1,
					Capacity:      300,
				},
				policy1: &models.ChannelEdgePolicy{
					ChannelID:     1,
					TimeLockDelta: 4,
					ToNode:        node2ID,
					ExtraOpaqueData: []byte{
						253, 217, 3, 8, 0, 0, 0, 10, 0,
						0, 0, 20,
					},
					LastUpdate: time.Unix(0, 0),
				},
			},
			{
				info: &models.ChannelEdgeInfo{
					ChannelID:     2,
					NodeKey1Bytes: node3ID,
					NodeKey2Bytes: node2ID,
					ChannelPoint:  testChanPoint2,
					Capacity:      500,
					AuthProof:     nonNilProof,
				},
				policy2: &models.ChannelEdgePolicy{
					ChannelID:     2,
					TimeLockDelta: 4,
					ToNode:        node3ID,
					ChannelFlags:  direction,
					LastUpdate:    time.Unix(0, 0),
				},
			},
		}, chans)
	})

	t.Run("ForEachNodeChannel", func(t *testing.T) {
		kit := newTestKit(t)
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything,
			&lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: true,
			},
			mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Channels: []*lnrpc.ChannelEdge{
				{
					ChannelId: 1,
					ChanPoint: chanPoint1,
					Node1Pub:  node1Hex,
					Node2Pub:  node2Hex,
					Capacity:  300,
					Node1Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
						CustomRecords: inboundFeeRecord,
					},
					Announced: false,
				},
				{
					ChannelId: 2,
					ChanPoint: chanPoint2,
					Node1Pub:  node3Hex,
					Node2Pub:  node1Hex,
					Capacity:  500,
					Node2Policy: &lnrpc.RoutingPolicy{
						TimeLockDelta: 4,
					},
					Announced: true,
				},
			},
		}, nil)

		type chanInfo struct {
			info    *models.ChannelEdgeInfo
			policy1 *models.ChannelEdgePolicy
			policy2 *models.ChannelEdgePolicy
		}

		// Collect the channels passed to the call-back.
		var chans []*chanInfo
		err := kit.client.ForEachNodeChannel(ctx, route.Vertex(node1ID),
			func(info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error {

				chans = append(chans, &chanInfo{
					info:    info,
					policy1: policy,
					policy2: policy2,
				})

				return nil
			},
		)
		require.NoError(t, err)

		// Verify that the call-back was passed all the expected
		// channels.
		require.EqualValues(t, []*chanInfo{
			{
				info: &models.ChannelEdgeInfo{
					ChannelID:     1,
					NodeKey1Bytes: node1ID,
					NodeKey2Bytes: node2ID,
					ChannelPoint:  testChanPoint1,
					Capacity:      300,
				},
				policy1: &models.ChannelEdgePolicy{
					ChannelID:     1,
					TimeLockDelta: 4,
					ToNode:        node2ID,
					ExtraOpaqueData: []byte{
						253, 217, 3, 8, 0, 0, 0, 10, 0,
						0, 0, 20,
					},
					LastUpdate: time.Unix(0, 0),
				},
			},
			{
				info: &models.ChannelEdgeInfo{
					ChannelID:     2,
					NodeKey1Bytes: node3ID,
					NodeKey2Bytes: node1ID,
					ChannelPoint:  testChanPoint2,
					Capacity:      500,
					AuthProof:     nonNilProof,
				},
				policy2: &models.ChannelEdgePolicy{
					ChannelID:     2,
					TimeLockDelta: 4,
					ToNode:        node3ID,
					ChannelFlags:  direction,
					LastUpdate:    time.Unix(0, 0),
				},
			},
		}, chans)

		// If the node is not found, no error should be returned.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything,
			&lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: true,
			},
			mock.Anything,
		).Return(nil, status.Error(
			codes.NotFound,
			graphdb.ErrGraphNodeNotFound.Error(),
		))

		err = kit.client.ForEachNodeChannel(ctx, route.Vertex(node2ID),
			func(info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error {

				return nil
			},
		)
		require.NoError(t, err)
	})

	t.Run("FetchLightningNode", func(t *testing.T) {
		kit := newTestKit(t)
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				PubKey:     node1Hex,
				Alias:      "Node1",
				Addresses:  []*lnrpc.NodeAddress{testAddr1},
				LastUpdate: 1,
				Features:   make(map[uint32]*lnrpc.Feature),
				CustomRecords: map[uint64][]byte{
					1: {1, 3, 4},
				},
			},
			IsPublic: true,
		}, nil)

		node, err := kit.client.FetchLightningNode(
			ctx, route.NewVertex(node1Pub),
		)
		require.NoError(t, err)
		require.Equal(t, &models.LightningNode{
			PubKeyBytes:          node1ID,
			HaveNodeAnnouncement: true,
			LastUpdate:           time.Unix(1, 0),
			Addresses: []net.Addr{
				addr1,
			},
			Color: color.RGBA{},
			Alias: "Node1",
			Features: lnwire.NewFeatureVector(
				nil, map[lnwire.FeatureBit]string{},
			),
			ExtraOpaqueData: []byte{1, 3, 1, 3, 4},
		}, node)

		// If the node is not found via the GetNodeInfo call, then
		// graphdb.ErrGraphNodeNotFound must be returned.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node3Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(
			nil, status.Error(codes.NotFound, "not found"),
		)

		_, err = kit.client.FetchLightningNode(
			ctx, route.NewVertex(node3Pub),
		)
		require.ErrorIs(t, err, graphdb.ErrGraphNodeNotFound)
	})

	t.Run("FetchChannelEdgesByOutpoint", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"GetChanInfo", mock.Anything, &lnrpc.ChanInfoRequest{
				ChanPoint: testChanPoint2.String(),
			}, mock.Anything,
		).Return(&lnrpc.ChannelEdge{
			ChannelId: 1,
			ChanPoint: chanPoint1,
			Node1Pub:  node1Hex,
			Node2Pub:  node2Hex,
			Capacity:  300,
			Node1Policy: &lnrpc.RoutingPolicy{
				TimeLockDelta: 4,
				CustomRecords: inboundFeeRecord,
			},
			Announced: false,
		}, nil)

		edge, policy1, policy2, err := kit.client.FetchChannelEdgesByOutpoint( //nolint:lll
			ctx, &testChanPoint2,
		)
		require.NoError(t, err)
		require.Equal(t, &models.ChannelEdgeInfo{
			ChannelID:     1,
			NodeKey1Bytes: node1ID,
			NodeKey2Bytes: node2ID,
			ChannelPoint:  testChanPoint1,
			Capacity:      300,
		}, edge)
		require.Equal(t, &models.ChannelEdgePolicy{
			ChannelID:     1,
			TimeLockDelta: 4,
			ToNode:        node2ID,
			ExtraOpaqueData: []byte{
				253, 217, 3, 8, 0, 0, 0, 10, 0, 0, 0, 20,
			},
			LastUpdate: time.Unix(0, 0),
		}, policy1)
		require.Nil(t, policy2)

		// Test that the correct error is returned if GetChanInfo
		// returns a not-found status.
		kit.lnConn.On(
			"GetChanInfo", mock.Anything, &lnrpc.ChanInfoRequest{
				ChanPoint: testChanPoint1.String(),
			}, mock.Anything,
		).Return(nil, status.Error(codes.NotFound, "not found"))

		_, _, _, err = kit.client.FetchChannelEdgesByOutpoint(
			ctx, &testChanPoint1,
		)
		require.ErrorIs(t, err, graphdb.ErrEdgeNotFound)
	})

	t.Run("FetchChannelEdgesByID", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"GetChanInfo", mock.Anything, &lnrpc.ChanInfoRequest{
				ChanId: 1,
			}, mock.Anything,
		).Return(&lnrpc.ChannelEdge{
			ChannelId: 1,
			ChanPoint: chanPoint1,
			Node1Pub:  node1Hex,
			Node2Pub:  node2Hex,
			Capacity:  300,
			Node1Policy: &lnrpc.RoutingPolicy{
				TimeLockDelta: 4,
				CustomRecords: inboundFeeRecord,
			},
			Announced: false,
		}, nil)

		edge, policy1, policy2, err := kit.client.FetchChannelEdgesByID(
			ctx, 1,
		)
		require.NoError(t, err)
		require.Equal(t, &models.ChannelEdgeInfo{
			ChannelID:     1,
			NodeKey1Bytes: node1ID,
			NodeKey2Bytes: node2ID,
			ChannelPoint:  testChanPoint1,
			Capacity:      300,
		}, edge)
		require.Equal(t, &models.ChannelEdgePolicy{
			ChannelID:     1,
			TimeLockDelta: 4,
			ToNode:        node2ID,
			ExtraOpaqueData: []byte{
				253, 217, 3, 8, 0, 0, 0, 10, 0, 0, 0, 20,
			},
			LastUpdate: time.Unix(0, 0),
		}, policy1)
		require.Nil(t, policy2)
	})

	t.Run("IsPublicNode", func(t *testing.T) {
		kit := newTestKit(t)

		// Node is known and public.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			IsPublic: true,
		}, nil)

		isPublic, err := kit.client.IsPublicNode(ctx, node1ID)
		require.NoError(t, err)
		require.True(t, isPublic)

		// Node is known and not public.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			IsPublic: false,
		}, nil)

		isPublic, err = kit.client.IsPublicNode(ctx, node2ID)
		require.NoError(t, err)
		require.False(t, isPublic)

		// The node is unknown.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node3Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(
			nil, status.Error(codes.NotFound, "not found"),
		)

		// We expect the graphdb.ErrGraphNodeNotFound error.
		_, err = kit.client.IsPublicNode(ctx, node3ID)
		require.ErrorIs(t, err, graphdb.ErrGraphNodeNotFound)

		// Some other GetNodeInfo error is returned.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node4Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, fmt.Errorf("other error"))

		_, err = kit.client.IsPublicNode(ctx, node4ID)
		require.ErrorContains(t, err, "other error")
	})

	t.Run("AddrsForNode", func(t *testing.T) {
		kit := newTestKit(t)

		//  Node is known and has addresses.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				PubKey: node1Hex,
				Alias:  "Node1",
				Addresses: []*lnrpc.NodeAddress{
					testAddr1,
					testAddr2,
				},
			},
		}, nil)

		found, addrs, err := kit.client.AddrsForNode(ctx, node1Pub)
		require.NoError(t, err)
		require.True(t, found)
		require.EqualValues(t, []net.Addr{addr1, addr2}, addrs)

		// Node is unknown.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, status.Error(codes.NotFound, "not found"))

		found, addrs, err = kit.client.AddrsForNode(ctx, node2Pub)
		require.NoError(t, err)
		require.False(t, found)
		require.Nil(t, addrs)

		// Some other GetNodeInfo error is returned.
		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node3Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, fmt.Errorf("other error"))

		found, _, err = kit.client.AddrsForNode(ctx, node3Pub)
		require.Error(t, err)
		require.False(t, found)
	})

	t.Run("HasLightningNode", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				PubKey:     node1Hex,
				Alias:      "Node1",
				LastUpdate: 1,
			},
		}, nil)

		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node2Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, status.Error(codes.NotFound, "not found"))

		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node3Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(nil, fmt.Errorf("other error"))

		lastUpdate, found, err := kit.client.HasLightningNode(
			ctx, node1ID,
		)
		require.NoError(t, err)
		require.True(t, found)
		require.EqualValues(t, time.Unix(1, 0), lastUpdate)

		lastUpdate, found, err = kit.client.HasLightningNode(
			ctx, node2ID,
		)
		require.NoError(t, err)
		require.False(t, found)
		require.EqualValues(t, time.Time{}, lastUpdate)

		_, found, err = kit.client.HasLightningNode(ctx, node3ID)
		require.ErrorContains(t, err, "other error")
		require.False(t, found)
	})

	t.Run("LookupAlias", func(t *testing.T) {
		kit := newTestKit(t)

		kit.lnConn.On(
			"GetNodeInfo", mock.Anything, &lnrpc.NodeInfoRequest{
				PubKey:          node1Hex,
				IncludeChannels: false,
			}, mock.Anything,
		).Return(&lnrpc.NodeInfo{
			Node: &lnrpc.LightningNode{
				Alias: "Node1",
			},
		}, nil)

		alias, err := kit.client.LookupAlias(ctx, node1Pub)
		require.NoError(t, err)
		require.Equal(t, "Node1", alias)
	})
}

func genPub(t *testing.T) (*btcec.PublicKey, autopilot.NodeID, string) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pub := priv.PubKey()

	return pub, autopilot.NewNodeID(pub),
		hex.EncodeToString(pub.SerializeCompressed())
}

type testKit struct {
	lnConn    *lnrpc.MockLNConn
	graphConn *MockGraphClient
	client    *Client
}

func newTestKit(t *testing.T) *testKit {
	var (
		lnConn    lnrpc.MockLNConn
		graphConn MockGraphClient
	)
	t.Cleanup(func() {
		lnConn.AssertExpectations(t)
		graphConn.AssertExpectations(t)
	})

	return &testKit{
		lnConn:    &lnConn,
		graphConn: &graphConn,
		client: &Client{
			graphConn:      &graphConn,
			lnConn:         &lnConn,
			resolveTCPAddr: net.ResolveTCPAddr,
		},
	}
}
