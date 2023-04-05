package discovery

import (
	"bytes"
	"encoding/hex"
	"fmt"
	prand "math/rand"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	testAddrs    = []net.Addr{testAddr}
	testFeatures = lnwire.NewRawFeatureVector()
	testKeyLoc   = keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey}

	selfKeyPriv, _ = btcec.NewPrivateKey()
	selfKeyDesc    = &keychain.KeyDescriptor{
		PubKey:     selfKeyPriv.PubKey(),
		KeyLocator: testKeyLoc,
	}

	bitcoinKeyPriv1, _ = btcec.NewPrivateKey()
	bitcoinKeyPub1     = bitcoinKeyPriv1.PubKey()

	remoteKeyPriv1, _ = btcec.NewPrivateKey()
	remoteKeyPub1     = remoteKeyPriv1.PubKey()

	bitcoinKeyPriv2, _ = btcec.NewPrivateKey()
	bitcoinKeyPub2     = bitcoinKeyPriv2.PubKey()

	remoteKeyPriv2, _ = btcec.NewPrivateKey()

	trickleDelay     = time.Millisecond * 100
	retransmitDelay  = time.Hour * 1
	proofMatureDelta uint32

	// The test timestamp + rebroadcast interval makes sure messages won't
	// be rebroadcasted automaticallty during the tests.
	testTimestamp       = uint32(1234567890)
	rebroadcastInterval = time.Hour * 1000000
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes.
func makeTestDB(t *testing.T) (*channeldb.DB, error) {
	// Create channeldb for the first time.
	cdb, err := channeldb.Open(t.TempDir())
	if err != nil {
		return nil, err
	}

	t.Cleanup(func() {
		cdb.Close()
	})

	return cdb, nil
}

type mockGraphSource struct {
	bestHeight uint32

	mu            sync.Mutex
	nodes         []channeldb.LightningNode
	infos         map[uint64]channeldb.ChannelEdgeInfo
	edges         map[uint64][]channeldb.ChannelEdgePolicy
	zombies       map[uint64][][33]byte
	chansToReject map[uint64]struct{}
}

func newMockRouter(height uint32) *mockGraphSource {
	return &mockGraphSource{
		bestHeight:    height,
		infos:         make(map[uint64]channeldb.ChannelEdgeInfo),
		edges:         make(map[uint64][]channeldb.ChannelEdgePolicy),
		zombies:       make(map[uint64][][33]byte),
		chansToReject: make(map[uint64]struct{}),
	}
}

var _ routing.ChannelGraphSource = (*mockGraphSource)(nil)

func (r *mockGraphSource) AddNode(node *channeldb.LightningNode,
	_ ...batch.SchedulerOption) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes = append(r.nodes, *node)
	return nil
}

func (r *mockGraphSource) AddEdge(info *channeldb.ChannelEdgeInfo,
	_ ...batch.SchedulerOption) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.infos[info.ChannelID]; ok {
		return errors.New("info already exist")
	}

	if _, ok := r.chansToReject[info.ChannelID]; ok {
		return errors.New("validation failed")
	}

	r.infos[info.ChannelID] = *info
	return nil
}

func (r *mockGraphSource) queueValidationFail(chanID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.chansToReject[chanID] = struct{}{}
}

func (r *mockGraphSource) UpdateEdge(edge *channeldb.ChannelEdgePolicy,
	_ ...batch.SchedulerOption) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.edges[edge.ChannelID]) == 0 {
		r.edges[edge.ChannelID] = make([]channeldb.ChannelEdgePolicy, 2)
	}

	if edge.ChannelFlags&lnwire.ChanUpdateDirection == 0 {
		r.edges[edge.ChannelID][0] = *edge
	} else {
		r.edges[edge.ChannelID][1] = *edge
	}

	return nil
}

func (r *mockGraphSource) CurrentBlockHeight() (uint32, error) {
	return r.bestHeight, nil
}

func (r *mockGraphSource) AddProof(chanID lnwire.ShortChannelID,
	proof *channeldb.ChannelAuthProof) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	chanIDInt := chanID.ToUint64()
	info, ok := r.infos[chanIDInt]
	if !ok {
		return errors.New("channel does not exist")
	}

	info.AuthProof = proof
	r.infos[chanIDInt] = info

	return nil
}

func (r *mockGraphSource) ForEachNode(func(node *channeldb.LightningNode) error) error {
	return nil
}

func (r *mockGraphSource) ForAllOutgoingChannels(cb func(tx kvdb.RTx,
	i *channeldb.ChannelEdgeInfo,
	c *channeldb.ChannelEdgePolicy) error) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	chans := make(map[uint64]channeldb.ChannelEdge)
	for _, info := range r.infos {
		info := info

		edgeInfo := chans[info.ChannelID]
		edgeInfo.Info = &info
		chans[info.ChannelID] = edgeInfo
	}
	for _, edges := range r.edges {
		edges := edges

		edge := chans[edges[0].ChannelID]
		edge.Policy1 = &edges[0]
		chans[edges[0].ChannelID] = edge
	}

	for _, channel := range chans {
		if err := cb(nil, channel.Info, channel.Policy1); err != nil {
			return err
		}
	}

	return nil
}

func (r *mockGraphSource) ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {
	return nil
}

func (r *mockGraphSource) GetChannelByID(chanID lnwire.ShortChannelID) (
	*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	r.mu.Lock()
	defer r.mu.Unlock()

	chanIDInt := chanID.ToUint64()
	chanInfo, ok := r.infos[chanIDInt]
	if !ok {
		pubKeys, isZombie := r.zombies[chanIDInt]
		if !isZombie {
			return nil, nil, nil, channeldb.ErrEdgeNotFound
		}

		return &channeldb.ChannelEdgeInfo{
			NodeKey1Bytes: pubKeys[0],
			NodeKey2Bytes: pubKeys[1],
		}, nil, nil, channeldb.ErrZombieEdge
	}

	edges := r.edges[chanID.ToUint64()]
	if len(edges) == 0 {
		return &chanInfo, nil, nil, nil
	}

	var edge1 *channeldb.ChannelEdgePolicy
	if !reflect.DeepEqual(edges[0], channeldb.ChannelEdgePolicy{}) {
		edge1 = &edges[0]
	}

	var edge2 *channeldb.ChannelEdgePolicy
	if !reflect.DeepEqual(edges[1], channeldb.ChannelEdgePolicy{}) {
		edge2 = &edges[1]
	}

	return &chanInfo, edge1, edge2, nil
}

func (r *mockGraphSource) FetchLightningNode(
	nodePub route.Vertex) (*channeldb.LightningNode, error) {

	for _, node := range r.nodes {
		if bytes.Equal(nodePub[:], node.PubKeyBytes[:]) {
			return &node, nil
		}
	}

	return nil, channeldb.ErrGraphNodeNotFound
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
func (r *mockGraphSource) IsStaleNode(nodePub route.Vertex, timestamp time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, node := range r.nodes {
		if node.PubKeyBytes == nodePub {
			return node.LastUpdate.After(timestamp) ||
				node.LastUpdate.Equal(timestamp)
		}
	}

	// If we did not find the node among our existing graph nodes, we
	// require the node to already have a channel in the graph to not be
	// considered stale.
	for _, info := range r.infos {
		if info.NodeKey1Bytes == nodePub {
			return false
		}
		if info.NodeKey2Bytes == nodePub {
			return false
		}
	}
	return true
}

// IsPublicNode determines whether the given vertex is seen as a public node in
// the graph from the graph's source node's point of view.
func (r *mockGraphSource) IsPublicNode(node route.Vertex) (bool, error) {
	for _, info := range r.infos {
		if !bytes.Equal(node[:], info.NodeKey1Bytes[:]) &&
			!bytes.Equal(node[:], info.NodeKey2Bytes[:]) {
			continue
		}

		if info.AuthProof != nil {
			return true, nil
		}
	}
	return false, nil
}

// IsKnownEdge returns true if the graph source already knows of the passed
// channel ID either as a live or zombie channel.
func (r *mockGraphSource) IsKnownEdge(chanID lnwire.ShortChannelID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	chanIDInt := chanID.ToUint64()
	_, exists := r.infos[chanIDInt]
	_, isZombie := r.zombies[chanIDInt]
	return exists || isZombie
}

// IsStaleEdgePolicy returns true if the graph source has a channel edge for
// the passed channel ID (and flags) that have a more recent timestamp.
func (r *mockGraphSource) IsStaleEdgePolicy(chanID lnwire.ShortChannelID,
	timestamp time.Time, flags lnwire.ChanUpdateChanFlags) bool {

	r.mu.Lock()
	defer r.mu.Unlock()

	chanIDInt := chanID.ToUint64()
	edges, ok := r.edges[chanIDInt]
	if !ok {
		// Since the edge doesn't exist, we'll check our zombie index as
		// well.
		_, isZombie := r.zombies[chanIDInt]
		if !isZombie {
			return false
		}

		// Since it exists within our zombie index, we'll check that it
		// respects the router's live edge horizon to determine whether
		// it is stale or not.
		return time.Since(timestamp) > routing.DefaultChannelPruneExpiry
	}

	switch {
	case flags&lnwire.ChanUpdateDirection == 0 &&
		!reflect.DeepEqual(edges[0], channeldb.ChannelEdgePolicy{}):

		return !timestamp.After(edges[0].LastUpdate)

	case flags&lnwire.ChanUpdateDirection == 1 &&
		!reflect.DeepEqual(edges[1], channeldb.ChannelEdgePolicy{}):

		return !timestamp.After(edges[1].LastUpdate)

	default:
		return false
	}
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *mockGraphSource) MarkEdgeLive(chanID lnwire.ShortChannelID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.zombies, chanID.ToUint64())
	return nil
}

