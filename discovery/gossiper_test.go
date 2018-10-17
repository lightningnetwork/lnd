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
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

	trickleDelay     = time.Millisecond * 100
	retransmitDelay  = time.Hour * 1
	proofMatureDelta uint32
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
	nodes      []*channeldb.LightningNode
	infos      map[uint64]*channeldb.ChannelEdgeInfo
	edges      map[uint64][]*channeldb.ChannelEdgePolicy
	bestHeight uint32
}

func newMockRouter(height uint32) *mockGraphSource {
	return &mockGraphSource{
		bestHeight: height,
		infos:      make(map[uint64]*channeldb.ChannelEdgeInfo),
		edges:      make(map[uint64][]*channeldb.ChannelEdgePolicy),
	}
}

var _ routing.ChannelGraphSource = (*mockGraphSource)(nil)

func (r *mockGraphSource) AddNode(node *channeldb.LightningNode) error {
	r.nodes = append(r.nodes, node)
	return nil
}

func (r *mockGraphSource) AddEdge(info *channeldb.ChannelEdgeInfo) error {
	if _, ok := r.infos[info.ChannelID]; ok {
		return errors.New("info already exist")
	}
	r.infos[info.ChannelID] = info
	return nil
}

func (r *mockGraphSource) UpdateEdge(edge *channeldb.ChannelEdgePolicy) error {
	r.edges[edge.ChannelID] = append(
		r.edges[edge.ChannelID],
		edge,
	)
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
	info, ok := r.infos[chanID.ToUint64()]
	if !ok {
		return errors.New("channel does not exist")
	}
	info.AuthProof = proof
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

	chanInfo, ok := r.infos[chanID.ToUint64()]
	if !ok {
		return nil, nil, nil, channeldb.ErrEdgeNotFound
	}

	edges := r.edges[chanID.ToUint64()]
	if len(edges) == 0 {
		return chanInfo, nil, nil, nil
	}

	if len(edges) == 1 {
		return chanInfo, edges[0], nil, nil
	}

	return chanInfo, edges[0], edges[1], nil
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
func (r *mockGraphSource) IsStaleNode(nodePub routing.Vertex, timestamp time.Time) bool {
	for _, node := range r.nodes {
		if node.PubKeyBytes == nodePub {
			return node.LastUpdate.After(timestamp) ||
				node.LastUpdate.Equal(timestamp)
		}
	}

	return false
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
// channel ID.
func (r *mockGraphSource) IsKnownEdge(chanID lnwire.ShortChannelID) bool {
	_, ok := r.infos[chanID.ToUint64()]
	return ok
}

// IsStaleEdgePolicy returns true if the graph source has a channel edge for
// the passed channel ID (and flags) that have a more recent timestamp.
func (r *mockGraphSource) IsStaleEdgePolicy(chanID lnwire.ShortChannelID,
	timestamp time.Time, flags lnwire.ChanUpdateFlag) bool {

	edges, ok := r.edges[chanID.ToUint64()]
	if !ok {
		return false
	}

	switch {

	case len(edges) >= 1 && edges[0].Flags == flags:
		return !edges[0].LastUpdate.Before(timestamp)

	case len(edges) >= 2 && edges[1].Flags == flags:
		return !edges[1].LastUpdate.Before(timestamp)

	default:
		return false
	}
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

func createUpdateAnnouncement(blockHeight uint32, flags lnwire.ChanUpdateFlag,
	nodeKey *btcec.PrivateKey, timestamp uint32,
	extraBytes ...[]byte) (*lnwire.ChannelUpdate, error) {

	var err error

	a := &lnwire.ChannelUpdate{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		Timestamp:       timestamp,
		TimeLockDelta:   uint16(prand.Int63()),
		Flags:           flags,
		HtlcMinimumMsat: lnwire.MilliSatoshi(prand.Int63()),
		FeeRate:         uint32(prand.Int31()),
		BaseFee:         uint32(prand.Int31()),
	}
	if len(extraBytes) == 1 {
		a.ExtraOpaqueData = extraBytes[0]
	}

	pub := nodeKey.PubKey()
	signer := mockSigner{nodeKey}
	sig, err := SignAnnouncement(&signer, pub, a)
	if err != nil {
		return nil, err
	}

	a.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return nil, err
	}

	return a, nil
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

	broadcastedMessage := make(chan msgWithSenders, 10)
	gossiper, err := New(Config{
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
		SendToPeer: func(target *btcec.PublicKey, msg ...lnwire.Message) error {
			return nil
		},
		FindPeer: func(target *btcec.PublicKey) (lnpeer.Peer, error) {
			return &mockPeer{target, nil, nil}, nil
		},
		Router:           router,
		TrickleDelay:     trickleDelay,
		RetransmitDelay:  retransmitDelay,
		ProofMatureDelta: proofMatureDelta,
		DB:               db,
	}, nodeKeyPub1)
	if err != nil {
		cleanUpDb()
		return nil, nil, fmt.Errorf("unable to create router %v", err)
	}
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

	// Set up a channel that we can use to inspect the messages
	// sent directly fromn the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.FindPeer = func(target *btcec.PublicKey) (lnpeer.Peer, error) {
		return &mockPeer{target, sentMsgs, ctx.gossiper.quit}, nil
	}
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
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
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
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
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
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
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

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

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

	// Set up a channel that we can use to inspect the messages
	// sent directly from the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.FindPeer = func(target *btcec.PublicKey) (lnpeer.Peer, error) {
		return &mockPeer{target, sentMsgs, ctx.gossiper.quit}, nil
	}
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
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
	if err := ctx.gossiper.waitingProofs.ForAll(
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

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
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
		t.Fatalf("unable to process: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
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
		if msg != batch.localProofAnn {
			t.Fatalf("expected local proof to be sent, got %v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("local proof was not sent to peer")
	}

	// And since both remote and local announcements are processed, we
	// should be broadcasting the final channel announcements.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

// Test that sending AnnounceSignatures to remote peer will continue
// to be tried until the peer comes online.
func TestSignatureAnnouncementRetry(t *testing.T) {
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
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
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
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Make the SendToPeer fail, simulating the peer being offline.
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}

	// We expect the gossiper to register for a notification when the peer
	// comes back online, so keep track of the channel it wants to get
	// notified on.
	notifyPeers := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- lnpeer.Peer) {
		notifyPeers <- connectedChan
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// Since sending this local announcement proof to the remote will fail,
	// the gossiper should register for a notification when the remote is
	// online again.
	var conChan chan<- lnpeer.Peer
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// Since both proofs are not yet exchanged, no message should be
	// broadcasted yet.
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

	// When the peer comes online, the gossiper gets notified, and should
	// retry sending the AnnounceSignatures. We make the SendToPeer
	// method work again.
	sentToPeer := make(chan lnwire.Message, 1)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		sentToPeer <- msg[0]
		return nil
	}

	// Notify that peer is now online. This should trigger a new call
	// to SendToPeer.
	close(conChan)

	select {
	case <-sentToPeer:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not send message when peer came online")
	}

	// Now give the gossiper the remote proof. This should trigger a
	// broadcast of 3 messages (ChannelAnnouncement + 2 ChannelUpdate).
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

// Test that if we restart the gossiper, it will retry sending the
// AnnounceSignatures to the peer if it did not succeed before
// shutting down, and the full channel proof is not yet assembled.
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
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
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
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	// Make the SendToPeerFail, simulating the peer being offline.
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	notifyPeers := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- lnpeer.Peer) {
		notifyPeers <- connectedChan
	}

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

	// Since sending to the remote peer will fail, the gossiper should
	// register for a notification when it comes back online.
	var conChan chan<- lnpeer.Peer
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

	// Shut down gossiper, and restart. This should trigger a new attempt
	// to send the message to the peer.
	ctx.gossiper.Stop()
	gossiper, err := New(Config{
		Notifier:  ctx.gossiper.cfg.Notifier,
		Broadcast: ctx.gossiper.cfg.Broadcast,
		SendToPeer: func(target *btcec.PublicKey,
			msg ...lnwire.Message) error {
			return fmt.Errorf("intentional error in SendToPeer")
		},
		NotifyWhenOnline: func(peer *btcec.PublicKey,
			connectedChan chan<- lnpeer.Peer) {
			notifyPeers <- connectedChan
		},
		Router:           ctx.gossiper.cfg.Router,
		TrickleDelay:     trickleDelay,
		RetransmitDelay:  retransmitDelay,
		ProofMatureDelta: proofMatureDelta,
		DB:               ctx.gossiper.cfg.DB,
	}, ctx.gossiper.selfKey)
	if err != nil {
		t.Fatalf("unable to recreate gossiper: %v", err)
	}
	if err := gossiper.Start(); err != nil {
		t.Fatalf("unable to start recreated gossiper: %v", err)
	}
	defer gossiper.Stop()

	ctx.gossiper = gossiper

	// After starting up, the gossiper will see that it has a waitingproof
	// in the database, and will retry sending its part to the remote. Since
	// SendToPeer will fail again, it should register for a notification
	// when the peer comes online.
	select {
	case conChan = <-notifyPeers:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not ask to get notified when " +
			"peer is online")
	}

	// Fix the SendToPeer method.
	sentToPeer := make(chan lnwire.Message, 1)
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		select {
		case sentToPeer <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}

		return nil
	}
	// Notify that peer is now online. This should trigger a new call
	// to SendToPeer.
	close(conChan)

	select {
	case <-sentToPeer:
	case <-time.After(2 * time.Second):
		t.Fatalf("gossiper did not send message when peer came online")
	}

	// Now exchanging the remote channel proof, the channel announcement
	// broadcast should continue as normal.
	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

