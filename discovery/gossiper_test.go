package discovery

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	prand "math/rand"

	"testing"

	"math/big"

	"time"

	"io/ioutil"
	"os"

	"github.com/go-errors/errors"
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
		return nil, nil, nil, errors.New("can't find channel info")
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
	numConfs, _ uint32) (*chainntnfs.ConfirmationEvent, error) {

	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ uint32) (*chainntnfs.SpendEvent, error) {
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

type annBatch struct {
	nodeAnn1       *lnwire.NodeAnnouncement
	nodeAnn2       *lnwire.NodeAnnouncement
	localChanAnn   *lnwire.ChannelAnnouncement
	remoteChanAnn  *lnwire.ChannelAnnouncement
	chanUpdAnn     *lnwire.ChannelUpdate
	localProofAnn  *lnwire.AnnounceSignatures
	remoteProofAnn *lnwire.AnnounceSignatures
}

func createAnnouncements(blockHeight uint32) (*annBatch, error) {
	var err error
	var batch annBatch

	batch.nodeAnn1, err = createNodeAnnouncement(nodeKeyPriv1)
	if err != nil {
		return nil, err
	}

	batch.nodeAnn2, err = createNodeAnnouncement(nodeKeyPriv2)
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
	batch.localChanAnn.BitcoinSig1 = nil
	batch.localChanAnn.BitcoinSig2 = nil
	batch.localChanAnn.NodeSig1 = nil
	batch.localChanAnn.NodeSig2 = nil

	batch.chanUpdAnn, err = createUpdateAnnouncement(blockHeight)
	if err != nil {
		return nil, err
	}
	batch.localChanAnn.BitcoinSig1 = nil
	batch.localChanAnn.BitcoinSig2 = nil
	batch.localChanAnn.NodeSig1 = nil
	batch.localChanAnn.NodeSig2 = nil

	return &batch, nil

}

func createNodeAnnouncement(priv *btcec.PrivateKey) (*lnwire.NodeAnnouncement,
	error) {
	var err error

	k := hex.EncodeToString(priv.Serialize())
	alias, err := lnwire.NewNodeAlias("kek" + k[:10])
	if err != nil {
		return nil, err
	}

	a := &lnwire.NodeAnnouncement{
		Timestamp: uint32(prand.Int31()),
		Addresses: testAddrs,
		NodeID:    priv.PubKey(),
		Alias:     alias,
		Features:  testFeatures,
	}

	signer := mockSigner{priv}
	if a.Signature, err = SignAnnouncement(&signer, priv.PubKey(), a); err != nil {
		return nil, err
	}

	return a, nil
}

func createUpdateAnnouncement(blockHeight uint32) (*lnwire.ChannelUpdate, error) {
	var err error

	a := &lnwire.ChannelUpdate{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
		},
		Timestamp:       uint32(prand.Int31()),
		TimeLockDelta:   uint16(prand.Int63()),
		HtlcMinimumMsat: lnwire.MilliSatoshi(prand.Int63()),
		FeeRate:         uint32(prand.Int31()),
		BaseFee:         uint32(prand.Int31()),
	}

	pub := nodeKeyPriv1.PubKey()
	signer := mockSigner{nodeKeyPriv1}
	if a.Signature, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	return a, nil
}

func createRemoteChannelAnnouncement(blockHeight uint32) (*lnwire.ChannelAnnouncement,
	error) {
	var err error

	a := &lnwire.ChannelAnnouncement{
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: blockHeight,
			TxIndex:     0,
			TxPosition:  0,
		},
		NodeID1:     nodeKeyPub1,
		NodeID2:     nodeKeyPub2,
		BitcoinKey1: bitcoinKeyPub1,
		BitcoinKey2: bitcoinKeyPub2,
		Features:    testFeatures,
	}

	pub := nodeKeyPriv1.PubKey()
	signer := mockSigner{nodeKeyPriv1}
	if a.NodeSig1, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = nodeKeyPriv2.PubKey()
	signer = mockSigner{nodeKeyPriv2}
	if a.NodeSig2, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv1.PubKey()
	signer = mockSigner{bitcoinKeyPriv1}
	if a.BitcoinSig1, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	pub = bitcoinKeyPriv2.PubKey()
	signer = mockSigner{bitcoinKeyPriv2}
	if a.BitcoinSig2, err = SignAnnouncement(&signer, pub, a); err != nil {
		return nil, err
	}

	return a, nil
}