// MarkEdgeZombie marks an edge as a zombie within our zombie index.
func (r *mockGraphSource) MarkEdgeZombie(chanID lnwire.ShortChannelID, pubKey1,
	pubKey2 [33]byte) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.zombies[chanID.ToUint64()] = [][33]byte{pubKey1, pubKey2}

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
	_ []byte, numConfs, _ uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	_ uint32) (*chainntnfs.SpendEvent, error) {
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

func (m *mockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
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

func (m *mockNotifier) Started() bool {
	return true
}

func (m *mockNotifier) Stop() error {
	return nil
}

type annBatch struct {
	nodeAnn1 *lnwire.NodeAnnouncement
	nodeAnn2 *lnwire.NodeAnnouncement

	chanAnn *lnwire.ChannelAnnouncement

	chanUpdAnn1 *lnwire.ChannelUpdate
	chanUpdAnn2 *lnwire.ChannelUpdate

	localProofAnn  *lnwire.AnnounceSignatures
	remoteProofAnn *lnwire.AnnounceSignatures
}

func createLocalAnnouncements(blockHeight uint32) (*annBatch, error) {
	return createAnnouncements(blockHeight, selfKeyPriv, remoteKeyPriv1)
}

func createRemoteAnnouncements(blockHeight uint32) (*annBatch, error) {
	return createAnnouncements(blockHeight, remoteKeyPriv1, remoteKeyPriv2)
}

func createAnnouncements(blockHeight uint32, key1, key2 *btcec.PrivateKey) (*annBatch, error) {
	var err error
	var batch annBatch
	timestamp := testTimestamp

	batch.nodeAnn1, err = createNodeAnnouncement(key1, timestamp)
	if err != nil {
		return nil, err
	}

	batch.nodeAnn2, err = createNodeAnnouncement(key2, timestamp)
	if err != nil {
		return nil, err
	}

	batch.chanAnn, err = createChannelAnnouncement(blockHeight, key1, key2)
	if err != nil {
		return nil, err
	}

	batch.remoteProofAnn = &lnwire.AnnounceSignatures{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		NodeSignature:    batch.chanAnn.NodeSig2,
		BitcoinSignature: batch.chanAnn.BitcoinSig2,
	}

	batch.localProofAnn = &lnwire.AnnounceSignatures{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		NodeSignature:    batch.chanAnn.NodeSig1,
		BitcoinSignature: batch.chanAnn.BitcoinSig1,
	}

	batch.chanUpdAnn1, err = createUpdateAnnouncement(
		blockHeight, 0, key1, timestamp,
	)
	if err != nil {
		return nil, err
	}

	batch.chanUpdAnn2, err = createUpdateAnnouncement(
		blockHeight, 1, key2, timestamp,
	)
	if err != nil {
		return nil, err
	}

	return &batch, nil

}

func createNodeAnnouncement(priv *btcec.PrivateKey,
	timestamp uint32, extraBytes ...[]byte) (*lnwire.NodeAnnouncement, error) {

	var err error
	k := hex.EncodeToString(priv.Serialize())
	alias, err := lnwire.NewNodeAlias("kek" + k[:10])
	if err != nil {
		return nil, err
	}

	a := &lnwire.NodeAnnouncement{
		Timestamp: timestamp,
		Addresses: testAddrs,
		Alias:     alias,
		Features:  testFeatures,
	}
	copy(a.NodeID[:], priv.PubKey().SerializeCompressed())
	if len(extraBytes) == 1 {
		a.ExtraOpaqueData = extraBytes[0]
	}

	signer := mock.SingleSigner{Privkey: priv}
	sig, err := netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return nil, err
	}

	a.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func createUpdateAnnouncement(blockHeight uint32,
	flags lnwire.ChanUpdateChanFlags,
	nodeKey *btcec.PrivateKey, timestamp uint32,
	extraBytes ...[]byte) (*lnwire.ChannelUpdate, error) {

	var err error

	htlcMinMsat := lnwire.MilliSatoshi(prand.Int63())
	a := &lnwire.ChannelUpdate{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		Timestamp:       timestamp,
		MessageFlags:    lnwire.ChanUpdateRequiredMaxHtlc,
		ChannelFlags:    flags,
		TimeLockDelta:   uint16(prand.Int63()),
		HtlcMinimumMsat: htlcMinMsat,

		// Since the max HTLC must be greater than the min HTLC to pass channel
		// update validation, set it to double the min htlc.
		HtlcMaximumMsat: 2 * htlcMinMsat,
		FeeRate:         uint32(prand.Int31()),
		BaseFee:         uint32(prand.Int31()),
	}
	if len(extraBytes) == 1 {
		a.ExtraOpaqueData = extraBytes[0]
	}

	err = signUpdate(nodeKey, a)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func signUpdate(nodeKey *btcec.PrivateKey, a *lnwire.ChannelUpdate) error {
	signer := mock.SingleSigner{Privkey: nodeKey}
	sig, err := netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return err
	}

	a.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return err
	}

	return nil
}

func createAnnouncementWithoutProof(blockHeight uint32,
	key1, key2 *btcec.PublicKey,
	extraBytes ...[]byte) *lnwire.ChannelAnnouncement {

	a := &lnwire.ChannelAnnouncement{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
			TxIndex:     0,
			TxPosition:  0,
		},
		Features: testFeatures,
	}
	copy(a.NodeID1[:], key1.SerializeCompressed())
	copy(a.NodeID2[:], key2.SerializeCompressed())
	copy(a.BitcoinKey1[:], bitcoinKeyPub1.SerializeCompressed())
	copy(a.BitcoinKey2[:], bitcoinKeyPub2.SerializeCompressed())
	if len(extraBytes) == 1 {
		a.ExtraOpaqueData = extraBytes[0]
	}

	return a
}

func createRemoteChannelAnnouncement(blockHeight uint32,
	extraBytes ...[]byte) (*lnwire.ChannelAnnouncement, error) {

	return createChannelAnnouncement(blockHeight, remoteKeyPriv1, remoteKeyPriv2, extraBytes...)
}

func createChannelAnnouncement(blockHeight uint32, key1, key2 *btcec.PrivateKey,
	extraBytes ...[]byte) (*lnwire.ChannelAnnouncement, error) {

	a := createAnnouncementWithoutProof(blockHeight, key1.PubKey(), key2.PubKey(), extraBytes...)

	signer := mock.SingleSigner{Privkey: key1}
	sig, err := netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return nil, err
	}
	a.NodeSig1, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	signer = mock.SingleSigner{Privkey: key2}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return nil, err
	}
	a.NodeSig2, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	signer = mock.SingleSigner{Privkey: bitcoinKeyPriv1}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return nil, err
	}
	a.BitcoinSig1, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	signer = mock.SingleSigner{Privkey: bitcoinKeyPriv2}
	sig, err = netann.SignAnnouncement(&signer, testKeyLoc, a)
	if err != nil {
		return nil, err
	}
	a.BitcoinSig2, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func mockFindChannel(node *btcec.PublicKey, chanID lnwire.ChannelID) (
	*channeldb.OpenChannel, error) {

	return nil, nil
}

type testCtx struct {
	gossiper           *AuthenticatedGossiper
	router             *mockGraphSource
	notifier           *mockNotifier
	broadcastedMessage chan msgWithSenders
}

func createTestCtx(t *testing.T, startHeight uint32) (*testCtx, error) {
	// Next we'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send,
	// broadcast functions won't be populated.
	notifier := newMockNotifier()
	router := newMockRouter(startHeight)

	db, err := makeTestDB(t)
	if err != nil {
		return nil, err
	}

	waitingProofStore, err := channeldb.NewWaitingProofStore(db)
	if err != nil {
		return nil, err
	}

	broadcastedMessage := make(chan msgWithSenders, 10)

	isAlias := func(lnwire.ShortChannelID) bool {
		return false
	}

	signAliasUpdate := func(*lnwire.ChannelUpdate) (*ecdsa.Signature,
		error) {

		return nil, nil
	}

	findBaseByAlias := func(lnwire.ShortChannelID) (lnwire.ShortChannelID,
		error) {

		return lnwire.ShortChannelID{}, fmt.Errorf("no base scid")
	}

	getAlias := func(lnwire.ChannelID) (lnwire.ShortChannelID, error) {
		return lnwire.ShortChannelID{}, fmt.Errorf("no peer alias")
	}

	gossiper := New(Config{
		Notifier: notifier,
		Broadcast: func(senders map[route.Vertex]struct{},
			msgs ...lnwire.Message) error {

			for _, msg := range msgs {
				broadcastedMessage <- msgWithSenders{
					msg:     msg,
					senders: senders,
				}
			}

			return nil
		},
		NotifyWhenOnline: func(target [33]byte,
			peerChan chan<- lnpeer.Peer) {

			pk, _ := btcec.ParsePubKey(target[:])
			peerChan <- &mockPeer{pk, nil, nil}
		},
		NotifyWhenOffline: func(_ [33]byte) <-chan struct{} {
			c := make(chan struct{})
			return c
		},
		FetchSelfAnnouncement: func() lnwire.NodeAnnouncement {
			return lnwire.NodeAnnouncement{
				Timestamp: testTimestamp,
			}
		},
		UpdateSelfAnnouncement: func() (lnwire.NodeAnnouncement,
			error) {

			return lnwire.NodeAnnouncement{
				Timestamp: testTimestamp,
			}, nil
		},
		Router:                router,
		TrickleDelay:          trickleDelay,
		RetransmitTicker:      ticker.NewForce(retransmitDelay),
		RebroadcastInterval:   rebroadcastInterval,
		ProofMatureDelta:      proofMatureDelta,
		WaitingProofStore:     waitingProofStore,
		MessageStore:          newMockMessageStore(),
		RotateTicker:          ticker.NewForce(DefaultSyncerRotationInterval),
		HistoricalSyncTicker:  ticker.NewForce(DefaultHistoricalSyncInterval),
		NumActiveSyncers:      3,
		AnnSigner:             &mock.SingleSigner{Privkey: selfKeyPriv},
		SubBatchDelay:         1 * time.Millisecond,
		MinimumBatchSize:      10,
		MaxChannelUpdateBurst: DefaultMaxChannelUpdateBurst,
		ChannelUpdateInterval: DefaultChannelUpdateInterval,
		IsAlias:               isAlias,
		SignAliasUpdate:       signAliasUpdate,
		FindBaseByAlias:       findBaseByAlias,
		GetAlias:              getAlias,
		FindChannel:           mockFindChannel,
	}, selfKeyDesc)

	if err := gossiper.Start(); err != nil {
		return nil, fmt.Errorf("unable to start router: %v", err)
	}

	// Mark the graph as synced in order to allow the announcements to be
	// broadcast.
	gossiper.syncMgr.markGraphSynced()

	t.Cleanup(func() {
		gossiper.Stop()
	})

	return &testCtx{
		router:             router,
		notifier:           notifier,
		gossiper:           gossiper,
		broadcastedMessage: broadcastedMessage,
	}, nil
}

// TestProcessAnnouncement checks that mature announcements are propagated to
// the router subsystem.
func TestProcessAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := testTimestamp
	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	assertSenderExistence := func(sender *btcec.PublicKey, msg msgWithSenders) {
		t.Helper()

		if _, ok := msg.senders[route.NewVertex(sender)]; !ok {
			t.Fatalf("sender=%x not present in %v",
				sender.SerializeCompressed(), spew.Sdump(msg))
		}
	}

	nodePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}

	// First, we'll craft a valid remote channel announcement and send it to
	// the gossiper so that it can be processed.
	ca, err := createRemoteChannelAnnouncement(0)
	require.NoError(t, err, "can't create channel announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ca, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	require.NoError(t, err, "can't process remote announcement")

	// The announcement should be broadcast and included in our local view
	// of the graph.
	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(nodePeer.IdentityKey(), msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcement wasn't proceeded")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge wasn't added to router: %v", err)
	}

	// We'll craft an invalid channel update, setting no message flags.
	ua, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create update announcement")
	ua.MessageFlags = 0

	// We send an invalid channel update and expect it to fail.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ua, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	require.ErrorContains(t, err, "max htlc flag not set for channel "+
		"update")

	// We should not broadcast the channel update.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("gossiper should not have broadcast channel update")
	case <-time.After(2 * trickleDelay):
	}

	// We'll then craft the channel policy of the remote party and also send
	// it to the gossiper.
	ua, err = createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create update announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ua, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	require.NoError(t, err, "can't process remote announcement")

	// The channel policy should be broadcast to the rest of the network.
	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(nodePeer.IdentityKey(), msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcement wasn't proceeded")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}

	// Finally, we'll craft the remote party's node announcement.
	na, err := createNodeAnnouncement(remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create node announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(na, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	require.NoError(t, err, "can't process remote announcement")

	// It should also be broadcast to the network and included in our local
	// view of the graph.
	select {
	case msg := <-ctx.broadcastedMessage:
		assertSenderExistence(nodePeer.IdentityKey(), msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcement wasn't proceeded")
	}

	if len(ctx.router.nodes) != 1 {
		t.Fatalf("node wasn't added to router: %v", err)
	}
}

// TestPrematureAnnouncement checks that premature announcements are not
// propagated to the router subsystem.
func TestPrematureAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := testTimestamp

	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	_, err = createNodeAnnouncement(remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create node announcement")

	nodePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}

	// Pretending that we receive the valid channel announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be ignored and not
	// added to the router.
	ca, err := createRemoteChannelAnnouncement(1)
	require.NoError(t, err, "can't create channel announcement")

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ca, nodePeer):
	case <-time.After(time.Second):
		t.Fatal("announcement was not processed")
	}

	if len(ctx.router.infos) != 0 {
		t.Fatal("edge was added to router")
	}
}

