package sources

import (
	"context"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/discovery"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/graph/session"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
)

type MockGraphSource struct {
	mock.Mock
}

func (m *MockGraphSource) NewPathFindTx(ctx context.Context) (session.RTx,
	error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(session.RTx), args.Error(1)
}

func (m *MockGraphSource) ForEachNodeDirectedChannel(ctx context.Context,
	tx session.RTx, node route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error) error {

	return m.Called(ctx, tx, node, cb).Error(0)
}

func (m *MockGraphSource) FetchNodeFeatures(ctx context.Context, tx session.RTx,
	node route.Vertex) (*lnwire.FeatureVector, error) {

	args := m.Called(ctx, tx, node)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*lnwire.FeatureVector), args.Error(1)
}

func (m *MockGraphSource) FetchChannelEdgesByID(ctx context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	args := m.Called(ctx, chanID)

	var (
		ret1 *models.ChannelEdgeInfo
		ret2 *models.ChannelEdgePolicy
		ret3 *models.ChannelEdgePolicy
	)

	if args.Get(0) != nil {
		ret1 = args.Get(0).(*models.ChannelEdgeInfo)
	}

	if args.Get(1) != nil {
		ret2 = args.Get(1).(*models.ChannelEdgePolicy)
	}

	if args.Get(2) != nil {
		ret3 = args.Get(2).(*models.ChannelEdgePolicy)
	}

	return ret1, ret2, ret3, args.Error(3)
}

func (m *MockGraphSource) IsPublicNode(ctx context.Context,
	pubKey [33]byte) (bool, error) {

	args := m.Called(ctx, pubKey)
	return args.Bool(0), args.Error(1)
}

func (m *MockGraphSource) FetchChannelEdgesByOutpoint(ctx context.Context,
	point *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	var (
		ret1 *models.ChannelEdgeInfo
		ret2 *models.ChannelEdgePolicy
		ret3 *models.ChannelEdgePolicy
	)

	args := m.Called(ctx, point)

	if args.Get(0) != nil {
		ret1 = args.Get(0).(*models.ChannelEdgeInfo)
	}

	if args.Get(1) != nil {
		ret2 = args.Get(1).(*models.ChannelEdgePolicy)
	}

	if args.Get(2) != nil {
		ret3 = args.Get(2).(*models.ChannelEdgePolicy)
	}

	return ret1, ret2, ret3, args.Error(3)
}

func (m *MockGraphSource) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	args := m.Called(ctx, nodePub)
	if args.Get(1) == nil {
		return args.Bool(0), nil, args.Error(2)
	}

	return args.Bool(0), args.Get(1).([]net.Addr), args.Error(2)
}

func (m *MockGraphSource) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	return m.Called(ctx, cb).Error(0)
}

func (m *MockGraphSource) ForEachNode(ctx context.Context,
	cb func(*models.LightningNode) error) error {

	return m.Called(ctx, cb).Error(0)
}

func (m *MockGraphSource) HasLightningNode(ctx context.Context,
	nodePub [33]byte) (time.Time, bool, error) {

	args := m.Called(ctx, nodePub)

	return args.Get(0).(time.Time), args.Bool(1), args.Error(2)
}

func (m *MockGraphSource) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	args := m.Called(ctx, pub)

	return args.String(0), args.Error(1)
}

func (m *MockGraphSource) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	return m.Called(ctx, nodePub, cb).Error(0)
}

func (m *MockGraphSource) FetchLightningNode(ctx context.Context,
	nodePub route.Vertex) (*models.LightningNode, error) {

	args := m.Called(ctx, nodePub)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*models.LightningNode), args.Error(1)
}

func (m *MockGraphSource) GraphBootstrapper(ctx context.Context) (
	discovery.NetworkPeerBootstrapper, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(discovery.NetworkPeerBootstrapper), args.Error(1)
}

func (m *MockGraphSource) NetworkStats(ctx context.Context) (
	*models.NetworkStats, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*models.NetworkStats), args.Error(1)
}

func (m *MockGraphSource) BetweennessCentrality(ctx context.Context) (
	map[autopilot.NodeID]*models.BetweennessCentrality, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(map[autopilot.NodeID]*models.BetweennessCentrality),
		args.Error(1)
}

var _ GraphSource = (*MockGraphSource)(nil)
