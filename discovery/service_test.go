package discovery

import (
	"fmt"
	"net"
	"sync"

	prand "math/rand"

	"testing"

	"math/big"

	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	testAddrs    = []net.Addr{testAddr}
	testFeatures = lnwire.NewFeatureVector([]lnwire.Feature{})
	testSig      = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)
)

type mockGraphSource struct {
	nodes      []*channeldb.LightningNode
	edges      []*channeldb.ChannelEdgeInfo
	updates    []*channeldb.ChannelEdgePolicy
	bestHeight uint32
}

func newMockRouter(height uint32) *mockGraphSource {
	return &mockGraphSource{
		bestHeight: height,
	}
}

var _ routing.ChannelGraphSource = (*mockGraphSource)(nil)

func (r *mockGraphSource) AddNode(node *channeldb.LightningNode) error {
	r.nodes = append(r.nodes, node)
	return nil
}

func (r *mockGraphSource) AddEdge(edge *channeldb.ChannelEdgeInfo) error {
	r.edges = append(r.edges, edge)
	return nil
}

func (r *mockGraphSource) UpdateEdge(policy *channeldb.ChannelEdgePolicy) error {
	r.updates = append(r.updates, policy)
	return nil
}

func (r *mockGraphSource) AddProof(chanID uint8,
	proof *channeldb.ChannelAuthProof) error {
	return nil
}

func (r *mockGraphSource) SelfEdges() ([]*channeldb.ChannelEdgePolicy, error) {
	return nil, nil
}

func (r *mockGraphSource) CurrentBlockHeight() (uint32, error) {
	return r.bestHeight, nil
}

func (r *mockGraphSource) ForEachNode(func(node *channeldb.LightningNode) error) error {
	return nil
}

func (r *mockGraphSource) ForAllOutgoingChannels(cb func(c *channeldb.ChannelEdgePolicy) error) error {
	return nil
}

func (r *mockGraphSource) ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {
	return nil
}

type mockNotifier struct {
	clientCounter uint32
	epochClients  map[uint32]chan *chainntnfs.BlockEpoch

	sync.RWMutex
}

func newMockNotifier() *mockNotifier {
	return &mockNotifier{
		epochClients: make(map[uint32]chan *chainntnfs.BlockEpoch),
	}
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs uint32) (*chainntnfs.ConfirmationEvent, error) {

	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint) (*chainntnfs.SpendEvent, error) {
	return nil, nil
}

func (m *mockNotifier) notifyBlock(hash chainhash.Hash, height uint32) {
	m.RLock()
	defer m.RUnlock()

	for _, client := range m.epochClients {
		client <- &chainntnfs.BlockEpoch{
			Height: int32(height),
			Hash:   &hash,
		}
	}
}

func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	m.RLock()
	defer m.RUnlock()

	epochChan := make(chan *chainntnfs.BlockEpoch)
	clientID := m.clientCounter
	m.clientCounter++
	m.epochClients[clientID] = epochChan

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

func createNodeAnnouncement() (*lnwire.NodeAnnouncement,
	error) {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	pub := priv.PubKey().SerializeCompressed()

	alias, err := lnwire.NewAlias("kek" + string(pub[:]))
	if err != nil {
		return nil, err
	}

	return &lnwire.NodeAnnouncement{
		Timestamp: uint32(prand.Int31()),
		Addresses: testAddrs,
		NodeID:    priv.PubKey(),
		Alias:     alias,
		Features:  testFeatures,
	}, nil
}

func createUpdateAnnouncement(blockHeight uint32) *lnwire.ChannelUpdateAnnouncement {
	return &lnwire.ChannelUpdateAnnouncement{
		Signature: testSig,
		ChannelID: lnwire.ChannelID{
			BlockHeight: blockHeight,
		},
		Timestamp:                 uint32(prand.Int31()),
		TimeLockDelta:             uint16(prand.Int63()),
		HtlcMinimumMsat:           uint32(prand.Int31()),
		FeeBaseMsat:               uint32(prand.Int31()),
		FeeProportionalMillionths: uint32(prand.Int31()),
	}
}

func createChannelAnnouncement(blockHeight uint32) *lnwire.ChannelAnnouncement {
	// Our fake channel will be "confirmed" at height 101.
	chanID := lnwire.ChannelID{
		BlockHeight: blockHeight,
		TxIndex:     0,
		TxPosition:  0,
	}

	return &lnwire.ChannelAnnouncement{
		ChannelID: chanID,
	}
}

type testCtx struct {
	discovery *Discovery
	router    *mockGraphSource
	notifier  *mockNotifier

	broadcastedMessage chan lnwire.Message
}

func createTestCtx(startHeight uint32) (*testCtx, func(), error) {
	// Next we'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send,
	// broadcast functions won't be populated.
	notifier := newMockNotifier()
	router := newMockRouter(startHeight)

	broadcastedMessage := make(chan lnwire.Message)
	discovery, err := New(Config{
		Notifier: notifier,
		Broadcast: func(_ *btcec.PublicKey, msgs ...lnwire.Message) error {
			for _, msg := range msgs {
				broadcastedMessage <- msg
			}
			return nil
		},
		SendMessages: nil,
		Router:       router,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create router %v", err)
	}
	if err := discovery.Start(); err != nil {
		return nil, nil, fmt.Errorf("unable to start router: %v", err)
	}

	cleanUp := func() {
		discovery.Stop()
	}

	return &testCtx{
		router:             router,
		notifier:           notifier,
		discovery:          discovery,
		broadcastedMessage: broadcastedMessage,
	}, cleanUp, nil
}

// TestProcessAnnouncement checks that mature announcements are propagated to
// the router subsystem.
func TestProcessAnnouncement(t *testing.T) {
	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("can't create node pub key: %v", err)
	}
	nodePub := priv.PubKey()

	na, err := createNodeAnnouncement()
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	ctx.discovery.ProcessRemoteAnnouncement(na, nodePub)

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(time.Second):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.nodes) != 1 {
		t.Fatalf("node wasn't added to router: %v", err)
	}

	ca := createChannelAnnouncement(0)
	ctx.discovery.ProcessRemoteAnnouncement(ca, nodePub)
	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(time.Second):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge wasn't added to router: %v", err)
	}

	ua := createUpdateAnnouncement(0)
	ctx.discovery.ProcessRemoteAnnouncement(ua, nodePub)
	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(time.Second):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.updates) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestPrematureAnnouncement checks that premature networkMsgs are
// not propagated to the router subsystem until block with according
// block height received.
func TestPrematureAnnouncement(t *testing.T) {
	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("can't create node pub key: %v", err)
	}
	nodePub := priv.PubKey()

	ca := createChannelAnnouncement(1)
	ctx.discovery.ProcessRemoteAnnouncement(ca, nodePub)
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.edges) != 0 {
		t.Fatal("edge was added to router")
	}

	ua := createUpdateAnnouncement(1)
	ctx.discovery.ProcessRemoteAnnouncement(ua, nodePub)
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.updates) != 0 {
		t.Fatal("edge update was added to router")
	}

	newBlock := &wire.MsgBlock{}
	ctx.notifier.notifyBlock(newBlock.Header.BlockHash(), 1)

	select {
	case <-ctx.broadcastedMessage:
		if err != nil {
			t.Fatalf("announcememt was proceeded with err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge was't added to router: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		if err != nil {
			t.Fatalf("announcememt was proceeded with err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.updates) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}