// TestSignatureAnnouncementLocalFirst ensures that the AuthenticatedGossiper
// properly processes partial and fully announcement signatures message.
func TestSignatureAnnouncementLocalFirst(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(target [33]byte,
		peerChan chan<- lnpeer.Peer) {

		pk, _ := btcec.ParsePubKey(target[:])

		select {
		case peerChan <- &mockPeer{pk, sentMsgs, ctx.gossiper.quit}:
		case <-ctx.gossiper.quit:
		}
	}

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		assertMessage(t, batch.chanUpdAnn1, msg)
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.nodeAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process local proof")

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process remote proof")

	for i := 0; i < 5; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// TestOrphanSignatureAnnouncement ensures that the gossiper properly
// processes announcement with unknown channel ids.
func TestOrphanSignatureAnnouncement(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(target [33]byte,
		peerChan chan<- lnpeer.Peer) {

		pk, _ := btcec.ParsePubKey(target[:])

		select {
		case peerChan <- &mockPeer{pk, sentMsgs, ctx.gossiper.quit}:
		case <-ctx.gossiper.quit:
		}
	}

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process, in
	// this case the announcement should be added in the orphan batch
	// because we haven't announce the channel yet.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to proceed announcement")

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}

	require.NoError(t, err, "unable to process")

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process")

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		assertMessage(t, batch.chanUpdAnn1, msg)
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.nodeAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// After that we process local announcement, and waiting to receive
	// the channel announcement.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process")

	// The local proof should be sent to the remote peer.
	select {
	case msg := <-sentMsgs:
		assertMessage(t, batch.localProofAnn, msg)
	case <-time.After(2 * time.Second):
		t.Fatalf("local proof was not sent to peer")
	}

	// And since both remote and local announcements are processed, we
	// should be broadcasting the final channel announcements.
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(p *channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatalf("wrong number of objects in storage: %v", number)
	}
}

