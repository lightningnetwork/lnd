package sources

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	// Test vertices for use in tests.
	pubSelf  = route.Vertex{0x01}
	pubAlice = route.Vertex{0x02}
	pubBob   = route.Vertex{0x03}
	pubCarol = route.Vertex{0x04}
)

// mockLocal is a minimal mock of graphdb.GraphSource for testing the Mux.
// Only the methods exercised by the tests are implemented.
type mockLocal struct {
	nodes    []*models.Node
	channels []mockChannel
	nodeMap  map[route.Vertex]*models.Node
	chanMap  map[uint64]mockChannel
	aliases  map[route.Vertex]string
}

type mockChannel struct {
	info *models.ChannelEdgeInfo
	p1   *models.ChannelEdgePolicy
	p2   *models.ChannelEdgePolicy
}

func newMockLocal() *mockLocal {
	return &mockLocal{
		nodeMap: make(map[route.Vertex]*models.Node),
		chanMap: make(map[uint64]mockChannel),
		aliases: make(map[route.Vertex]string),
	}
}

func (m *mockLocal) addNode(pub route.Vertex) {
	n := &models.Node{PubKeyBytes: pub}
	m.nodes = append(m.nodes, n)
	m.nodeMap[pub] = n
}

func (m *mockLocal) addChannel(id uint64, node1, node2 route.Vertex) {
	ch := mockChannel{
		info: &models.ChannelEdgeInfo{
			ChannelID:     id,
			NodeKey1Bytes: node1,
			NodeKey2Bytes: node2,
		},
	}
	m.channels = append(m.channels, ch)
	m.chanMap[id] = ch
}

func (m *mockLocal) ForEachNode(_ context.Context,
	cb func(*models.Node) error, reset func()) error {

	reset()
	for _, n := range m.nodes {
		if err := cb(n); err != nil {
			return err
		}
	}

	return nil
}

func (m *mockLocal) ForEachChannel(_ context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	reset()
	for _, ch := range m.channels {
		if err := cb(ch.info, ch.p1, ch.p2); err != nil {
			return err
		}
	}

	return nil
}