// TestSignatureAnnouncementFullProofWhenRemoteProof tests that if a
// remote proof is received when we already have the full proof,
// the gossiper will send the full proof (ChannelAnnouncement) to
// the remote peer.
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
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Recreate lightning network topology. Initialize router with channel
	// between two nodes.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
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
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process remote announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}
	// Set up a channel we can use to inspect messages sent by the
	// gossiper to the remote peer.
	sentToPeer := make(chan lnwire.Message, 1)
	remotePeer.sentMsgs = sentToPeer
	remotePeer.quit = ctx.gossiper.quit
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey,
		msg ...lnwire.Message) error {
		select {
		case <-ctx.gossiper.quit:
			return fmt.Errorf("gossiper shutting down")
		case sentToPeer <- msg[0]:
		}
		return nil
	}

	notifyPeers := make(chan chan<- lnpeer.Peer, 1)
	ctx.gossiper.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey,
		connectedChan chan<- lnpeer.Peer) {
		notifyPeers <- connectedChan
	}

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process.
	select {
	case err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn,
		localKey):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	// We expect the gossiper to send this message to the remote peer.
	select {
	case msg := <-sentToPeer:
		if msg != batch.localProofAnn {
			t.Fatalf("wrong message sent to peer: %v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not send local proof to peer")
	}

	// And all channel announcements should be broadcast.
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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
	case err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn,
		remotePeer):
	case <-time.After(2 * time.Second):
		t.Fatal("did not process local announcement")
	}
	if err != nil {
		t.Fatalf("unable to process :%v", err)
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
			ua3.Flags,
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

// TestReceiveRemoteChannelUpdateFirst tests that if we receive a
// ChannelUpdate from the remote before we have processed our
// own ChannelAnnouncement, it will be reprocessed later, after
// our ChannelAnnouncement.
func TestReceiveRemoteChannelUpdateFirst(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(uint32(proofMatureDelta))
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Set up a channel that we can use to inspect the messages
	// sent directly fromn the gossiper.
	sentMsgs := make(chan lnwire.Message, 10)
	ctx.gossiper.cfg.FindPeer = func(target *btcec.PublicKey) (lnpeer.Peer, error) {
		return &mockPeer{target, sentMsgs, ctx.gossiper.quit}, nil
	}
	ctx.gossiper.cfg.SendToPeer = func(target *btcec.PublicKey, msg ...lnwire.Message) error {
		select {
		case sentMsgs <- msg[0]:
		case <-ctx.gossiper.quit:
			return fmt.Errorf("shutting down")
		}
		return nil
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
	remotePeer := &mockPeer{remoteKey, nil, nil}

	// Recreate the case where the remote node is sending us its ChannelUpdate
	// before we have been able to process our own ChannelAnnouncement and
	// ChannelUpdate.
	errRemoteAnn := ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn2, remotePeer)

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
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

	// The local ChannelUpdate should now be sent directly to the remote peer,
	// such that the edge can be used for routing, regardless if this channel
	// is announced or not (private channel).
	select {
	case msg := <-sentMsgs:
		if msg != batch.chanUpdAnn1 {
			t.Fatalf("expected local channel update, instead got %v", msg)
		}
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
	chanInfo, e1, e2, err = ctx.router.GetChannelByID(batch.chanUpdAnn1.ShortChannelID)
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
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("announcements were broadcast")
	case <-time.After(2 * trickleDelay):
	}

	number := 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn, remotePeer)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-ctx.broadcastedMessage:
		case <-time.After(time.Second):
			t.Fatal("announcement wasn't broadcast")
		}
	}

	number = 0
	if err := ctx.gossiper.waitingProofs.ForAll(
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

// mockPeer implements the lnpeer.Peer interface and is used to test the
// gossiper's interaction with peers.
type mockPeer struct {
	pk       *btcec.PublicKey
	sentMsgs chan lnwire.Message
	quit     chan struct{}
}

var _ lnpeer.Peer = (*mockPeer)(nil)

func (p *mockPeer) SendMessage(_ bool, msgs ...lnwire.Message) error {
	if p.sentMsgs == nil && p.quit == nil {
		return nil
	}

	for _, msg := range msgs {
		select {
		case p.sentMsgs <- msg:
		case <-p.quit:
		}
	}

	return nil
}
func (p *mockPeer) AddNewChannel(_ *channeldb.OpenChannel, _ <-chan struct{}) error {
	return nil
}
func (p *mockPeer) WipeChannel(_ *wire.OutPoint) error { return nil }
func (p *mockPeer) IdentityKey() *btcec.PublicKey      { return p.pk }
func (p *mockPeer) PubKey() [33]byte {
	var pubkey [33]byte
	copy(pubkey[:], p.pk.SerializeCompressed())
	return pubkey
}
func (p *mockPeer) Address() net.Addr { return nil }
func (p *mockPeer) QuitSignal() <-chan struct{} {
	return p.quit
}