// TestSignatureAnnouncementRetryAtStartup tests that if we restart the
// gossiper, it will retry sending the AnnounceSignatures to the peer if it did
// not succeed before shutting down, and the full channel proof is not yet
// assembled.
func TestSignatureAnnouncementRetryAtStartup(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")

	// Set up a channel to intercept the messages sent to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Since the reliable send to the remote peer of the local channel proof
	// requires a notification when the peer comes online, we'll capture the
	// channel through which it gets sent to control exactly when to
	// dispatch it.
	notifyPeers := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer [33]byte,
		connectedChan chan<- lnpeer.Peer) {
		notifyPeers <- connectedChan
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// The gossiper should register for a notification for when the peer is
	// online.
	select {
	case <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// The proof should not be broadcast yet since we're still missing the
	// remote party's.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// And it shouldn't be sent to the peer either as they are offline.
	select {
	case msg := <-sentToPeer:
		t.Fatalf("received unexpected message: %v", spew.Sdump(msg))
	case <-time.After(time.Second):
	}

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	// Restart the gossiper and restore its original NotifyWhenOnline and
	// NotifyWhenOffline methods. This should trigger a new attempt to send
	// the message to the peer.
	ctx.gossiper.Stop()

	isAlias := func(lnwire.ShortChannelID) bool {
		return false
	}

	signAliasUpdate := func(*lnwire.ChannelUpdate) (*ecdsa.Signature,
		error) {

		return nil, nil
	}

	findBaseByAlias := func(lnwire.ShortChannelID) (lnwire.ShortChannelID,
		error) {

		return lnwire.ShortChannelID{}, fmt.Errorf("no base scid")
	}

	getAlias := func(lnwire.ChannelID) (lnwire.ShortChannelID, error) {
		return lnwire.ShortChannelID{}, fmt.Errorf("no peer alias")
	}

	//nolint:lll
	gossiper := New(Config{
		Notifier:               ctx.gossiper.cfg.Notifier,
		Broadcast:              ctx.gossiper.cfg.Broadcast,
		NotifyWhenOnline:       ctx.gossiper.reliableSender.cfg.NotifyWhenOnline,
		NotifyWhenOffline:      ctx.gossiper.reliableSender.cfg.NotifyWhenOffline,
		FetchSelfAnnouncement:  ctx.gossiper.cfg.FetchSelfAnnouncement,
		UpdateSelfAnnouncement: ctx.gossiper.cfg.UpdateSelfAnnouncement,
		Router:                 ctx.gossiper.cfg.Router,
		TrickleDelay:           trickleDelay,
		RetransmitTicker:       ticker.NewForce(retransmitDelay),
		RebroadcastInterval:    rebroadcastInterval,
		ProofMatureDelta:       proofMatureDelta,
		WaitingProofStore:      ctx.gossiper.cfg.WaitingProofStore,
		MessageStore:           ctx.gossiper.cfg.MessageStore,
		RotateTicker:           ticker.NewForce(DefaultSyncerRotationInterval),
		HistoricalSyncTicker:   ticker.NewForce(DefaultHistoricalSyncInterval),
		NumActiveSyncers:       3,
		MinimumBatchSize:       10,
		SubBatchDelay:          time.Second * 5,
		IsAlias:                isAlias,
		SignAliasUpdate:        signAliasUpdate,
		FindBaseByAlias:        findBaseByAlias,
		GetAlias:               getAlias,
	}, &keychain.KeyDescriptor{
		PubKey:     ctx.gossiper.selfKey,
		KeyLocator: ctx.gossiper.selfKeyLoc,
	})
	require.NoError(t, err, "unable to recreate gossiper")
	if err := gossiper.Start(); err != nil {
		t.Fatalf("unable to start recreated gossiper: %v", err)
	}
	defer gossiper.Stop()

	// Mark the graph as synced in order to allow the announcements to be
	// broadcast.
	gossiper.syncMgr.markGraphSynced()

	ctx.gossiper = gossiper
	remotePeer.quit = ctx.gossiper.quit

	// After starting up, the gossiper will see that it has a proof in the
	// WaitingProofStore, and will retry sending its part to the remote.
	// It should register for a notification for when the peer is online.
	var peerChan chan<- lnpeer.Peer
	select {
	case peerChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// Notify that peer is now online. This should allow the proof to be
	// sent.
	peerChan <- remotePeer

out:
	for {
		select {
		case msg := <-sentToPeer:
			// Since the ChannelUpdate will also be resent as it is
			// sent reliably, we'll need to filter it out.
			if _, ok := msg.(*lnwire.AnnounceSignatures); !ok {
				continue
			}

			assertMessage(t, batch.localProofAnn, msg)
			break out
		case <-time.After(2 * time.Second):
			t.Fatalf("gossiper did not send message when peer " +
				"came online")
		}
	}

	// Now exchanging the remote channel proof, the channel announcement
	// broadcast should continue as normal.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(time.Second):
		t.Fatal("announcement wasn't broadcast")
	}

	number = 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// TestSignatureAnnouncementFullProofWhenRemoteProof tests that if a remote
// proof is received when we already have the full proof, the gossiper will send
// the full proof (ChannelAnnouncement) to the remote peer.
func TestSignatureAnnouncementFullProofWhenRemoteProof(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")

	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Override NotifyWhenOnline to return the remote peer which we expect
	// meesages to be sent to.
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer [33]byte,
		peerChan chan<- lnpeer.Peer) {

		peerChan <- remotePeer
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanAnn,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case msg := <-sentToPeer:
		assertMessage(t, batch.chanUpdAnn1, msg)
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not send channel update to remove peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.nodeAnn1,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann:%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.nodeAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process local proof")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process remote proof")

	// We expect the gossiper to send this message to the remote peer.
	select {
	case msg := <-sentToPeer:
		assertMessage(t, batch.localProofAnn, msg)
	case <-time.After(2 * time.Second):
		t.Fatal("did not send local proof to peer")
	}

	// All channel and node announcements should be broadcast.
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}

	// Now give the gossiper the remote proof yet again. This should
	// trigger a send of the full ChannelAnnouncement.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process remote proof")

	// We expect the gossiper to send this message to the remote peer.
	select {
	case msg := <-sentToPeer:
		_, ok := msg.(*lnwire.ChannelAnnouncement)
		if !ok {
			t.Fatalf("expected ChannelAnnouncement, instead got %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not send local proof to peer")
	}
}

// TestDeDuplicatedAnnouncements ensures that the deDupedAnnouncements struct
// properly stores and delivers the set of de-duplicated announcements.
func TestDeDuplicatedAnnouncements(t *testing.T) {
	t.Parallel()

	timestamp := testTimestamp
	announcements := deDupedAnnouncements{}
	announcements.Reset()

	// Ensure that after new deDupedAnnouncements struct is created and
	// reset that storage of each announcement type is empty.
	if len(announcements.channelAnnouncements) != 0 {
		t.Fatal("channel announcements map not empty after reset")
	}
	if len(announcements.channelUpdates) != 0 {
		t.Fatal("channel updates map not empty after reset")
	}
	if len(announcements.nodeAnnouncements) != 0 {
		t.Fatal("node announcements map not empty after reset")
	}

	// Ensure that remote channel announcements are properly stored
	// and de-duplicated.
	ca, err := createRemoteChannelAnnouncement(0)
	require.NoError(t, err, "can't create remote channel announcement")

	nodePeer := &mockPeer{bitcoinKeyPub2, nil, nil}
	announcements.AddMsgs(networkMsg{
		msg:    ca,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelAnnouncements) != 1 {
		t.Fatal("new channel announcement not stored in batch")
	}

	// We'll create a second instance of the same announcement with the
	// same channel ID. Adding this shouldn't cause an increase in the
	// number of items as they should be de-duplicated.
	ca2, err := createRemoteChannelAnnouncement(0)
	require.NoError(t, err, "can't create remote channel announcement")
	announcements.AddMsgs(networkMsg{
		msg:    ca2,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelAnnouncements) != 1 {
		t.Fatal("channel announcement not replaced in batch")
	}

	// Next, we'll ensure that channel update announcements are properly
	// stored and de-duplicated. We do this by creating two updates
	// announcements with the same short ID and flag.
	ua, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create update announcement")
	announcements.AddMsgs(networkMsg{
		msg:    ua,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("new channel update not stored in batch")
	}

	// Adding the very same announcement shouldn't cause an increase in the
	// number of ChannelUpdate announcements stored.
	ua2, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create update announcement")
	announcements.AddMsgs(networkMsg{
		msg:    ua2,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not replaced in batch")
	}

	// Adding an announcement with a later timestamp should replace the
	// stored one.
	ua3, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp+1)
	require.NoError(t, err, "can't create update announcement")
	announcements.AddMsgs(networkMsg{
		msg:    ua3,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not replaced in batch")
	}

	assertChannelUpdate := func(channelUpdate *lnwire.ChannelUpdate) {
		channelKey := channelUpdateID{
			ua3.ShortChannelID,
			ua3.ChannelFlags,
		}

		mws, ok := announcements.channelUpdates[channelKey]
		if !ok {
			t.Fatal("channel update not in batch")
		}
		if mws.msg != channelUpdate {
			t.Fatalf("expected channel update %v, got %v)",
				channelUpdate, mws.msg)
		}
	}

	// Check that ua3 is the currently stored channel update.
	assertChannelUpdate(ua3)

	// Adding a channel update with an earlier timestamp should NOT
	// replace the one stored.
	ua4, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create update announcement")
	announcements.AddMsgs(networkMsg{
		msg:    ua4,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.channelUpdates) != 1 {
		t.Fatal("channel update not in batch")
	}
	assertChannelUpdate(ua3)

	// Next well ensure that node announcements are properly de-duplicated.
	// We'll first add a single instance with a node's private key.
	na, err := createNodeAnnouncement(remoteKeyPriv1, timestamp)
	require.NoError(t, err, "can't create node announcement")
	announcements.AddMsgs(networkMsg{
		msg:    na,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 1 {
		t.Fatal("new node announcement not stored in batch")
	}

	// We'll now add another node to the batch.
	na2, err := createNodeAnnouncement(remoteKeyPriv2, timestamp)
	require.NoError(t, err, "can't create node announcement")
	announcements.AddMsgs(networkMsg{
		msg:    na2,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not stored in batch")
	}

	// Adding a new instance of the _same_ node shouldn't increase the size
	// of the node ann batch.
	na3, err := createNodeAnnouncement(remoteKeyPriv2, timestamp)
	require.NoError(t, err, "can't create node announcement")
	announcements.AddMsgs(networkMsg{
		msg:    na3,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not replaced in batch")
	}

	// Ensure that node announcement with different pointer to same public
	// key is still de-duplicated.
	newNodeKeyPointer := remoteKeyPriv2
	na4, err := createNodeAnnouncement(newNodeKeyPointer, timestamp)
	require.NoError(t, err, "can't create node announcement")
	announcements.AddMsgs(networkMsg{
		msg:    na4,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("second node announcement not replaced again in batch")
	}

	// Ensure that node announcement with increased timestamp replaces
	// what is currently stored.
	na5, err := createNodeAnnouncement(remoteKeyPriv2, timestamp+1)
	require.NoError(t, err, "can't create node announcement")
	announcements.AddMsgs(networkMsg{
		msg:    na5,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("node announcement not replaced in batch")
	}
	nodeID := route.NewVertex(remoteKeyPriv2.PubKey())
	stored, ok := announcements.nodeAnnouncements[nodeID]
	if !ok {
		t.Fatalf("node announcement not found in batch")
	}
	if stored.msg != na5 {
		t.Fatalf("expected de-duped node announcement to be %v, got %v",
			na5, stored.msg)
	}

	// Ensure that announcement batch delivers channel announcements,
	// channel updates, and node announcements in proper order.
	batch := announcements.Emit()
	if batch.length() != 4 {
		t.Fatal("announcement batch incorrect length")
	}

	if !reflect.DeepEqual(batch.localMsgs[0].msg, ca2) {
		t.Fatalf("channel announcement not first in batch: got %v, "+
			"expected %v", spew.Sdump(batch.localMsgs[0].msg),
			spew.Sdump(ca2))
	}

	if !reflect.DeepEqual(batch.localMsgs[1].msg, ua3) {
		t.Fatalf("channel update not next in batch: got %v, "+
			"expected %v", spew.Sdump(batch.localMsgs[1].msg),
			spew.Sdump(ua2))
	}

	// We'll ensure that both node announcements are present. We check both
	// indexes as due to the randomized order of map iteration they may be
	// in either place.
	if !reflect.DeepEqual(batch.localMsgs[2].msg, na) &&
		!reflect.DeepEqual(batch.localMsgs[3].msg, na) {

		t.Fatalf("first node announcement not in last part of batch: "+
			"got %v, expected %v", batch.localMsgs[2].msg,
			na)
	}
	if !reflect.DeepEqual(batch.localMsgs[2].msg, na5) &&
		!reflect.DeepEqual(batch.localMsgs[3].msg, na5) {

		t.Fatalf("second node announcement not in last part of batch: "+
			"got %v, expected %v", batch.localMsgs[3].msg,
			na5)
	}

	// Ensure that after reset, storage of each announcement type
	// in deDupedAnnouncements struct is empty again.
	announcements.Reset()
	if len(announcements.channelAnnouncements) != 0 {
		t.Fatal("channel announcements map not empty after reset")
	}
	if len(announcements.channelUpdates) != 0 {
		t.Fatal("channel updates map not empty after reset")
	}
	if len(announcements.nodeAnnouncements) != 0 {
		t.Fatal("node announcements map not empty after reset")
	}
}

// TestForwardPrivateNodeAnnouncement ensures that we do not forward node
// announcements for nodes who do not intend to publicly advertise themselves.
func TestForwardPrivateNodeAnnouncement(t *testing.T) {
	t.Parallel()

	const (
		startingHeight = 100
		timestamp      = 123456
	)

	ctx, err := createTestCtx(t, startingHeight)
	require.NoError(t, err, "can't create context")

	// We'll start off by processing a channel announcement without a proof
	// (i.e., an unadvertised channel), followed by a node announcement for
	// this same channel announcement.
	chanAnn := createAnnouncementWithoutProof(
		startingHeight-2, selfKeyDesc.PubKey, remoteKeyPub1,
	)
	pubKey := remoteKeyPriv1.PubKey()

	select {
	case err := <-ctx.gossiper.ProcessLocalAnnouncement(chanAnn):
		if err != nil {
			t.Fatalf("unable to process local announcement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("local announcement not processed")
	}

	// The gossiper should not broadcast the announcement due to it not
	// having its announcement signatures.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("gossiper should not have broadcast channel announcement")
	case <-time.After(2 * trickleDelay):
	}

	nodeAnn, err := createNodeAnnouncement(remoteKeyPriv1, timestamp)
	require.NoError(t, err, "unable to create node announcement")

	select {
	case err := <-ctx.gossiper.ProcessLocalAnnouncement(nodeAnn):
		if err != nil {
			t.Fatalf("unable to process remote announcement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}

	// The gossiper should also not broadcast the node announcement due to
	// it not being part of any advertised channels.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("gossiper should not have broadcast node announcement")
	case <-time.After(2 * trickleDelay):
	}

	// Now, we'll attempt to forward the NodeAnnouncement for the same node
	// by opening a public channel on the network. We'll create a
	// ChannelAnnouncement and hand it off to the gossiper in order to
	// process it.
	remoteChanAnn, err := createRemoteChannelAnnouncement(startingHeight - 1)
	require.NoError(t, err, "unable to create remote channel announcement")
	peer := &mockPeer{pubKey, nil, nil}

	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(remoteChanAnn, peer):
		if err != nil {
			t.Fatalf("unable to process remote announcement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("gossiper should have broadcast the channel announcement")
	}

	// We'll recreate the NodeAnnouncement with an updated timestamp to
	// prevent a stale update. The NodeAnnouncement should now be forwarded.
	nodeAnn, err = createNodeAnnouncement(remoteKeyPriv1, timestamp+1)
	require.NoError(t, err, "unable to create node announcement")

	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(nodeAnn, peer):
		if err != nil {
			t.Fatalf("unable to process remote announcement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("gossiper should have broadcast the node announcement")
	}
}

// TestRejectZombieEdge ensures that we properly reject any announcements for
// zombie edges.
func TestRejectZombieEdge(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test context with a batch of
	// announcements.
	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "unable to create test context")

	batch, err := createRemoteAnnouncements(0)
	require.NoError(t, err, "unable to create announcements")
	remotePeer := &mockPeer{pk: remoteKeyPriv2.PubKey()}

	// processAnnouncements is a helper closure we'll use to test that we
	// properly process/reject announcements based on whether they're for a
	// zombie edge or not.
	processAnnouncements := func(isZombie bool) {
		t.Helper()

		errChan := ctx.gossiper.ProcessRemoteAnnouncement(
			batch.chanAnn, remotePeer,
		)
		select {
		case err := <-errChan:
			if isZombie && err != nil {
				t.Fatalf("expected to reject live channel "+
					"announcement with nil error: %v", err)
			}
			if !isZombie && err != nil {
				t.Fatalf("expected to process live channel "+
					"announcement: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("expected to process channel announcement")
		}
		select {
		case <-ctx.broadcastedMessage:
			if isZombie {
				t.Fatal("expected to not broadcast zombie " +
					"channel announcement")
			}
		case <-time.After(2 * trickleDelay):
			if !isZombie {
				t.Fatal("expected to broadcast live channel " +
					"announcement")
			}
		}

		errChan = ctx.gossiper.ProcessRemoteAnnouncement(
			batch.chanUpdAnn2, remotePeer,
		)
		select {
		case err := <-errChan:
			if isZombie && err != nil {
				t.Fatalf("expected to reject zombie channel "+
					"update with nil error: %v", err)
			}
			if !isZombie && err != nil {
				t.Fatalf("expected to process live channel "+
					"update: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("expected to process channel update")
		}
		select {
		case <-ctx.broadcastedMessage:
			if isZombie {
				t.Fatal("expected to not broadcast zombie " +
					"channel update")
			}
		case <-time.After(2 * trickleDelay):
			if !isZombie {
				t.Fatal("expected to broadcast live channel " +
					"update")
			}
		}
	}

	// We'll mark the edge for which we'll process announcements for as a
	// zombie within the router. This should reject any announcements for
	// this edge while it remains as a zombie.
	chanID := batch.chanAnn.ShortChannelID
	err = ctx.router.MarkEdgeZombie(
		chanID, batch.chanAnn.NodeID1, batch.chanAnn.NodeID2,
	)
	if err != nil {
		t.Fatalf("unable to mark channel %v as zombie: %v", chanID, err)
	}

	processAnnouncements(true)

	// If we then mark the edge as live, the edge's zombie status should be
	// overridden and the announcements should be processed.
	if err := ctx.router.MarkEdgeLive(chanID); err != nil {
		t.Fatalf("unable mark channel %v as zombie: %v", chanID, err)
	}

	processAnnouncements(false)
}

// TestProcessZombieEdgeNowLive ensures that we can detect when a zombie edge
// becomes live by receiving a fresh update.
func TestProcessZombieEdgeNowLive(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test context with a batch of
	// announcements.
	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "unable to create test context")

	batch, err := createRemoteAnnouncements(0)
	require.NoError(t, err, "unable to create announcements")

	remotePeer := &mockPeer{pk: remoteKeyPriv1.PubKey()}

	// processAnnouncement is a helper closure we'll use to ensure an
	// announcement is properly processed/rejected based on whether the edge
	// is a zombie or not. The expectsErr boolean can be used to determine
	// whether we should expect an error when processing the message, while
	// the isZombie boolean can be used to determine whether the
	// announcement should be or not be broadcast.
	processAnnouncement := func(ann lnwire.Message, isZombie, expectsErr bool) {
		t.Helper()

		errChan := ctx.gossiper.ProcessRemoteAnnouncement(
			ann, remotePeer,
		)

		var err error
		select {
		case err = <-errChan:
		case <-time.After(time.Second):
			t.Fatal("expected to process announcement")
		}
		if expectsErr && err == nil {
			t.Fatal("expected error when processing announcement")
		}
		if !expectsErr && err != nil {
			t.Fatalf("received unexpected error when processing "+
				"announcement: %v", err)
		}

		select {
		case msgWithSenders := <-ctx.broadcastedMessage:
			if isZombie {
				t.Fatal("expected to not broadcast zombie " +
					"channel message")
			}
			assertMessage(t, ann, msgWithSenders.msg)

		case <-time.After(2 * trickleDelay):
			if !isZombie {
				t.Fatal("expected to broadcast live channel " +
					"message")
			}
		}
	}

	// We'll generate a channel update with a timestamp far enough in the
	// past to consider it a zombie.
	zombieTimestamp := time.Now().Add(-routing.DefaultChannelPruneExpiry)
	batch.chanUpdAnn2.Timestamp = uint32(zombieTimestamp.Unix())
	if err := signUpdate(remoteKeyPriv2, batch.chanUpdAnn2); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}

	// We'll also add the edge to our zombie index, provide a blank pubkey
	// for the first node as we're simulating the situation where the first
	// node is updating but the second node isn't. In this case we only
	// want to allow a new update from the second node to allow the entire
	// edge to be resurrected.
	chanID := batch.chanAnn.ShortChannelID
	err = ctx.router.MarkEdgeZombie(
		chanID, [33]byte{}, batch.chanAnn.NodeID2,
	)
	if err != nil {
		t.Fatalf("unable mark channel %v as zombie: %v", chanID, err)
	}

	// If we send a new update but for the other direction of the channel,
	// then it should still be rejected as we want a fresh update from the
	// one that was considered stale.
	batch.chanUpdAnn1.Timestamp = uint32(time.Now().Unix())
	if err := signUpdate(remoteKeyPriv1, batch.chanUpdAnn1); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}
	processAnnouncement(batch.chanUpdAnn1, true, true)

	// At this point, the channel should still be considered a zombie.
	_, _, _, err = ctx.router.GetChannelByID(chanID)
	if err != channeldb.ErrZombieEdge {
		t.Fatalf("channel should still be a zombie")
	}

	// Attempting to process the current channel update should fail due to
	// its edge being considered a zombie and its timestamp not being within
	// the live horizon. We should not expect an error here since it is just
	// a stale update.
	processAnnouncement(batch.chanUpdAnn2, true, false)

	// Now we'll generate a new update with a fresh timestamp. This should
	// allow the channel update to be processed even though it is still
	// marked as a zombie within the index, since it is a fresh new update.
	// This won't work however since we'll sign it with the wrong private
	// key (remote key 1 rather than remote key 2).
	batch.chanUpdAnn2.Timestamp = uint32(time.Now().Unix())
	if err := signUpdate(remoteKeyPriv1, batch.chanUpdAnn2); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}

	// We should expect an error due to the signature being invalid.
	processAnnouncement(batch.chanUpdAnn2, true, true)

	// Signing it with the correct private key should allow it to be
	// processed.
	if err := signUpdate(remoteKeyPriv2, batch.chanUpdAnn2); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}

	// The channel update cannot be successfully processed and broadcast
	// until the channel announcement is. Since the channel update indicates
	// a fresh new update, the gossiper should stash it until it sees the
	// corresponding channel announcement.
	updateErrChan := ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	)

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("expected to not broadcast live channel update " +
			"without announcement")
	case <-time.After(2 * trickleDelay):
	}

	// We'll go ahead and process the channel announcement to ensure the
	// channel update is processed thereafter.
	processAnnouncement(batch.chanAnn, false, false)

	// After successfully processing the announcement, the channel update
	// should have been processed and broadcast successfully as well.
	select {
	case err := <-updateErrChan:
		if err != nil {
			t.Fatalf("expected to process live channel update: %v",
				err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected to process announcement")
	}

	select {
	case msgWithSenders := <-ctx.broadcastedMessage:
		assertMessage(t, batch.chanUpdAnn2, msgWithSenders.msg)
	case <-time.After(2 * trickleDelay):
		t.Fatal("expected to broadcast live channel update")
	}
}

// TestReceiveRemoteChannelUpdateFirst tests that if we receive a ChannelUpdate
// from the remote before we have processed our own ChannelAnnouncement, it will
// be reprocessed later, after our ChannelAnnouncement.
func TestReceiveRemoteChannelUpdateFirst(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// Override NotifyWhenOnline to return the remote peer which we expect
	// messages to be sent to.
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer [33]byte,
		peerChan chan<- lnpeer.Peer) {

		peerChan <- remotePeer
	}

	// Recreate the case where the remote node is sending us its ChannelUpdate
	// before we have been able to process our own ChannelAnnouncement and
	// ChannelUpdate.
	errRemoteAnn := ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	)
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2, remotePeer)
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Since the remote ChannelUpdate was added for an edge that
	// we did not already know about, it should have been added
	// to the map of premature ChannelUpdates. Check that nothing
	// was added to the graph.
	chanInfo, e1, e2, err := ctx.router.GetChannelByID(batch.chanUpdAnn1.ShortChannelID)
	if err != channeldb.ErrEdgeNotFound {
		t.Fatalf("Expected ErrEdgeNotFound, got: %v", err)
	}
	if chanInfo != nil {
		t.Fatalf("chanInfo was not nil")
	}
	if e1 != nil {
		t.Fatalf("e1 was not nil")
	}
	if e2 != nil {
		t.Fatalf("e2 was not nil")
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		assertMessage(t, batch.chanUpdAnn1, msg)
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	// At this point the remote ChannelUpdate we received earlier should
	// be reprocessed, as we now have the necessary edge entry in the graph.
	select {
	case err := <-errRemoteAnn:
		if err != nil {
			t.Fatalf("error re-processing remote update: %v", err)
		}
	case <-time.After(2 * trickleDelay):
		t.Fatalf("remote update was not processed")
	}

	// Check that the ChannelEdgePolicy was added to the graph.
	chanInfo, e1, e2, err = ctx.router.GetChannelByID(
		batch.chanUpdAnn1.ShortChannelID,
	)
	require.NoError(t, err, "unable to get channel from router")
	if chanInfo == nil {
		t.Fatalf("chanInfo was nil")
	}
	if e1 == nil {
		t.Fatalf("e1 was nil")
	}
	if e2 == nil {
		t.Fatalf("e2 was nil")
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 1 {
		t.Fatal("wrong number of objects in storage")
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 4; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
		},
		func() {
			number = 0
		},
	); err != nil && err != channeldb.ErrWaitingProofNotFound {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("waiting proof should be removed from storage")
	}
}

// TestExtraDataChannelAnnouncementValidation tests that we're able to properly
// validate a ChannelAnnouncement that includes opaque bytes that we don't
// currently know of.
func TestExtraDataChannelAnnouncementValidation(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	remotePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}

	// We'll now create an announcement that contains an extra set of bytes
	// that we don't know of ourselves, but should still include in the
	// final signature check.
	extraBytes := []byte("gotta validate this still!")
	ca, err := createRemoteChannelAnnouncement(0, extraBytes)
	require.NoError(t, err, "can't create channel announcement")

	// We'll now send the announcement to the main gossiper. We should be
	// able to validate this announcement to problem.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ca, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
}

// TestExtraDataChannelUpdateValidation tests that we're able to properly
// validate a ChannelUpdate that includes opaque bytes that we don't currently
// know of.
func TestExtraDataChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	timestamp := testTimestamp
	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	remotePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}

	// In this scenario, we'll create two announcements, one regular
	// channel announcement, and another channel update announcement, that
	// has additional data that we won't be interpreting.
	chanAnn, err := createRemoteChannelAnnouncement(0)
	require.NoError(t, err, "unable to create chan ann")
	chanUpdAnn1, err := createUpdateAnnouncement(
		0, 0, remoteKeyPriv1, timestamp,
		[]byte("must also validate"),
	)
	require.NoError(t, err, "unable to create chan up")
	chanUpdAnn2, err := createUpdateAnnouncement(
		0, 1, remoteKeyPriv2, timestamp,
		[]byte("must also validate"),
	)
	require.NoError(t, err, "unable to create chan up")

	// We should be able to properly validate all three messages without
	// any issue.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanAnn, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn1, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn2, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")
}

// TestExtraDataNodeAnnouncementValidation tests that we're able to properly
// validate a NodeAnnouncement that includes opaque bytes that we don't
// currently know of.
func TestExtraDataNodeAnnouncementValidation(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	remotePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}
	timestamp := testTimestamp

	// We'll create a node announcement that includes a set of opaque data
	// which we don't know of, but will store anyway in order to ensure
	// upgrades can flow smoothly in the future.
	nodeAnn, err := createNodeAnnouncement(
		remoteKeyPriv1, timestamp, []byte("gotta validate"),
	)
	require.NoError(t, err, "can't create node announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(nodeAnn, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")
}

// assertBroadcast checks that num messages are being broadcasted from the
// gossiper. The broadcasted messages are returned.
func assertBroadcast(t *testing.T, ctx *testCtx, num int) []lnwire.Message {
	t.Helper()

	var msgs []lnwire.Message
	for i := 0; i < num; i++ {
		select {
		case msg := <-ctx.broadcastedMessage:
			msgs = append(msgs, msg.msg)
		case <-time.After(time.Second):
			t.Fatalf("expected %d messages to be broadcast, only "+
				"got %d", num, i)
		}
	}

	// No more messages should be broadcast.
	select {
	case msg := <-ctx.broadcastedMessage:
		t.Fatalf("unexpected message was broadcast: %T", msg.msg)
	case <-time.After(2 * trickleDelay):
	}

	return msgs
}

// assertProcessAnnouncemnt is a helper method that checks that the result of
// processing an announcement is successful.
func assertProcessAnnouncement(t *testing.T, result chan error) {
	t.Helper()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("unable to process :%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not process announcement")
	}
}

// TestRetransmit checks that the expected announcements are retransmitted when
// the retransmit ticker ticks.
func TestRetransmit(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Process a local channel announcement, channel update and node
	// announcement. No messages should be broadcasted yet, since no proof
	// has been exchanged.
	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn),
	)
	assertBroadcast(t, ctx, 0)

	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1),
	)
	assertBroadcast(t, ctx, 0)

	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1),
	)
	assertBroadcast(t, ctx, 0)

	// Add the remote channel update to the gossiper. Similarly, nothing
	// should be broadcasted.
	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessRemoteAnnouncement(
			batch.chanUpdAnn2, remotePeer,
		),
	)
	assertBroadcast(t, ctx, 0)

	// Now add the local and remote proof to the gossiper, which should
	// trigger a broadcast of the announcements.
	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn),
	)
	assertBroadcast(t, ctx, 0)

	assertProcessAnnouncement(
		t, ctx.gossiper.ProcessRemoteAnnouncement(
			batch.remoteProofAnn, remotePeer,
		),
	)

	// checkAnncouncments make sure the expected number of channel
	// announcements + channel updates + node announcements are broadcast.
	checkAnnouncements := func(t *testing.T, chanAnns, chanUpds,
		nodeAnns int) {

		t.Helper()

		num := chanAnns + chanUpds + nodeAnns
		anns := assertBroadcast(t, ctx, num)

		// Count the received announcements.
		var chanAnn, chanUpd, nodeAnn int
		for _, msg := range anns {
			switch msg.(type) {
			case *lnwire.ChannelAnnouncement:
				chanAnn++
			case *lnwire.ChannelUpdate:
				chanUpd++
			case *lnwire.NodeAnnouncement:
				nodeAnn++
			}
		}

		if chanAnn != chanAnns || chanUpd != chanUpds ||
			nodeAnn != nodeAnns {

			t.Fatalf("unexpected number of announcements: "+
				"chanAnn=%d, chanUpd=%d, nodeAnn=%d",
				chanAnn, chanUpd, nodeAnn)
		}
	}

	// All announcements should be broadcast, including the remote channel
	// update.
	checkAnnouncements(t, 1, 2, 1)

	// Now let the retransmit ticker tick, which should trigger updates to
	// be rebroadcast.
	now := time.Unix(int64(testTimestamp), 0)
	future := now.Add(rebroadcastInterval + 10*time.Second)
	select {
	case ctx.gossiper.cfg.RetransmitTicker.(*ticker.Force).Force <- future:
	case <-time.After(2 * time.Second):
		t.Fatalf("unable to force tick")
	}

	// The channel announcement + local channel update + node announcement
	// should be re-broadcast.
	checkAnnouncements(t, 1, 1, 1)
}