type testCtx struct {
	gossiper           *AuthenticatedGossiper
	router             *mockGraphSource
	notifier           *mockNotifier
	broadcastedMessage chan lnwire.Message
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

	broadcastedMessage := make(chan lnwire.Message, 10)
	gossiper, err := New(Config{
		Notifier: notifier,
		Broadcast: func(_ *btcec.PublicKey, msgs ...lnwire.Message) error {
			for _, msg := range msgs {
				broadcastedMessage <- msg
			}
			return nil
		},
		SendToPeer: func(target *btcec.PublicKey, msg ...lnwire.Message) error {
			return nil
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

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	// Create node valid, signed announcement, process it with with
	// gossiper service, check that valid announcement have been
	// propagated farther into the lightning network, and check that we
	// added new node into router.
	na, err := createNodeAnnouncement(nodeKeyPriv1)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(na, na.NodeID)
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.nodes) != 1 {
		t.Fatalf("node wasn't added to router: %v", err)
	}

	// Pretending that we receive the valid channel announcement from
	// remote side, and check that we broadcasted it to the our network,
	// and added channel info in the router.
	ca, err := createRemoteChannelAnnouncement(0)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(ca, na.NodeID)
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge wasn't added to router: %v", err)
	}

	// Pretending that we received valid channel policy update from remote
	// side, and check that we broadcasted it to the other network, and
	// added updates to the router.
	ua, err := createUpdateAnnouncement(0)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(ua, na.NodeID)
	if err != nil {
		t.Fatalf("can't process remote announcement: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't proceeded")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestPrematureAnnouncement checks that premature announcements are
// not propagated to the router subsystem until block with according
// block height received.
func TestPrematureAnnouncement(t *testing.T) {
	t.Parallel()

	ctx, cleanup, err := createTestCtx(0)
	if err != nil {
		t.Fatalf("can't create context: %v", err)
	}
	defer cleanup()

	na, err := createNodeAnnouncement(nodeKeyPriv1)
	if err != nil {
		t.Fatalf("can't create node announcement: %v", err)
	}

	// Pretending that we receive the valid channel announcement from
	// remote side, but block height of this announcement is greater than
	// highest know to us, for that reason it should be added to the
	// repeat/premature batch.
	ca, err := createRemoteChannelAnnouncement(1)
	if err != nil {
		t.Fatalf("can't create channel announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ca, na.NodeID):
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
	ua, err := createUpdateAnnouncement(1)
	if err != nil {
		t.Fatalf("can't create update announcement: %v", err)
	}

	select {
	case <-ctx.gossiper.ProcessRemoteAnnouncement(ua, na.NodeID):
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
		t.Fatal("announcememt wasn't broadcasted")
	}

	if len(ctx.router.infos) != 1 {
		t.Fatalf("edge was't added to router: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
	case <-time.After(2 * trickleDelay):
		t.Fatal("announcememt wasn't broadcasted")
	}

	if len(ctx.router.edges) != 1 {
		t.Fatalf("edge update wasn't added to router: %v", err)
	}
}

// TestSignatureAnnouncementLocalFirst ensures that the AuthenticatedGossiper properly
// processes partial and fully announcement signatures message.
func TestSignatureAnnouncementLocalFirst(t *testing.T) {
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

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

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

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process :%v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn, remoteKey)
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

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn, remoteKey)
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

	batch, err := createAnnouncements(0)
	if err != nil {
		t.Fatalf("can't generate announcements: %v", err)
	}

	localKey := batch.nodeAnn1.NodeID
	remoteKey := batch.nodeAnn2.NodeID

	// Pretending that we receive local channel announcement from funding
	// manager, thereby kick off the announcement exchange process, in
	// this case the announcement should be added in the orphan batch
	// because we haven't announce the channel yet.
	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.remoteProofAnn, remoteKey)
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
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localChanAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}

	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.chanUpdAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process: %v", err)
	}
	select {
	case <-ctx.broadcastedMessage:
		t.Fatal("channel update announcement was broadcast")
	case <-time.After(2 * trickleDelay):
	}

	err = <-ctx.gossiper.ProcessRemoteAnnouncement(batch.chanUpdAnn, remoteKey)
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
	err = <-ctx.gossiper.ProcessLocalAnnouncement(batch.localProofAnn, localKey)
	if err != nil {
		t.Fatalf("unable to process: %v", err)
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
	); err != nil {
		t.Fatalf("unable to retrieve objects from store: %v", err)
	}

	if number != 0 {
		t.Fatal("wrong number of objects in storage")
	}
}