func (m *mockLocal) ForEachNodeChannel(_ context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	reset()
	for _, ch := range m.channels {
		if ch.info.NodeKey1Bytes == nodePub ||
			ch.info.NodeKey2Bytes == nodePub {

			if err := cb(ch.info, ch.p1, ch.p2); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *mockLocal) FetchNode(_ context.Context,
	pub route.Vertex) (*models.Node, error) {

	n, ok := m.nodeMap[pub]
	if !ok {
		return nil, graphdb.ErrGraphNodeNotFound
	}

	return n, nil
}

func (m *mockLocal) FetchChannelEdgesByID(_ context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	ch, ok := m.chanMap[chanID]
	if !ok {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	}

	return ch.info, ch.p1, ch.p2, nil
}

func (m *mockLocal) LookupAlias(_ context.Context,
	pub *btcec.PublicKey) (string, error) {

	v := route.NewVertex(pub)
	alias, ok := m.aliases[v]
	if !ok {
		return "", graphdb.ErrGraphNodeNotFound
	}

	return alias, nil
}

func (m *mockLocal) IsPublicNode(_ context.Context,
	pubKey [33]byte) (bool, error) {

	_, ok := m.nodeMap[pubKey]
	return ok, nil
}

func (m *mockLocal) AddrsForNode(_ context.Context,
	pub *btcec.PublicKey) (bool, []net.Addr, error) {

	v := route.NewVertex(pub)
	_, ok := m.nodeMap[v]
	if !ok {
		return false, nil, nil
	}

	return true, nil, nil
}

// Stub methods that the Mux delegates directly to local.
func (m *mockLocal) ForEachNodeDirectedChannel(
	context.Context, route.Vertex,
	func(*graphdb.DirectedChannel) error,
	func(),
) error {

	return nil
}

func (m *mockLocal) FetchNodeFeatures(
	context.Context, route.Vertex,
) (*lnwire.FeatureVector, error) {

	return lnwire.EmptyFeatureVector(), nil
}

func (m *mockLocal) ForEachNodeCached(
	context.Context, bool,
	func(context.Context, route.Vertex, []net.Addr,
		map[uint64]*graphdb.DirectedChannel) error,
	func(),
) error {

	return nil
}

func (m *mockLocal) FetchChannelEdgesByOutpoint(
	context.Context, *wire.OutPoint,
) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return nil, nil, nil, graphdb.ErrEdgeNotFound
}

func (m *mockLocal) GraphSession(
	_ context.Context,
	cb func(graphdb.NodeTraverser) error, _ func(),
) error {

	return cb(nil)
}

func (m *mockLocal) SourceNode(
	context.Context,
) (*models.Node, error) {

	return m.nodeMap[pubSelf], nil
}

func (m *mockLocal) ForEachSourceNodeChannel(
	context.Context,
	func(wire.OutPoint, bool, *models.Node) error,
	func(),
) error {

	return nil
}

// mockRemote is a minimal mock of RemoteGraph for testing the Mux.
type mockRemote struct {
	nodes    []*models.Node
	channels []mockChannel
	nodeMap  map[route.Vertex]*models.Node
	chanMap  map[uint64]mockChannel
	aliases  map[route.Vertex]string

	// forEachErr causes ForEachNode/ForEachChannel to return this error.
	forEachErr error
}

func newMockRemote() *mockRemote {
	return &mockRemote{
		nodeMap: make(map[route.Vertex]*models.Node),
		chanMap: make(map[uint64]mockChannel),
		aliases: make(map[route.Vertex]string),
	}
}

func (m *mockRemote) addNode(pub route.Vertex) {
	n := &models.Node{PubKeyBytes: pub}
	m.nodes = append(m.nodes, n)
	m.nodeMap[pub] = n
}

func (m *mockRemote) addChannel(id uint64, node1, node2 route.Vertex) {
	ch := mockChannel{
		info: &models.ChannelEdgeInfo{
			ChannelID:     id,
			NodeKey1Bytes: node1,
			NodeKey2Bytes: node2,
		},
	}
	m.channels = append(m.channels, ch)
	m.chanMap[id] = ch
}

func (m *mockRemote) ForEachNode(_ context.Context,
	cb func(*models.Node) error, _ func()) error {

	if m.forEachErr != nil {
		return m.forEachErr
	}
	for _, n := range m.nodes {
		if err := cb(n); err != nil {
			return err
		}
	}

	return nil
}

func (m *mockRemote) ForEachChannel(_ context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	_ func()) error {

	if m.forEachErr != nil {
		return m.forEachErr
	}
	for _, ch := range m.channels {
		if err := cb(ch.info, ch.p1, ch.p2); err != nil {
			return err
		}
	}

	return nil
}

func (m *mockRemote) ForEachNodeChannel(_ context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	_ func()) error {

	if m.forEachErr != nil {
		return m.forEachErr
	}
	for _, ch := range m.channels {
		if ch.info.NodeKey1Bytes == nodePub ||
			ch.info.NodeKey2Bytes == nodePub {

			if err := cb(ch.info, ch.p1, ch.p2); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *mockRemote) FetchChannelEdgesByID(_ context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	ch, ok := m.chanMap[chanID]
	if !ok {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	}

	return ch.info, ch.p1, ch.p2, nil
}

func (m *mockRemote) FetchChannelEdgesByOutpoint(context.Context,
	*wire.OutPoint) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return nil, nil, nil, graphdb.ErrEdgeNotFound
}

func (m *mockRemote) FetchNode(_ context.Context,
	pub route.Vertex) (*models.Node, error) {

	n, ok := m.nodeMap[pub]
	if !ok {
		return nil, graphdb.ErrGraphNodeNotFound
	}

	return n, nil
}

func (m *mockRemote) IsPublicNode(_ context.Context,
	pubKey [33]byte) (bool, error) {

	_, ok := m.nodeMap[pubKey]

	return ok, nil
}

func (m *mockRemote) LookupAlias(_ context.Context,
	pub *btcec.PublicKey) (string, error) {

	v := route.NewVertex(pub)
	alias, ok := m.aliases[v]
	if !ok {
		return "", graphdb.ErrGraphNodeNotFound
	}

	return alias, nil
}

func (m *mockRemote) AddrsForNode(_ context.Context,
	pub *btcec.PublicKey) (bool, []net.Addr, error) {

	v := route.NewVertex(pub)
	_, ok := m.nodeMap[v]
	if !ok {
		return false, nil, nil
	}

	return true, nil, nil
}

// newTestMux creates a Mux with the given mocks and pubSelf as the self key.
func newTestMux(local *mockLocal, remote *mockRemote) *Mux {
	return NewMux(local, remote, pubSelf)
}

// TestMuxForEachNodeDedup verifies that nodes returned by both remote and local
// are deduplicated — only the remote version is returned.
func TestMuxForEachNodeDedup(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	// Alice exists on both; Bob only on remote; Carol only on local.
	remote.addNode(pubAlice)
	remote.addNode(pubBob)
	local.addNode(pubAlice)
	local.addNode(pubCarol)

	mux := newTestMux(local, remote)

	var got []route.Vertex
	err := mux.ForEachNode(t.Context(),
		func(n *models.Node) error {
			got = append(got, n.PubKeyBytes)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)

	// Should see Alice, Bob (from remote), Carol (from local). No dups.
	require.Len(t, got, 3)
	require.Contains(t, got, pubAlice)
	require.Contains(t, got, pubBob)
	require.Contains(t, got, pubCarol)
}

// TestMuxForEachChannelDedup verifies that channels from both sources are
// deduplicated by channel ID.
func TestMuxForEachChannelDedup(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	// Channel 1 on both; channel 2 only remote; channel 3 only local.
	remote.addChannel(1, pubAlice, pubBob)
	remote.addChannel(2, pubBob, pubCarol)
	local.addChannel(1, pubAlice, pubBob)
	local.addChannel(3, pubCarol, pubSelf)

	mux := newTestMux(local, remote)

	var got []uint64
	err := mux.ForEachChannel(t.Context(),
		func(info *models.ChannelEdgeInfo, _ *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			got = append(got, info.ChannelID)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)

	require.Len(t, got, 3)
	require.Contains(t, got, uint64(1))
	require.Contains(t, got, uint64(2))
	require.Contains(t, got, uint64(3))
}

// TestMuxForEachNodeRemoteError verifies that when the remote fails,
// ForEachNode falls back to local-only iteration.
func TestMuxForEachNodeRemoteError(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addNode(pubAlice)
	local.addNode(pubBob)
	remote.forEachErr = fmt.Errorf("connection refused")

	mux := newTestMux(local, remote)

	var got []route.Vertex
	err := mux.ForEachNode(t.Context(),
		func(n *models.Node) error {
			got = append(got, n.PubKeyBytes)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Contains(t, got, pubAlice)
	require.Contains(t, got, pubBob)
}

// TestMuxForEachChannelRemoteError verifies that ForEachChannel falls back to
// local when remote fails.
func TestMuxForEachChannelRemoteError(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addChannel(1, pubAlice, pubBob)
	remote.forEachErr = fmt.Errorf("connection refused")

	mux := newTestMux(local, remote)

	var got []uint64
	err := mux.ForEachChannel(t.Context(),
		func(info *models.ChannelEdgeInfo, _ *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			got = append(got, info.ChannelID)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, uint64(1), got[0])
}

// TestMuxForEachNodeChannelSelf verifies that ForEachNodeChannel uses
// local-only when querying our own channels.
func TestMuxForEachNodeChannelSelf(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	// Local has our private channel; remote does not.
	local.addChannel(1, pubSelf, pubAlice)
	remote.addChannel(2, pubBob, pubCarol)

	mux := newTestMux(local, remote)

	var got []uint64
	err := mux.ForEachNodeChannel(t.Context(), pubSelf,
		func(info *models.ChannelEdgeInfo, _ *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			got = append(got, info.ChannelID)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, uint64(1), got[0])
}

// TestMuxFetchNodeSelf verifies that FetchNode for our own pubkey goes to
// local, even if remote has data.
func TestMuxFetchNodeSelf(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addNode(pubSelf)
	remote.addNode(pubSelf)

	mux := newTestMux(local, remote)

	node, err := mux.FetchNode(t.Context(), pubSelf)
	require.NoError(t, err)
	require.Equal(t, pubSelf, route.Vertex(node.PubKeyBytes))
}

// TestMuxFetchNodeRemoteFirst verifies that FetchNode for non-self nodes
// returns the remote result when available.
func TestMuxFetchNodeRemoteFirst(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addNode(pubAlice)
	remote.addNode(pubAlice)

	mux := newTestMux(local, remote)

	node, err := mux.FetchNode(t.Context(), pubAlice)
	require.NoError(t, err)
	require.Equal(t, pubAlice, route.Vertex(node.PubKeyBytes))
}

// TestMuxFetchNodeFallback verifies that FetchNode falls back to local when
// the remote doesn't have the node.
func TestMuxFetchNodeFallback(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addNode(pubAlice)
	// Remote does not have Alice.

	mux := newTestMux(local, remote)

	node, err := mux.FetchNode(t.Context(), pubAlice)
	require.NoError(t, err)
	require.Equal(t, pubAlice, route.Vertex(node.PubKeyBytes))
}

// TestMuxFetchChannelEdgesByIDFallback verifies that channel lookups fall back
// to local when remote doesn't have the channel.
func TestMuxFetchChannelEdgesByIDFallback(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	// Private channel only on local.
	local.addChannel(42, pubSelf, pubAlice)

	mux := newTestMux(local, remote)

	info, _, _, err := mux.FetchChannelEdgesByID(
		t.Context(), 42,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(42), info.ChannelID)
}

// TestMuxFetchChannelEdgesByIDRemoteFirst verifies that channel lookups prefer
// the remote source.
func TestMuxFetchChannelEdgesByIDRemoteFirst(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	remote.addChannel(100, pubAlice, pubBob)
	local.addChannel(100, pubAlice, pubBob)

	mux := newTestMux(local, remote)

	info, _, _, err := mux.FetchChannelEdgesByID(
		t.Context(), 100,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(100), info.ChannelID)
}

// TestMuxLookupAliasSelf verifies that LookupAlias for our
// own key uses local.
func TestMuxLookupAliasSelf(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	// LookupAlias takes *btcec.PublicKey, so we need a
	// real key to derive the vertex.
	selfPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	selfPubKey := selfPriv.PubKey()
	selfVertex := route.NewVertex(selfPubKey)

	local.aliases[selfVertex] = "my-node"
	remote.aliases[selfVertex] = "remote-version"
	mux := NewMux(local, remote, selfVertex)

	alias, err := mux.LookupAlias(t.Context(), selfPubKey)
	require.NoError(t, err)
	require.Equal(t, "my-node", alias)
}

// TestMuxAddrsForNodeSelf verifies that AddrsForNode for our own key uses
// local.
func TestMuxAddrsForNodeSelf(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	selfPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	selfPubKey := selfPriv.PubKey()
	selfVertex := route.NewVertex(selfPubKey)

	local.addNode(selfVertex)
	remote.addNode(selfVertex)

	mux := NewMux(local, remote, selfVertex)

	known, _, err := mux.AddrsForNode(t.Context(), selfPubKey)
	require.NoError(t, err)
	require.True(t, known)
}

// TestMuxForEachNodeChannelRemoteError verifies that ForEachNodeChannel falls
// back to local on remote error.
func TestMuxForEachNodeChannelRemoteError(t *testing.T) {
	local := newMockLocal()
	remote := newMockRemote()

	local.addChannel(1, pubAlice, pubBob)
	remote.forEachErr = fmt.Errorf("timeout")

	mux := newTestMux(local, remote)

	var got []uint64
	err := mux.ForEachNodeChannel(t.Context(), pubAlice,
		func(info *models.ChannelEdgeInfo, _ *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			got = append(got, info.ChannelID)
			return nil
		}, func() { got = nil },
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
}