// TestNodeAnnouncementNoChannels tests that NodeAnnouncements for nodes with
// no existing channels in the graph do not get forwarded.
func TestNodeAnnouncementNoChannels(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	batch, err := createRemoteAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Process the remote node announcement.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	// Since no channels or node announcements were already in the graph,
	// the node announcement should be ignored, and not forwarded.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Now add the node's channel to the graph by processing the channel
	// announcement and channel update.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	// Now process the node announcement again.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	// This time the node announcement should be forwarded. The same should
	// the channel announcement and update be.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	// Processing the same node announcement again should be ignored, as it
	// is stale.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}
}

// TestOptionalFieldsChannelUpdateValidation tests that we're able to properly
// validate the msg flags and max HTLC field of a ChannelUpdate.
func TestOptionalFieldsChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "can't create context")

	processRemoteAnnouncement := ctx.gossiper.ProcessRemoteAnnouncement

	chanUpdateHeight := uint32(0)
	timestamp := uint32(123456)
	nodePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}

	// In this scenario, we'll test whether the message flags field in a
	// channel update is properly handled.
	chanAnn, err := createRemoteChannelAnnouncement(chanUpdateHeight)
	require.NoError(t, err, "can't create channel announcement")

	select {
	case err = <-processRemoteAnnouncement(chanAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process announcement")

	// The first update should fail from an invalid max HTLC field, which is
	// less than the min HTLC.
	chanUpdAnn, err := createUpdateAnnouncement(
		0, 0, remoteKeyPriv1, timestamp,
	)
	require.NoError(t, err, "unable to create channel update")

	chanUpdAnn.HtlcMinimumMsat = 5000
	chanUpdAnn.HtlcMaximumMsat = 4000
	if err := signUpdate(remoteKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-processRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid max htlc") {
		t.Fatalf("expected chan update to error, instead got %v", err)
	}

	// The second update should fail because the message flag is set but
	// the max HTLC field is 0.
	chanUpdAnn.HtlcMinimumMsat = 0
	chanUpdAnn.HtlcMaximumMsat = 0
	if err := signUpdate(remoteKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-processRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid max htlc") {
		t.Fatalf("expected chan update to error, instead got %v", err)
	}

	// The third update should not succeed, a channel update with no message
	// flag set is invalid.
	chanUpdAnn.MessageFlags = 0
	if err := signUpdate(remoteKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-processRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.ErrorContains(t, err, "max htlc flag not set")

	// The final update should succeed.
	chanUpdAnn, err = createUpdateAnnouncement(
		0, 0, remoteKeyPriv1, timestamp,
	)
	require.NoError(t, err, "unable to create channel update")

	if err := signUpdate(remoteKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-processRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "expected update to be processed")
}

// TestSendChannelUpdateReliably ensures that the latest channel update for a
// channel is always sent upon the remote party reconnecting.
func TestSendChannelUpdateReliably(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test context and a batch of
	// announcements.
	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "unable to create test context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	// We'll also create two keys, one for ourselves and another for the
	// remote party.

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")

	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Since we first wait to be notified of the peer before attempting to
	// send the message, we'll overwrite NotifyWhenOnline and
	// NotifyWhenOffline to instead give us access to the channel that will
	// receive the notification.
	notifyOnline := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(_ [33]byte,
		peerChan chan<- lnpeer.Peer) {

		notifyOnline <- peerChan
	}
	notifyOffline := make(chan chan struct{}, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOffline = func(
		_ [33]byte) <-chan struct{} {

		c := make(chan struct{}, 1)
		notifyOffline <- c
		return c
	}

	// assertMsgSent is a helper closure we'll use to determine if the
	// correct gossip message was sent.
	assertMsgSent := func(msg lnwire.Message) {
		t.Helper()

		select {
		case msgSent := <-sentToPeer:
			assertMessage(t, msg, msgSent)
		case <-time.After(2 * time.Second):
			t.Fatalf("did not send %v message to peer",
				msg.MsgType())
		}
	}

	// Process the channel announcement for which we'll send a channel
	// update for.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel announcement")
	}
	require.NoError(t, err, "unable to process local channel announcement")

	// It should not be broadcast due to not having an announcement proof.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Now, we'll process the channel update.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel update")
	}
	require.NoError(t, err, "unable to process local channel update")

	// It should also not be broadcast due to the announcement not having an
	// announcement proof.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// It should however send it to the peer directly. In order to do so,
	// it'll request a notification for when the peer is online.
	var peerChan chan<- lnpeer.Peer
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not request notification upon peer " +
			"connection")
	}

	// We can go ahead and notify the peer, which should trigger the message
	// to be sent.
	peerChan <- remotePeer
	assertMsgSent(batch.chanUpdAnn1)

	// The gossiper should now request a notification for when the peer
	// disconnects. We'll also trigger this now.
	var offlineChan chan struct{}
	select {
	case offlineChan = <-notifyOffline:
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not request notification upon peer " +
			"disconnection")
	}

	close(offlineChan)

	// Since it's offline, the gossiper should request another notification
	// for when it comes back online.
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not request notification upon peer " +
			"connection")
	}

	// Now that the remote peer is offline, we'll send a new channel update.
	batch.chanUpdAnn1.Timestamp++
	if err := signUpdate(selfKeyPriv, batch.chanUpdAnn1); err != nil {
		t.Fatalf("unable to sign new channel update: %v", err)
	}

	// With the new update created, we'll go ahead and process it.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel update")
	}
	require.NoError(t, err, "unable to process local channel update")

	// It should also not be broadcast due to the announcement not having an
	// announcement proof.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The message should not be sent since the peer remains offline.
	select {
	case msg := <-sentToPeer:
		t.Fatalf("received unexpected message: %v", spew.Sdump(msg))
	case <-time.After(time.Second):
	}

	// Once again, we'll notify the peer is online and ensure the new
	// channel update is received. This will also cause an offline
	// notification to be requested again.
	peerChan <- remotePeer
	assertMsgSent(batch.chanUpdAnn1)

	select {
	case offlineChan = <-notifyOffline:
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not request notification upon peer " +
			"disconnection")
	}

	// We'll then exchange proofs with the remote peer in order to announce
	// the channel.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel proof")
	}
	require.NoError(t, err, "unable to process local channel proof")

	// No messages should be broadcast as we don't have the full proof yet.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Our proof should be sent to the remote peer however.
	assertMsgSent(batch.localProofAnn)

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote channel proof")
	}
	require.NoError(t, err, "unable to process remote channel proof")

	// Now that we've constructed our full proof, we can assert that the
	// channel has been announced.
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(2 * trickleDelay):
			t.Fatal("expected channel to be announced")
		}
	}

	// With the channel announced, we'll generate a new channel update. This
	// one won't take the path of the reliable sender, as the channel has
	// already been announced. We'll keep track of the old message that is
	// now stale to use later on.
	staleChannelUpdate := batch.chanUpdAnn1
	newChannelUpdate := &lnwire.ChannelUpdate{}
	*newChannelUpdate = *staleChannelUpdate
	newChannelUpdate.Timestamp++
	if err := signUpdate(selfKeyPriv, newChannelUpdate); err != nil {
		t.Fatalf("unable to sign new channel update: %v", err)
	}

	// Process the new channel update. It should not be sent to the peer
	// directly since the reliable sender only applies when the channel is
	// not announced.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		newChannelUpdate,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel update")
	}
	require.NoError(t, err, "unable to process local channel update")
	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("channel update was not broadcast")
	}
	select {
	case msg := <-sentToPeer:
		t.Fatalf("received unexpected message: %v", spew.Sdump(msg))
	case <-time.After(time.Second):
	}

	// Then, we'll trigger the reliable sender to send its pending messages
	// by triggering an offline notification for the peer, followed by an
	// online one.
	close(offlineChan)

	select {
	case peerChan = <-notifyOnline:
	case <-time.After(2 * time.Second):
		t.Fatal("gossiper did not request notification upon peer " +
			"connection")
	}

	peerChan <- remotePeer

	// At this point, we should have sent both the AnnounceSignatures and
	// stale ChannelUpdate.
	for i := 0; i < 2; i++ {
		var msg lnwire.Message
		select {
		case msg = <-sentToPeer:
		case <-time.After(time.Second):
			t.Fatal("expected to send message")
		}

		switch msg := msg.(type) {
		case *lnwire.ChannelUpdate:
			assertMessage(t, staleChannelUpdate, msg)
		case *lnwire.AnnounceSignatures:
			assertMessage(t, batch.localProofAnn, msg)
		default:
			t.Fatalf("send unexpected %v message", msg.MsgType())
		}
	}

	// Since the messages above are now deemed as stale, they should be
	// removed from the message store.
	err = wait.NoError(func() error {
		msgs, err := ctx.gossiper.cfg.MessageStore.Messages()
		if err != nil {
			return fmt.Errorf("unable to retrieve pending "+
				"messages: %v", err)
		}
		if len(msgs) != 0 {
			return fmt.Errorf("expected no messages left, found %d",
				len(msgs))
		}
		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func sendLocalMsg(t *testing.T, ctx *testCtx, msg lnwire.Message,
	optionalMsgFields ...OptionalMsgField) {

	t.Helper()

	var err error
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		msg, optionalMsgFields...,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel msg")
}

func sendRemoteMsg(t *testing.T, ctx *testCtx, msg lnwire.Message,
	remotePeer lnpeer.Peer) {

	t.Helper()

	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(msg, remotePeer):
		if err != nil {
			t.Fatalf("unable to process channel msg: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
}

func assertBroadcastMsg(t *testing.T, ctx *testCtx,
	predicate func(lnwire.Message) error) {

	t.Helper()

	// We don't care about the order of the broadcast, only that our target
	// predicate returns true for any of the messages, so we'll continue to
	// retry until either we hit our timeout, or it returns with no error
	// (message found).
	err := wait.NoError(func() error {
		select {
		case msg := <-ctx.broadcastedMessage:
			return predicate(msg.msg)
		case <-time.After(2 * trickleDelay):
			return fmt.Errorf("no message broadcast")
		}
	}, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
}

// TestPropagateChanPolicyUpdate tests that we're able to issue requests to
// update policies for all channels and also select target channels.
// Additionally, we ensure that we don't propagate updates for any private
// channels.
func TestPropagateChanPolicyUpdate(t *testing.T) {
	t.Parallel()

	// First, we'll make out test context and add 3 random channels to the
	// graph.
	startingHeight := uint32(10)
	ctx, err := createTestCtx(t, startingHeight)
	require.NoError(t, err, "unable to create test context")

	const numChannels = 3
	channelsToAnnounce := make([]*annBatch, 0, numChannels)
	for i := 0; i < numChannels; i++ {
		newChan, err := createLocalAnnouncements(uint32(i + 1))
		if err != nil {
			t.Fatalf("unable to make new channel ann: %v", err)
		}

		channelsToAnnounce = append(channelsToAnnounce, newChan)
	}

	remoteKey := remoteKeyPriv1.PubKey()

	sentMsgs := make(chan lnwire.Message, 10)
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// The forced code path for sending the private ChannelUpdate to the
	// remote peer will be hit, forcing it to request a notification that
	// the remote peer is active. We'll ensure that it targets the proper
	// pubkey, and hand it our mock peer above.
	notifyErr := make(chan error, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(
		targetPub [33]byte, peerChan chan<- lnpeer.Peer) {

		if !bytes.Equal(targetPub[:], remoteKey.SerializeCompressed()) {
			notifyErr <- fmt.Errorf("reliableSender attempted to send the "+
				"message to the wrong peer: expected %x got %x",
				remoteKey.SerializeCompressed(),
				targetPub)
		}

		peerChan <- remotePeer
	}

	// With our channel announcements created, we'll now send them all to
	// the gossiper in order for it to process. However, we'll hold back
	// the channel ann proof from the first channel in order to have it be
	// marked as private channel.
	firstChanID := channelsToAnnounce[0].chanAnn.ShortChannelID
	for i, batch := range channelsToAnnounce {
		// channelPoint ensures that each channel policy in the map
		// returned by PropagateChanPolicyUpdate has a unique key. Since
		// the map is keyed by wire.OutPoint, we want to ensure that
		// each channel has a unique channel point.
		channelPoint := ChannelPoint(wire.OutPoint{Index: uint32(i)})

		sendLocalMsg(t, ctx, batch.chanAnn, channelPoint)
		sendLocalMsg(t, ctx, batch.chanUpdAnn1)
		sendLocalMsg(t, ctx, batch.nodeAnn1)

		sendRemoteMsg(t, ctx, batch.chanUpdAnn2, remotePeer)
		sendRemoteMsg(t, ctx, batch.nodeAnn2, remotePeer)

		// We'll skip sending the auth proofs from the first channel to
		// ensure that it's seen as a private channel.
		if batch.chanAnn.ShortChannelID == firstChanID {
			continue
		}

		sendLocalMsg(t, ctx, batch.localProofAnn)
		sendRemoteMsg(t, ctx, batch.remoteProofAnn, remotePeer)
	}

	// Drain out any broadcast or direct messages we might not have read up
	// to this point. We'll also check out notifyErr to detect if the
	// reliable sender had an issue sending to the remote peer.
out:
	for {
		select {
		case <-ctx.broadcastedMessage:
		case <-sentMsgs:
		case err := <-notifyErr:
			t.Fatal(err)

		// Give it 5 seconds to drain out.
		case <-time.After(5 * time.Second):
			break out
		}
	}

	// Now that all of our channels are loaded, we'll attempt to update the
	// policy of all of them.
	const newTimeLockDelta = 100
	var edgesToUpdate []EdgeWithInfo
	err = ctx.router.ForAllOutgoingChannels(func(
		_ kvdb.RTx,
		info *channeldb.ChannelEdgeInfo,
		edge *channeldb.ChannelEdgePolicy) error {

		edge.TimeLockDelta = uint16(newTimeLockDelta)
		edgesToUpdate = append(edgesToUpdate, EdgeWithInfo{
			Info: info,
			Edge: edge,
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = ctx.gossiper.PropagateChanPolicyUpdate(edgesToUpdate)
	require.NoError(t, err, "unable to chan policies")

	// Two channel updates should now be broadcast, with neither of them
	// being the channel our first private channel.
	for i := 0; i < numChannels-1; i++ {
		assertBroadcastMsg(t, ctx, func(msg lnwire.Message) error {
			upd, ok := msg.(*lnwire.ChannelUpdate)
			if !ok {
				return fmt.Errorf("channel update not "+
					"broadcast, instead %T was", msg)
			}

			if upd.ShortChannelID == firstChanID {
				return fmt.Errorf("private channel upd " +
					"broadcast")
			}
			if upd.TimeLockDelta != newTimeLockDelta {
				return fmt.Errorf("wrong delta: expected %v, "+
					"got %v", newTimeLockDelta,
					upd.TimeLockDelta)
			}

			return nil
		})
	}

	// Finally the ChannelUpdate should have been sent directly to the
	// remote peer via the reliable sender.
	select {
	case msg := <-sentMsgs:
		upd, ok := msg.(*lnwire.ChannelUpdate)
		if !ok {
			t.Fatalf("channel update not "+
				"broadcast, instead %T was", msg)
		}
		if upd.TimeLockDelta != newTimeLockDelta {
			t.Fatalf("wrong delta: expected %v, "+
				"got %v", newTimeLockDelta,
				upd.TimeLockDelta)
		}
		if upd.ShortChannelID != firstChanID {
			t.Fatalf("private channel upd " +
				"broadcast")
		}
	case <-time.After(time.Second * 5):
		t.Fatalf("message not sent directly to peer")
	}

	// At this point, no other ChannelUpdate messages should be broadcast
	// as we sent the two public ones to the network, and the private one
	// was sent directly to the peer.
	for {
		select {
		case msg := <-ctx.broadcastedMessage:
			if upd, ok := msg.msg.(*lnwire.ChannelUpdate); ok {
				if upd.ShortChannelID == firstChanID {
					t.Fatalf("chan update msg received: %v",
						spew.Sdump(msg))
				}
			}
		default:
			return
		}
	}
}

// TestProcessChannelAnnouncementOptionalMsgFields ensures that the gossiper can
// properly handled optional message fields provided by the caller when
// processing a channel announcement.
func TestProcessChannelAnnouncementOptionalMsgFields(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test context and a set of test channel
	// announcements.
	ctx, err := createTestCtx(t, 0)
	require.NoError(t, err, "unable to create test context")

	chanAnn1 := createAnnouncementWithoutProof(
		100, selfKeyDesc.PubKey, remoteKeyPub1,
	)
	chanAnn2 := createAnnouncementWithoutProof(
		101, selfKeyDesc.PubKey, remoteKeyPub1,
	)

	// assertOptionalMsgFields is a helper closure that ensures the optional
	// message fields were set as intended.
	assertOptionalMsgFields := func(chanID lnwire.ShortChannelID,
		capacity btcutil.Amount, channelPoint wire.OutPoint) {

		t.Helper()

		edge, _, _, err := ctx.router.GetChannelByID(chanID)
		if err != nil {
			t.Fatalf("unable to get channel by id: %v", err)
		}
		if edge.Capacity != capacity {
			t.Fatalf("expected capacity %v, got %v", capacity,
				edge.Capacity)
		}
		if edge.ChannelPoint != channelPoint {
			t.Fatalf("expected channel point %v, got %v",
				channelPoint, edge.ChannelPoint)
		}
	}

	// We'll process the first announcement without any optional fields. We
	// should see the channel's capacity and outpoint have a zero value.
	sendLocalMsg(t, ctx, chanAnn1)
	assertOptionalMsgFields(chanAnn1.ShortChannelID, 0, wire.OutPoint{})

	// Providing the capacity and channel point as optional fields should
	// propagate them all the way down to the router.
	capacity := btcutil.Amount(1000)
	channelPoint := wire.OutPoint{Index: 1}
	sendLocalMsg(
		t, ctx, chanAnn2, ChannelCapacity(capacity),
		ChannelPoint(channelPoint),
	)
	assertOptionalMsgFields(chanAnn2.ShortChannelID, capacity, channelPoint)
}

func assertMessage(t *testing.T, expected, got lnwire.Message) {
	t.Helper()

	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("expected: %v\ngot: %v", spew.Sdump(expected),
			spew.Sdump(got))
	}
}

// TestSplitAnnouncementsCorrectSubBatches checks that we split a given
// sizes of announcement list into the correct number of batches.
func TestSplitAnnouncementsCorrectSubBatches(t *testing.T) {
	// Create our test harness.
	const blockHeight = 100
	ctx, err := createTestCtx(t, blockHeight)
	require.NoError(t, err, "can't create context")

	const subBatchSize = 10

	announcementBatchSizes := []int{2, 5, 20, 45, 80, 100, 1005}
	expectedNumberMiniBatches := []int{1, 1, 2, 5, 8, 10, 101}

	lengthAnnouncementBatchSizes := len(announcementBatchSizes)
	lengthExpectedNumberMiniBatches := len(expectedNumberMiniBatches)

	batchSizeCalculator = func(totalDelay, subBatchDelay time.Duration,
		minimumBatchSize, batchSize int) int {

		return subBatchSize
	}

	if lengthAnnouncementBatchSizes != lengthExpectedNumberMiniBatches {
		t.Fatal("Length of announcementBatchSizes and " +
			"expectedNumberMiniBatches should be equal")
	}

	for testIndex := range announcementBatchSizes {
		var batchSize = announcementBatchSizes[testIndex]
		announcementBatch := make([]msgWithSenders, batchSize)

		splitAnnouncementBatch := ctx.gossiper.splitAnnouncementBatches(
			announcementBatch,
		)

		lengthMiniBatches := len(splitAnnouncementBatch)

		if lengthMiniBatches != expectedNumberMiniBatches[testIndex] {
			t.Fatalf("Expecting %d mini batches, actual %d",
				expectedNumberMiniBatches[testIndex],
				lengthMiniBatches)
		}
	}
}

func assertCorrectSubBatchSize(t *testing.T, expectedSubBatchSize,
	actualSubBatchSize int) {

	t.Helper()

	if actualSubBatchSize != expectedSubBatchSize {
		t.Fatalf("Expecting subBatch size of %d, actual %d",
			expectedSubBatchSize, actualSubBatchSize)
	}
}

// TestCalculateCorrectSubBatchSize checks that we check the correct
// sub batch size for each of the input vectors of batch sizes.
func TestCalculateCorrectSubBatchSizes(t *testing.T) {
	t.Parallel()

	const minimumSubBatchSize = 10
	const batchDelay = time.Duration(100)
	const subBatchDelay = time.Duration(10)

	batchSizes := []int{2, 200, 250, 305, 352, 10010, 1000001}
	expectedSubBatchSize := []int{10, 20, 25, 31, 36, 1001, 100001}

	for testIndex := range batchSizes {
		batchSize := batchSizes[testIndex]
		expectedBatchSize := expectedSubBatchSize[testIndex]

		actualSubBatchSize := calculateSubBatchSize(
			batchDelay, subBatchDelay, minimumSubBatchSize, batchSize,
		)

		assertCorrectSubBatchSize(t, expectedBatchSize, actualSubBatchSize)
	}
}

// TestCalculateCorrectSubBatchSizesDifferentDelay checks that we check the
// correct sub batch size for each of different delay.
func TestCalculateCorrectSubBatchSizesDifferentDelay(t *testing.T) {
	t.Parallel()

	const batchSize = 100
	const minimumSubBatchSize = 10

	batchDelays := []time.Duration{100, 50, 20, 25, 5, 0}
	const subBatchDelay = 10

	expectedSubBatchSize := []int{10, 20, 50, 40, 100, 100}

	for testIndex := range batchDelays {
		batchDelay := batchDelays[testIndex]
		expectedBatchSize := expectedSubBatchSize[testIndex]

		actualSubBatchSize := calculateSubBatchSize(
			batchDelay, subBatchDelay, minimumSubBatchSize, batchSize,
		)

		assertCorrectSubBatchSize(t, expectedBatchSize, actualSubBatchSize)
	}
}

// markGraphSynced allows us to report that the initial historical sync has
// completed.
func (m *SyncManager) markGraphSyncing() {
	atomic.StoreInt32(&m.initialHistoricalSyncCompleted, 0)
}

// TestBroadcastAnnsAfterGraphSynced ensures that we only broadcast
// announcements after the graph has been considered as synced, i.e., after our
// initial historical sync has completed.
func TestBroadcastAnnsAfterGraphSynced(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, 10)
	require.NoError(t, err, "can't create context")

	// We'll mark the graph as not synced. This should prevent us from
	// broadcasting any messages we've received as part of our initial
	// historical sync.
	ctx.gossiper.syncMgr.markGraphSyncing()

	assertBroadcast := func(msg lnwire.Message, isRemote bool,
		shouldBroadcast bool) {

		t.Helper()

		nodePeer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}
		var errChan chan error
		if isRemote {
			errChan = ctx.gossiper.ProcessRemoteAnnouncement(
				msg, nodePeer,
			)
		} else {
			errChan = ctx.gossiper.ProcessLocalAnnouncement(msg)
		}

		select {
		case err := <-errChan:
			if err != nil {
				t.Fatalf("unable to process gossip message: %v",
					err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("gossip message not processed")
		}

		select {
		case <-ctx.broadcastedMessage:
			if !shouldBroadcast {
				t.Fatal("gossip message was broadcast")
			}
		case <-time.After(2 * trickleDelay):
			if shouldBroadcast {
				t.Fatal("gossip message wasn't broadcast")
			}
		}
	}

	// A remote channel announcement should not be broadcast since the graph
	// has not yet been synced.
	chanAnn1, err := createRemoteChannelAnnouncement(0)
	require.NoError(t, err, "unable to create channel announcement")
	assertBroadcast(chanAnn1, true, false)

	// A local channel announcement should be broadcast though, regardless
	// of whether we've synced our graph or not.
	chanUpd, err := createUpdateAnnouncement(0, 0, remoteKeyPriv1, 1)
	require.NoError(t, err, "unable to create channel announcement")
	assertBroadcast(chanUpd, false, true)

	// Mark the graph as synced, which should allow the channel announcement
	// should to be broadcast.
	ctx.gossiper.syncMgr.markGraphSynced()

	chanAnn2, err := createRemoteChannelAnnouncement(1)
	require.NoError(t, err, "unable to create channel announcement")
	assertBroadcast(chanAnn2, true, true)
}

// TestRateLimitChannelUpdates ensures that we properly rate limit incoming
// channel updates.
func TestRateLimitChannelUpdates(t *testing.T) {
	t.Parallel()

	// Create our test harness.
	const blockHeight = 100
	ctx, err := createTestCtx(t, blockHeight)
	require.NoError(t, err, "can't create context")
	ctx.gossiper.cfg.RebroadcastInterval = time.Hour
	ctx.gossiper.cfg.MaxChannelUpdateBurst = 5
	ctx.gossiper.cfg.ChannelUpdateInterval = 5 * time.Second

	// The graph should start empty.
	require.Empty(t, ctx.router.infos)
	require.Empty(t, ctx.router.edges)

	// We'll create a batch of signed announcements, including updates for
	// both sides, for a channel and process them. They should all be
	// forwarded as this is our first time learning about the channel.
	batch, err := createRemoteAnnouncements(blockHeight)
	require.NoError(t, err)

	nodePeer1 := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil}
	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanAnn, nodePeer1,
	):
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("remote announcement not processed")
	}

	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn1, nodePeer1,
	):
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("remote announcement not processed")
	}

	nodePeer2 := &mockPeer{remoteKeyPriv2.PubKey(), nil, nil}
	select {
	case err := <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, nodePeer2,
	):
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("remote announcement not processed")
	}

	timeout := time.After(2 * trickleDelay)
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-timeout:
			t.Fatal("expected announcement to be broadcast")
		}
	}

	shortChanID := batch.chanAnn.ShortChannelID.ToUint64()
	require.Contains(t, ctx.router.infos, shortChanID)
	require.Contains(t, ctx.router.edges, shortChanID)

	// We'll define a helper to assert whether updates should be rate
	// limited or not depending on their contents.
	assertRateLimit := func(update *lnwire.ChannelUpdate, peer lnpeer.Peer,
		shouldRateLimit bool) {

		t.Helper()

		select {
		case err := <-ctx.gossiper.ProcessRemoteAnnouncement(update, peer):
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("remote announcement not processed")
		}

		select {
		case <-ctx.broadcastedMessage:
			if shouldRateLimit {
				t.Fatal("unexpected channel update broadcast")
			}
		case <-time.After(2 * trickleDelay):
			if !shouldRateLimit {
				t.Fatal("expected channel update broadcast")
			}
		}
	}

	// We'll start with the keep alive case.
	//
	// We rate limit any keep alive updates that have not at least spanned
	// our rebroadcast interval.
	rateLimitKeepAliveUpdate := *batch.chanUpdAnn1
	rateLimitKeepAliveUpdate.Timestamp++
	require.NoError(t, signUpdate(remoteKeyPriv1, &rateLimitKeepAliveUpdate))
	assertRateLimit(&rateLimitKeepAliveUpdate, nodePeer1, true)

	keepAliveUpdate := *batch.chanUpdAnn1
	keepAliveUpdate.Timestamp = uint32(
		time.Unix(int64(batch.chanUpdAnn1.Timestamp), 0).
			Add(ctx.gossiper.cfg.RebroadcastInterval).Unix(),
	)
	require.NoError(t, signUpdate(remoteKeyPriv1, &keepAliveUpdate))
	assertRateLimit(&keepAliveUpdate, nodePeer1, false)

	// Then, we'll move on to the non keep alive cases.
	//
	// For this test, non keep alive updates are rate limited to one per 5
	// seconds with a max burst of 5 per direction. We'll process the max
	// burst of one direction first. None of these should be rate limited.
	updateSameDirection := keepAliveUpdate
	for i := uint32(0); i < uint32(ctx.gossiper.cfg.MaxChannelUpdateBurst); i++ {
		updateSameDirection.Timestamp++
		updateSameDirection.BaseFee++
		require.NoError(t, signUpdate(remoteKeyPriv1, &updateSameDirection))
		assertRateLimit(&updateSameDirection, nodePeer1, false)
	}

	// Following with another update should be rate limited as the max burst
	// has been reached and we haven't ticked at the next interval yet.
	updateSameDirection.Timestamp++
	updateSameDirection.BaseFee++
	require.NoError(t, signUpdate(remoteKeyPriv1, &updateSameDirection))
	assertRateLimit(&updateSameDirection, nodePeer1, true)

	// An update for the other direction should not be rate limited.
	updateDiffDirection := *batch.chanUpdAnn2
	updateDiffDirection.Timestamp++
	updateDiffDirection.BaseFee++
	require.NoError(t, signUpdate(remoteKeyPriv2, &updateDiffDirection))
	assertRateLimit(&updateDiffDirection, nodePeer2, false)

	// Wait for the next interval to tick. Since we've only waited for one,
	// only one more update is allowed.
	<-time.After(ctx.gossiper.cfg.ChannelUpdateInterval)
	for i := 0; i < ctx.gossiper.cfg.MaxChannelUpdateBurst; i++ {
		updateSameDirection.Timestamp++
		updateSameDirection.BaseFee++
		require.NoError(t, signUpdate(remoteKeyPriv1, &updateSameDirection))

		shouldRateLimit := i != 0
		assertRateLimit(&updateSameDirection, nodePeer1, shouldRateLimit)
	}
}

// TestIgnoreOwnAnnouncement tests that the gossiper will ignore announcements
// about our own channels when coming from a remote peer.
func TestIgnoreOwnAnnouncement(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	batch, err := createLocalAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Try to let the remote peer tell us about the channel we are part of.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	// It should be ignored, since the gossiper only cares about local
	// announcements for its own channels.
	if err == nil || !strings.Contains(err.Error(), "ignoring") {
		t.Fatalf("expected gossiper to ignore announcement, got: %v", err)
	}

	// Now do the local channelannouncement, node announcement, and channel
	// update. No messages should be brodcasted yet, since we don't have
	// the announcement signatures.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// We should accept the remote's channel update and node announcement.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process channel update")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.nodeAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process node ann")
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Now we exchange the proofs, the messages will be broadcasted to the
	// network.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process local proof")

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	require.NoError(t, err, "unable to process remote proof")

	for i := 0; i < 5; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	// Finally, we again check that we'll ignore the remote giving us
	// announcements about our own channel.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err == nil || !strings.Contains(err.Error(), "ignoring") {
		t.Fatalf("expected gossiper to ignore announcement, got: %v", err)
	}
}

