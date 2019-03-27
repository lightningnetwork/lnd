package discovery

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/big"
	prand "math/rand"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	testAddrs    = []net.Addr{testAddr}
	testFeatures = lnwire.NewRawFeatureVector()
	testSig      = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	inputStr = "147caa76786596590baa4e98f5d9f48b86c7765e489f7a6ff3360fe5c674360b"
	sha, _   = chainhash.NewHashFromStr(inputStr)
	outpoint = wire.NewOutPoint(sha, 0)

	bitcoinKeyPriv1, _ = btcec.NewPrivateKey(btcec.S256())
	bitcoinKeyPub1     = bitcoinKeyPriv1.PubKey()

	nodeKeyPriv1, _ = btcec.NewPrivateKey(btcec.S256())
	nodeKeyPub1     = nodeKeyPriv1.PubKey()

	bitcoinKeyPriv2, _ = btcec.NewPrivateKey(btcec.S256())
	bitcoinKeyPub2     = bitcoinKeyPriv2.PubKey()

	nodeKeyPriv2, _ = btcec.NewPrivateKey(btcec.S256())
	nodeKeyPub2     = nodeKeyPriv2.PubKey()

	trickleDelay        = time.Millisecond * 100
	retransmitDelay     = time.Hour * 1
	proofMatureDelta    uint32
	maxBtcFundingAmount = btcutil.Amount(1<<62) - 1
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

type mockSigner struct {
	privKey *btcec.PrivateKey
}

func (n *mockSigner) SignMessage(pubKey *btcec.PublicKey,
	msg []byte) (*btcec.Signature, error) {

	if !pubKey.IsEqual(n.privKey.PubKey()) {
		return nil, fmt.Errorf("unknown public key")
	}

	digest := chainhash.DoubleHashB(msg)
	sign, err := n.privKey.Sign(digest)
	if err != nil {
		return nil, fmt.Errorf("can't sign the message: %v", err)
	}

	return sign, nil
}

type mockGraphSource struct {
	bestHeight uint32

	mu      sync.Mutex
	nodes   []channeldb.LightningNode
	infos   map[uint64]channeldb.ChannelEdgeInfo
	edges   map[uint64][]channeldb.ChannelEdgePolicy
	zombies map[uint64][][33]byte
}

func newMockRouter(height uint32) *mockGraphSource {
	return &mockGraphSource{
		bestHeight: height,
		infos:      make(map[uint64]channeldb.ChannelEdgeInfo),
		edges:      make(map[uint64][]channeldb.ChannelEdgePolicy),
		zombies:    make(map[uint64][][33]byte),
	}
}

var _ routing.ChannelGraphSource = (*mockGraphSource)(nil)

func (r *mockGraphSource) AddNode(node *channeldb.LightningNode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes = append(r.nodes, *node)
	return nil
}

func (r *mockGraphSource) AddEdge(info *channeldb.ChannelEdgeInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.infos[info.ChannelID]; ok {
		return errors.New("info already exist")
	}

	// Usually, the capacity is fetched in the router from the funding txout.
	// Since the mockGraphSource can't access the txout, assign a default value.
	info.Capacity = maxBtcFundingAmount
	r.infos[info.ChannelID] = *info
	return nil
}

func (r *mockGraphSource) UpdateEdge(edge *channeldb.ChannelEdgePolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.edges[edge.ChannelID] = append(r.edges[edge.ChannelID], *edge)
	return nil
}