// TestRejectCacheChannelAnn checks that if we reject a channel announcement,
// then if we attempt to validate it again, we'll reject it with the proper
// error.
func TestRejectCacheChannelAnn(t *testing.T) {
	t.Parallel()

	ctx, err := createTestCtx(t, proofMatureDelta)
	require.NoError(t, err, "can't create context")

	// First, we create a channel announcement to send over to our test
	// peer.
	batch, err := createRemoteAnnouncements(0)
	require.NoError(t, err, "can't generate announcements")

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:])
	require.NoError(t, err, "unable to parse pubkey")
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Before sending over the announcement, we'll modify it such that we
	// know it will always fail.
	chanID := batch.chanAnn.ShortChannelID.ToUint64()
	ctx.router.queueValidationFail(chanID)

	// If we process the batch the first time we should get an error.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanAnn, remotePeer,
	):
		require.NotNil(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}

	// If we process it a *second* time, then we should get an error saying
	// we rejected it already.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanAnn, remotePeer,
	):
		errStr := err.Error()
		require.Contains(t, errStr, "recently rejected")
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
}

// TestFutureMsgCacheEviction checks that when the cache's capacity is reached,
// saving one more item will evict the oldest item.
func TestFutureMsgCacheEviction(t *testing.T) {
	t.Parallel()

	// Create a future message cache with size 1.
	c := newFutureMsgCache(1)

	// Send two messages to the cache, which ends in the first message
	// being evicted.
	//
	// Put the first item.
	id := c.nextMsgID()
	evicted, err := c.Put(id, &cachedFutureMsg{height: uint32(id)})
	require.NoError(t, err)
	require.False(t, evicted, "should not be evicted")

	// Put the second item.
	id = c.nextMsgID()
	evicted, err = c.Put(id, &cachedFutureMsg{height: uint32(id)})
	require.NoError(t, err)
	require.True(t, evicted, "should be evicted")

	// The first item should have been evicted.
	//
	// NOTE: msg ID starts at 1, not 0.
	_, err = c.Get(1)
	require.ErrorIs(t, err, cache.ErrElementNotFound)

	// The second item should be found.
	item, err := c.Get(2)
	require.NoError(t, err)
	require.EqualValues(t, 2, item.height, "should be the second item")
}