func (r *mockGraphSource) SelfEdges() ([]*channeldb.ChannelEdgePolicy, error) {
	return nil, nil
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

func (r *mockGraphSource) ForAllOutgoingChannels(cb func(i *channeldb.ChannelEdgeInfo,
	c *channeldb.ChannelEdgePolicy) error) error {
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

	if len(edges) == 1 {
		edge1 := edges[0]
		return &chanInfo, &edge1, nil, nil
	}

	edge1, edge2 := edges[0], edges[1]
	return &chanInfo, &edge1, &edge2, nil
}

func (r *mockGraphSource) FetchLightningNode(
	nodePub routing.Vertex) (*channeldb.LightningNode, error) {

	for _, node := range r.nodes {
		if bytes.Equal(nodePub[:], node.PubKeyBytes[:]) {
			return &node, nil
		}
	}

	return nil, channeldb.ErrGraphNodeNotFound
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
func (r *mockGraphSource) IsStaleNode(nodePub routing.Vertex, timestamp time.Time) bool {
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
func (r *mockGraphSource) IsPublicNode(node routing.Vertex) (bool, error) {
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
	case len(edges) >= 1 && edges[0].ChannelFlags == flags:
		return !edges[0].LastUpdate.Before(timestamp)

	case len(edges) >= 2 && edges[1].ChannelFlags == flags:
		return !edges[1].LastUpdate.Before(timestamp)

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
	_ []byte, numConfs, _ uint32) (*chainntnfs.ConfirmationEvent, error) {

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

func (m *mockNotifier) Stop() error {
	return nil
}

type annBatch struct {
	nodeAnn1 *lnwire.NodeAnnouncement
	nodeAnn2 *lnwire.NodeAnnouncement

	localChanAnn  *lnwire.ChannelAnnouncement
	remoteChanAnn *lnwire.ChannelAnnouncement

	chanUpdAnn1 *lnwire.ChannelUpdate
	chanUpdAnn2 *lnwire.ChannelUpdate

	localProofAnn  *lnwire.AnnounceSignatures
	remoteProofAnn *lnwire.AnnounceSignatures
}

func createAnnouncements(blockHeight uint32) (*annBatch, error) {
	var err error
	var batch annBatch
	timestamp := uint32(123456)

	batch.nodeAnn1, err = createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		return nil, err
	}

	batch.nodeAnn2, err = createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		return nil, err
	}

	batch.remoteChanAnn, err = createRemoteChannelAnnouncement(blockHeight)
	if err != nil {
		return nil, err
	}

	batch.localProofAnn = &lnwire.AnnounceSignatures{
		NodeSignature:    batch.remoteChanAnn.NodeSig1,
		BitcoinSignature: batch.remoteChanAnn.BitcoinSig1,
	}

	batch.remoteProofAnn = &lnwire.AnnounceSignatures{
		NodeSignature:    batch.remoteChanAnn.NodeSig2,
		BitcoinSignature: batch.remoteChanAnn.BitcoinSig2,
	}

	batch.localChanAnn, err = createRemoteChannelAnnouncement(blockHeight)
	if err != nil {
		return nil, err
	}

	batch.chanUpdAnn1, err = createUpdateAnnouncement(
		blockHeight, 0, nodeKeyPriv1, timestamp,
	)
	if err != nil {
		return nil, err
	}

	batch.chanUpdAnn2, err = createUpdateAnnouncement(
		blockHeight, 1, nodeKeyPriv2, timestamp,
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

	signer := mockSigner{priv}
	sig, err := SignAnnouncement(&signer, priv.PubKey(), a)
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
		MessageFlags:    lnwire.ChanUpdateOptionMaxHtlc,
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
	pub := nodeKey.PubKey()
	signer := mockSigner{nodeKey}
	sig, err := SignAnnouncement(&signer, pub, a)
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
	extraBytes ...[]byte) *lnwire.ChannelAnnouncement {

	a := &lnwire.ChannelAnnouncement{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
			TxIndex:     0,
			TxPosition:  0,
		},
		Features: testFeatures,
	}
	copy(a.NodeID1[:], nodeKeyPub1.SerializeCompressed())
	copy(a.NodeID2[:], nodeKeyPub2.SerializeCompressed())
	copy(a.BitcoinKey1[:], bitcoinKeyPub1.SerializeCompressed())
	copy(a.BitcoinKey2[:], bitcoinKeyPub2.SerializeCompressed())
	if len(extraBytes) == 1 {
		a.ExtraOpaqueData = extraBytes[0]
	}

	return a
}

func createRemoteChannelAnnouncement(blockHeight uint32,
	extraBytes ...[]byte) (*lnwire.ChannelAnnouncement, error) {

	a := createAnnouncementWithoutProof(blockHeight, extraBytes...)

	pub := nodeKeyPriv1.PubKey()
	signer := mockSigner{nodeKeyPriv1}
	sig, err := SignAnnouncement(&signer, pub, a)
	if err != nil {
		return nil, err
	}
	a.NodeSig1, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	pub = nodeKeyPriv2.PubKey()
	signer = mockSigner{nodeKeyPriv2}
	sig, err = SignAnnouncement(&signer, pub, a)
	if err != nil {
		return nil, err
	}
	a.NodeSig2, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv1.PubKey()
	signer = mockSigner{bitcoinKeyPriv1}
	sig, err = SignAnnouncement(&signer, pub, a)
	if err != nil {
		return nil, err
	}
	a.BitcoinSig1, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv2.PubKey()
	signer = mockSigner{bitcoinKeyPriv2}
	sig, err = SignAnnouncement(&signer, pub, a)
	if err != nil {
		return nil, err
	}
	a.BitcoinSig2, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	return a, nil
}

type testCtx struct {
	gossiper           *AuthenticatedGossiper
	router             *mockGraphSource
	notifier           *mockNotifier
	broadcastedMessage chan msgWithSenders
}

func createTestCtx(startHeight uint32) (*testCtx, func(), error) {
	// Next we'll initialize an instance of the channel router with mock
	// versions of the chain and channel notifier. As we don't need to test
	// any p2p functionality, the peer send and switch send,
	// broadcast functions won't be populated.
	notifier := newMockNotifier()
	router := newMockRouter(startHeight)

	db, cleanUpDb, err := makeTestDB()
	if err != nil {
		return nil, nil, err
	}

	waitingProofStore, err := channeldb.NewWaitingProofStore(db)
	if err != nil {
		cleanUpDb()
		return nil, nil, err
	}

	broadcastedMessage := make(chan msgWithSenders, 10)
	gossiper := New(Config{
		Notifier: notifier,
		Broadcast: func(senders map[routing.Vertex]struct{},
			msgs ...lnwire.Message) error {

			for _, msg := range msgs {
				broadcastedMessage <- msgWithSenders{
					msg:     msg,
					senders: senders,
				}
			}

			return nil
		},
		NotifyWhenOnline: func(target *btcec.PublicKey,
			peerChan chan<- lnpeer.Peer) {
			peerChan <- &mockPeer{target, nil, nil}
		},
		NotifyWhenOffline: func(_ [33]byte) <-chan struct{} {
			c := make(chan struct{})
			return c
		},
		Router:            router,
		TrickleDelay:      trickleDelay,
		RetransmitDelay:   retransmitDelay,
		ProofMatureDelta:  proofMatureDelta,
		WaitingProofStore: waitingProofStore,
		MessageStore:      newMockMessageStore(),
	}, nodeKeyPub1)

	if err := gossiper.Start(); err != nil {
		cleanUpDb()
		return nil, nil, fmt.Errorf("unable to start router: %v", err)
	}

	cleanUp := func() {
		gossiper.Stop()
		cleanUpDb()
	}

	return &testCtx{
		router:             router,
		notifier:           notifier,
		gossiper:           gossiper,
		broadcastedMessage: broadcastedMessage,
	}, cleanUp, nil
}

// TestProcessAnnouncement checks that mature announcements are propagated to
// the router subsystem.
func TestProcessAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := uint32(123456)

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	assertSenderExistence := func(sender *btcec.PublicKey, msg msgWithSenders) {
		if _, ok := msg.senders[routing.NewVertex(sender)]; !ok {
			t.Fatalf("sender=%x not present in %v",
				sender.SerializeCompressed(), spew.Sdump(msg))
		}
	}

	nodePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}

	// First, we'll craft a valid remote channel announcement and send it to
	// the gossiper so that it can be processed.
	ca, err := createRemoteChannelAnnouncement(0)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ca, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

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

	// We'll then craft the channel policy of the remote party and also send
	// it to the gossiper.
	ua, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(ua, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

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
	na, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(na, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("remote announcement not processed")
	}
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

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

// TestPrematureAnnouncement checks that premature announcements are
// not propagated to the router subsystem until block with according
// block height received.
func TestPrematureAnnouncement(t *testing.T) {
	t.Parallel()

	timestamp := uint32(123456)

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	_, err = createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	nodePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}

	// Pretending that we receive the valid channel announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be added to the
	// repeat/premature batch.
	ca, err := createRemoteChannelAnnouncement(1)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ca, nodePeer):
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.infos) != 0 {
		t.Fatal("edge was added to router")
	}

	// Pretending that we receive the valid channel update announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be added to the
	// repeat/premature batch.
	ua, err := createUpdateAnnouncement(1, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ua, nodePeer):
		t.Fatal("announcement was proceeded")
	case <-time.After(100 * time.Millisecond):
	}

	if len(ctx.router.edges) != 0 {
		t.Fatal("edge update was added to router")
	}

	// Generate new block and waiting the previously added announcements
	// to be proceeded.
	newBlock := &wire.MsgBlock{}
	ctx.notifier.notifyBlock(newBlock.Header.BlockHash(), 1)

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcement wasn't broadcasted")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge wasn't added to router: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcement wasn't broadcasted")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestSignatureAnnouncementLocalFirst ensures that the AuthenticatedGossiper
// properly processes partial and fully announcement signatures message.
func TestSignatureAnnouncementLocalFirst(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(target *btcec.PublicKey,
		peerChan chan<- lnpeer.Peer) {

		select {
		case peerChan <- &mockPeer{target, sentMsgs, ctx.gossiper.quit}:
		case <-ctx.gossiper.quit:
		}
	}

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localChanAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.nodeAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
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

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.chanUpdAnn2, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process local proof: %v", err)
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
	if err != nil {
		t.Fatalf("unable to process remote proof: %v", err)
	}

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

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(target *btcec.PublicKey,
		peerChan chan<- lnpeer.Peer) {

		select {
		case peerChan <- &mockPeer{target, sentMsgs, ctx.gossiper.quit}:
		case <-ctx.gossiper.quit:
		}
	}

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to proceed announcement: %v", err)
	}

	number := 0
	if err := ctx.gossiper.cfg.WaitingProofStore.ForAll(
		func(*channeldb.WaitingProof) error {
			number++
			return nil
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
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}

	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.nodeAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
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

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// After that we process local announcement, and waiting to receive
	// the channel announcement.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

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

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	// Set up a channel to intercept the messages sent to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Override NotifyWhenOnline to return the remote peer which we expect
	// meesages to be sent to.
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		peerChan chan<- lnpeer.Peer) {

		peerChan <- remotePeer
	}

	// Override NotifyWhenOffline to return the channel which will notify
	// the gossiper that the peer is offline. We'll use this to signal that
	// the peer is offline so that the gossiper requests a notification when
	// it comes back online.
	notifyOffline := make(chan chan struct{}, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOffline = func(
		_ [33]byte) <-chan struct{} {

		c := make(chan struct{})
		notifyOffline <- c
		return c
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localChanAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}
	select {
	case msg := <-sentToPeer:
		assertMessage(t, batch.chanUpdAnn1, msg)
	case <-time.After(1 * time.Second):
		t.Fatal("gossiper did not send channel update to peer")
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.nodeAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
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
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
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
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Since the reliable send to the remote peer of the local channel proof
	// requires a notification when the peer comes online, we'll capture the
	// channel through which it gets sent to control exactly when to
	// dispatch it.
	notifyPeers := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- lnpeer.Peer) {
		notifyPeers <- connectedChan
	}

	// Before sending the local channel proof, we'll notify that the peer is
	// offline, so that it's not sent to the peer.
	var peerOffline chan struct{}
	select {
	case peerOffline = <-notifyOffline:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not request notification for when " +
			"peer disconnects")
	}
	close(peerOffline)

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
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
	gossiper := New(Config{
		Notifier:          ctx.gossiper.cfg.Notifier,
		Broadcast:         ctx.gossiper.cfg.Broadcast,
		NotifyWhenOnline:  ctx.gossiper.reliableSender.cfg.NotifyWhenOnline,
		NotifyWhenOffline: ctx.gossiper.reliableSender.cfg.NotifyWhenOffline,
		Router:            ctx.gossiper.cfg.Router,
		TrickleDelay:      trickleDelay,
		RetransmitDelay:   retransmitDelay,
		ProofMatureDelta:  proofMatureDelta,
		WaitingProofStore: ctx.gossiper.cfg.WaitingProofStore,
		MessageStore:      ctx.gossiper.cfg.MessageStore,
	}, ctx.gossiper.selfKey)
	if err != nil {
		t.Fatalf("unable to recreate gossiper: %v", err)
	}
	if err := gossiper.Start(); err != nil {
		t.Fatalf("unable to start recreated gossiper: %v", err)
	}
	defer gossiper.Stop()

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

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Override NotifyWhenOnline to return the remote peer which we expect
	// meesages to be sent to.
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		peerChan chan<- lnpeer.Peer) {

		peerChan <- remotePeer
	}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localChanAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
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
		batch.nodeAnn1, localKey,
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
	if err != nil {
		t.Fatalf("unable to process channel update: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process local proof: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(
		batch.remoteProofAnn, remotePeer,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process remote proof: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to process remote proof: %v", err)
	}

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

	timestamp := uint32(123456)
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
	if err != nil {
		t.Fatalf("can't create remote channel announcement: %v", err)
	}

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
	if err != nil {
		t.Fatalf("can't create remote channel announcement: %v", err)
	}
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
	ua, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
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
	ua2, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
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
	ua3, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp+1)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
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
	ua4, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}
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
	na, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{
		msg:    na,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 1 {
		t.Fatal("new node announcement not stored in batch")
	}

	// We'll now add another node to the batch.
	na2, err := createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
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
	na3, err := createNodeAnnouncement(nodeKeyPriv2, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
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
	newNodeKeyPointer := nodeKeyPriv2
	na4, err := createNodeAnnouncement(newNodeKeyPointer, timestamp)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
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
	na5, err := createNodeAnnouncement(nodeKeyPriv2, timestamp+1)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}
	announcements.AddMsgs(networkMsg{
		msg:    na5,
		peer:   nodePeer,
		source: nodePeer.IdentityKey(),
	})
	if len(announcements.nodeAnnouncements) != 2 {
		t.Fatal("node announcement not replaced in batch")
	}
	nodeID := routing.NewVertex(nodeKeyPriv2.PubKey())
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
	if len(batch) != 4 {
		t.Fatal("announcement batch incorrect length")
	}

	if !reflect.DeepEqual(batch[0].msg, ca2) {
		t.Fatalf("channel announcement not first in batch: got %v, "+
			"expected %v", spew.Sdump(batch[0].msg), spew.Sdump(ca2))
	}

	if !reflect.DeepEqual(batch[1].msg, ua3) {
		t.Fatalf("channel update not next in batch: got %v, "+
			"expected %v", spew.Sdump(batch[1].msg), spew.Sdump(ua2))
	}

	// We'll ensure that both node announcements are present. We check both
	// indexes as due to the randomized order of map iteration they may be
	// in either place.
	if !reflect.DeepEqual(batch[2].msg, na) && !reflect.DeepEqual(batch[3].msg, na) {
		t.Fatal("first node announcement not in last part of batch: "+
			"got %v, expected %v", batch[2].msg,
			na)
	}
	if !reflect.DeepEqual(batch[2].msg, na5) && !reflect.DeepEqual(batch[3].msg, na5) {
		t.Fatalf("second node announcement not in last part of batch: "+
			"got %v, expected %v", batch[3].msg,
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

	ctx, cleanup, err := createTestCtx(startingHeight)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// We'll start off by processing a channel announcement without a proof
	// (i.e., an unadvertised channel), followed by a node announcement for
	// this same channel announcement.
	chanAnn := createAnnouncementWithoutProof(startingHeight - 2)
	pubKey := nodeKeyPriv1.PubKey()

	select {
	case err := <-ctx.gossiper.ProcessLocalAnnouncement(chanAnn, pubKey):
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

	nodeAnn, err := createNodeAnnouncement(nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("unable to create node announcement: %v", err)
	}

	select {
	case err := <-ctx.gossiper.ProcessLocalAnnouncement(nodeAnn, pubKey):
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
	if err != nil {
		t.Fatalf("unable to create remote channel announcement: %v", err)
	}
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
	nodeAnn, err = createNodeAnnouncement(nodeKeyPriv1, timestamp+1)
	if err != nil {
		t.Fatalf("unable to create node announcement: %v", err)
	}

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
	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("unable to create test context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("unable to create announcements: %v", err)
	}
	remotePeer := &mockPeer{pk: nodeKeyPriv2.PubKey()}

	// processAnnouncements is a helper closure we'll use to test that we
	// properly process/reject announcements based on whether they're for a
	// zombie edge or not.
	processAnnouncements := func(isZombie bool) {
		t.Helper()

		errChan := ctx.gossiper.ProcessRemoteAnnouncement(
			batch.remoteChanAnn, remotePeer,
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
	chanID := batch.remoteChanAnn.ShortChannelID
	err = ctx.router.MarkEdgeZombie(
		chanID, batch.remoteChanAnn.NodeID1, batch.remoteChanAnn.NodeID2,
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
	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("unable to create test context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("unable to create announcements: %v", err)
	}

	localPrivKey := nodeKeyPriv1
	remotePrivKey := nodeKeyPriv2

	remotePeer := &mockPeer{pk: remotePrivKey.PubKey()}

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
	if err := signUpdate(remotePrivKey, batch.chanUpdAnn2); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}

	// We'll also add the edge to our zombie index.
	chanID := batch.remoteChanAnn.ShortChannelID
	err = ctx.router.MarkEdgeZombie(
		chanID, batch.remoteChanAnn.NodeID1, batch.remoteChanAnn.NodeID2,
	)
	if err != nil {
		t.Fatalf("unable mark channel %v as zombie: %v", chanID, err)
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
	// key (local rather than remote).
	batch.chanUpdAnn2.Timestamp = uint32(time.Now().Unix())
	if err := signUpdate(localPrivKey, batch.chanUpdAnn2); err != nil {
		t.Fatalf("unable to sign update with new timestamp: %v", err)
	}

	// We should expect an error due to the signature being invalid.
	processAnnouncement(batch.chanUpdAnn2, true, true)

	// Signing it with the correct private key should allow it to be
	// processed.
	if err := signUpdate(remotePrivKey, batch.chanUpdAnn2); err != nil {
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
	processAnnouncement(batch.remoteChanAnn, false, false)

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

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	// Set up a channel that we can use to inspect the messages sent
	// directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	remotePeer := &mockPeer{remoteKey, sentMsgs, ctx.gossiper.quit}

	// Override NotifyWhenOnline to return the remote peer which we expect
	// meesages to be sent to.
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
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
	if err != nil {
		t.Fatalf("unable to process node ann: %v", err)
	}
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
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn1, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.nodeAnn1, localKey)
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
	if err != nil {
		t.Fatalf("unable to get channel from router: %v", err)
	}
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
	err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localProofAnn, localKey,
	)
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

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	remotePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}

	// We'll now create an announcement that contains an extra set of bytes
	// that we don't know of ourselves, but should still include in the
	// final signature check.
	extraBytes := []byte("gotta validate this stil!")
	ca, err := createRemoteChannelAnnouncement(0, extraBytes)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

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

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	remotePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}
	timestamp := uint32(123456)

	// In this scenario, we'll create two announcements, one regular
	// channel announcement, and another channel update announcement, that
	// has additional data that we won't be interpreting.
	chanAnn, err := createRemoteChannelAnnouncement(0)
	if err != nil {
		t.Fatalf("unable to create chan ann: %v", err)
	}
	chanUpdAnn1, err := createUpdateAnnouncement(
		0, 0, nodeKeyPriv1, timestamp,
		[]byte("must also validate"),
	)
	if err != nil {
		t.Fatalf("unable to create chan up: %v", err)
	}
	chanUpdAnn2, err := createUpdateAnnouncement(
		0, 1, nodeKeyPriv2, timestamp,
		[]byte("must also validate"),
	)
	if err != nil {
		t.Fatalf("unable to create chan up: %v", err)
	}

	// We should be able to properly validate all three messages without
	// any issue.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanAnn, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn1, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn2, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}
}

// TestExtraDataNodeAnnouncementValidation tests that we're able to properly
// validate a NodeAnnouncement that includes opaque bytes that we don't
// currently know of.
func TestExtraDataNodeAnnouncementValidation(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	remotePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}
	timestamp := uint32(123456)

	// We'll create a node announcement that includes a set of opaque data
	// which we don't know of, but will store anyway in order to ensure
	// upgrades can flow smoothly in the future.
	nodeAnn, err := createNodeAnnouncement(
		nodeKeyPriv1, timestamp, []byte("gotta validate"),
	)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(nodeAnn, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}
}

// TestNodeAnnouncementNoChannels tests that NodeAnnouncements for nodes with
// no existing channels in the graph do not get forwarded.
func TestNodeAnnouncementNoChannels(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:],
		btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Process the remote node announcement.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	// Since no channels or node announcements were already in the graph,
	// the node announcement should be ignored, and not forwarded.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Now add the node's channel to the graph by processing the channel
	// announement and channel update.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteChanAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	// Now process the node announcement again.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2, remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	// This time the node announcement should be forwarded. The same should
	// the channel announcement and update be.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	// Processing the same node announement again should be ignored, as it
	// is stale.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.nodeAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("node announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}
}

// TestOptionalFieldsChannelUpdateValidation tests that we're able to properly
// validate the msg flags and optional max HTLC field of a ChannelUpdate.
func TestOptionalFieldsChannelUpdateValidation(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	chanUpdateHeight := uint32(0)
	timestamp := uint32(123456)
	nodePeer := &mockPeer{nodeKeyPriv1.PubKey(), nil, nil}

	// In this scenario, we'll test whether the message flags field in a channel
	// update is properly handled.
	chanAnn, err := createRemoteChannelAnnouncement(chanUpdateHeight)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}

	// The first update should fail from an invalid max HTLC field, which is
	// less than the min HTLC.
	chanUpdAnn, err := createUpdateAnnouncement(0, 0, nodeKeyPriv1, timestamp)
	if err != nil {
		t.Fatalf("unable to create channel update: %v", err)
	}

	chanUpdAnn.HtlcMinimumMsat = 5000
	chanUpdAnn.HtlcMaximumMsat = 4000
	if err := signUpdate(nodeKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn, nodePeer):
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
	if err := signUpdate(nodeKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err == nil || !strings.Contains(err.Error(), "invalid max htlc") {
		t.Fatalf("expected chan update to error, instead got %v", err)
	}

	// The final update should succeed, since setting the flag 0 means the
	// nonsense max_htlc field will just be ignored.
	chanUpdAnn.MessageFlags = 0
	if err := signUpdate(nodeKeyPriv1, chanUpdAnn); err != nil {
		t.Fatalf("unable to sign channel update: %v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(chanUpdAnn, nodePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process announcement: %v", err)
	}
}

// TestSendChannelUpdateReliably ensures that the latest channel update for a
// channel is always sent upon the remote party reconnecting.
func TestSendChannelUpdateReliably(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test context and a batch of
	// announcements.
	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("unable to create test context: %v", err)
	}
	defer cleanup()

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	// We'll also create two keys, one for ourselves and another for the
	// remote party.
	localKey, err := btcec.ParsePubKey(batch.nodeAnn1.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}
	remoteKey, err := btcec.ParsePubKey(batch.nodeAnn2.NodeID[:], btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer := &mockPeer{remoteKey, sentToPeer, ctx.gossiper.quit}

	// Since we first wait to be notified of the peer before attempting to
	// send the message, we'll overwrite NotifyWhenOnline and
	// NotifyWhenOffline to instead give us access to the channel that will
	// receive the notification.
	notifyOnline := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(_ *btcec.PublicKey,
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

	// assertReceivedChannelUpdate is a helper closure we'll use to
	// determine if the correct channel update was received.
	assertReceivedChannelUpdate := func(channelUpdate *lnwire.ChannelUpdate) {
		t.Helper()

		select {
		case msg := <-sentToPeer:
			assertMessage(t, batch.chanUpdAnn1, msg)
		case <-time.After(2 * time.Second):
			t.Fatal("did not send local channel update to peer")
		}
	}

	// Process the channel announcement for which we'll send a channel
	// update for.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.localChanAnn, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel announcement")
	}
	if err != nil {
		t.Fatalf("unable to process local channel announcement: %v", err)
	}

	// It should not be broadcast due to not having an announcement proof.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Now, we'll process the channel update.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel update")
	}
	if err != nil {
		t.Fatalf("unable to process local channel update: %v", err)
	}

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
	assertReceivedChannelUpdate(batch.chanUpdAnn1)

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
	prevTimestamp := batch.chanUpdAnn1.Timestamp
	newChanUpdate, err := createUpdateAnnouncement(
		0, 0, nodeKeyPriv1, prevTimestamp+1,
	)
	if err != nil {
		t.Fatalf("unable to create new channel update: %v", err)
	}

	// With the new update created, we'll go ahead and process it.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(
		batch.chanUpdAnn1, localKey,
	):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local channel update")
	}
	if err != nil {
		t.Fatalf("unable to process local channel update: %v", err)
	}

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

	// Finally, we'll notify the peer is online and ensure the new channel
	// update is received.
	peerChan <- remotePeer
	assertReceivedChannelUpdate(newChanUpdate)
}

func assertMessage(t *testing.T, expected, got lnwire.Message) {
	t.Helper()

	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("expected: %v\ngot: %v", spew.Sdump(expected),
			spew.Sdump(got))
	}
}
