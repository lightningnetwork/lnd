package peer

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

const (
	broadcastHeight = 100

	// timeout is a timeout value to use for tests which need to wait for
	// a return value on a channel.
	timeout = time.Second * 5

	// testCltvRejectDelta is the minimum delta between expiry and current
	// height below which htlcs are rejected.
	testCltvRejectDelta = 13
)

var (
	testKeyLoc = keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey}
)

// noUpdate is a function which can be used as a parameter in
// createTestPeerWithChannel to call the setup code with no custom values on
// the channels set up.
var noUpdate = func(a, b *channeldb.OpenChannel) {}

type peerTestCtx struct {
	peer          *Brontide
	channel       *lnwallet.LightningChannel
	notifier      *mock.ChainNotifier
	publishTx     <-chan *wire.MsgTx
	mockSwitch    *mockMessageSwitch
	db            *channeldb.DB
	privKey       *btcec.PrivateKey
	mockConn      *mockMessageConn
	customChan    chan *customMsg
	chanStatusMgr *netann.ChanStatusManager
}

// createTestPeerWithChannel creates a channel between two nodes, and returns a
// peer for one of the nodes, together with the channel seen from both nodes.
// It takes an updateChan function which can be used to modify the default
// values on the channel states for each peer.
func createTestPeerWithChannel(t *testing.T, updateChan func(a,
	b *channeldb.OpenChannel)) (*peerTestCtx, error) {

	params := createTestPeer(t)

	var (
		publishTx     = params.publishTx
		mockSwitch    = params.mockSwitch
		alicePeer     = params.peer
		notifier      = params.notifier
		aliceKeyPriv  = params.privKey
		dbAlice       = params.db
		chanStatusMgr = params.chanStatusMgr
	)

	err := chanStatusMgr.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, chanStatusMgr.Stop())
	})

	aliceKeyPub := alicePeer.IdentityKey()
	estimator := alicePeer.cfg.FeeEstimator

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(1300)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)
	isAliceInitiator := true

	prevOut := &wire.OutPoint{
		Hash:  channels.TestHdSeed,
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(
		channels.BobsPrivKey,
	)

	aliceCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: aliceDustLimit,
			CsvDelay:  uint16(csvTimeoutAlice),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
	}
	bobCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: bobDustLimit,
			CsvDelay:  uint16(csvTimeoutBob),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
	}

	bobRoot, err := chainhash.NewHash(bobKeyPriv.Serialize())
	if err != nil {
		return nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(
		channelBal, channelBal, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, channeldb.SingleFunderTweaklessBit,
		isAliceInitiator, 0,
	)
	if err != nil {
		return nil, err
	}

	dbBob := channeldb.OpenForTesting(t, t.TempDir())

	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): need to factor in commit fee?
	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     feePerKw.FeeForWeight(input.CommitWeight),
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     feePerKw.FeeForWeight(input.CommitWeight),
		CommitTx:      bobCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}

	var chanIDBytes [8]byte
	if _, err := io.ReadFull(crand.Reader, chanIDBytes[:]); err != nil {
		return nil, err
	}

	shortChanID := lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(chanIDBytes[:]),
	)

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             isAliceInitiator,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              channels.TestFundingTx,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             !isAliceInitiator,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
	}

	// Set custom values on the channel states.
	updateChan(aliceChannelState, bobChannelState)

	aliceAddr := alicePeer.cfg.Addr.Address
	if err := aliceChannelState.SyncPending(aliceAddr, 0); err != nil {
		return nil, err
	}

	bobAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}

	if err := bobChannelState.SyncPending(bobAddr, 0); err != nil {
		return nil, err
	}

	aliceSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{aliceKeyPriv}, nil,
	)
	bobSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{bobKeyPriv}, nil,
	)

	alicePool := lnwallet.NewSigPool(1, aliceSigner)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(lnwallet.NewAuxSignerMock(
			lnwallet.EmptyMockJobHandler,
		)),
	)
	if err != nil {
		return nil, err
	}
	_ = alicePool.Start()
	t.Cleanup(func() {
		require.NoError(t, alicePool.Stop())
	})

	bobPool := lnwallet.NewSigPool(1, bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, bobChannelState, bobPool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(lnwallet.NewAuxSignerMock(
			lnwallet.EmptyMockJobHandler,
		)),
	)
	if err != nil {
		return nil, err
	}
	_ = bobPool.Start()
	t.Cleanup(func() {
		require.NoError(t, bobPool.Stop())
	})

	alicePeer.remoteFeatures = lnwire.NewFeatureVector(
		nil, lnwire.Features,
	)

	chanID := lnwire.NewChanIDFromOutPoint(channelAlice.ChannelPoint())
	alicePeer.activeChannels.Store(chanID, channelAlice)

	alicePeer.cg.WgAdd(1)
	go alicePeer.channelManager()

	return &peerTestCtx{
		peer:       alicePeer,
		channel:    channelBob,
		notifier:   notifier,
		publishTx:  publishTx,
		mockSwitch: mockSwitch,
		mockConn:   params.mockConn,
	}, nil
}

// mockMessageSwitch is a mock implementation of the messageSwitch interface
// used for testing without relying on a *htlcswitch.Switch in unit tests.
type mockMessageSwitch struct {
	links []htlcswitch.ChannelUpdateHandler
}

// BestHeight currently returns a dummy value.
func (m *mockMessageSwitch) BestHeight() uint32 {
	return 0
}

// CircuitModifier currently returns a dummy value.
func (m *mockMessageSwitch) CircuitModifier() htlcswitch.CircuitModifier {
	return nil
}

// RemoveLink currently does nothing.
func (m *mockMessageSwitch) RemoveLink(cid lnwire.ChannelID) {}

// CreateAndAddLink currently returns a dummy value.
func (m *mockMessageSwitch) CreateAndAddLink(cfg htlcswitch.ChannelLinkConfig,
	lnChan *lnwallet.LightningChannel) error {

	return nil
}

// GetLinksByInterface returns the active links.
func (m *mockMessageSwitch) GetLinksByInterface(pub [33]byte) (
	[]htlcswitch.ChannelUpdateHandler, error) {

	return m.links, nil
}

// mockUpdateHandler is a mock implementation of the ChannelUpdateHandler
// interface. It is used in mockMessageSwitch's GetLinksByInterface method.
type mockUpdateHandler struct {
	cid                  lnwire.ChannelID
	isOutgoingAddBlocked atomic.Bool
	isIncomingAddBlocked atomic.Bool
}

// newMockUpdateHandler creates a new mockUpdateHandler.
func newMockUpdateHandler(cid lnwire.ChannelID) *mockUpdateHandler {
	return &mockUpdateHandler{
		cid: cid,
	}
}

// HandleChannelUpdate currently does nothing.
func (m *mockUpdateHandler) HandleChannelUpdate(msg lnwire.Message) {}

// ChanID returns the mockUpdateHandler's cid.
func (m *mockUpdateHandler) ChanID() lnwire.ChannelID { return m.cid }

// Bandwidth currently returns a dummy value.
func (m *mockUpdateHandler) Bandwidth() lnwire.MilliSatoshi { return 0 }

// EligibleToForward currently returns a dummy value.
func (m *mockUpdateHandler) EligibleToForward() bool { return false }

// MayAddOutgoingHtlc currently returns nil.
func (m *mockUpdateHandler) MayAddOutgoingHtlc(lnwire.MilliSatoshi) error { return nil }

type mockMessageConn struct {
	t *testing.T

	// MessageConn embeds our interface so that the mock does not need to
	// implement every function. The mock will panic if an unspecified function
	// is called.
	MessageConn

	// writtenMessages is a channel that our mock pushes written messages into.
	writtenMessages chan []byte

	readMessages   chan []byte
	curReadMessage []byte

	// writeRaceDetectingCounter is incremented on any function call
	// associated with writing to the connection. The race detector will
	// trigger on this counter if a data race exists.
	writeRaceDetectingCounter int

	// readRaceDetectingCounter is incremented on any function call
	// associated with reading from the connection. The race detector will
	// trigger on this counter if a data race exists.
	readRaceDetectingCounter int
}

func (m *mockUpdateHandler) EnableAdds(dir htlcswitch.LinkDirection) bool {
	if dir == htlcswitch.Outgoing {
		return m.isOutgoingAddBlocked.Swap(false)
	}

	return m.isIncomingAddBlocked.Swap(false)
}

func (m *mockUpdateHandler) DisableAdds(dir htlcswitch.LinkDirection) bool {
	if dir == htlcswitch.Outgoing {
		return !m.isOutgoingAddBlocked.Swap(true)
	}

	return !m.isIncomingAddBlocked.Swap(true)
}

func (m *mockUpdateHandler) IsFlushing(dir htlcswitch.LinkDirection) bool {
	switch dir {
	case htlcswitch.Outgoing:
		return m.isOutgoingAddBlocked.Load()
	case htlcswitch.Incoming:
		return m.isIncomingAddBlocked.Load()
	}

	return false
}

func (m *mockUpdateHandler) OnFlushedOnce(hook func()) {
	hook()
}
func (m *mockUpdateHandler) OnCommitOnce(
	_ htlcswitch.LinkDirection, hook func(),
) {

	hook()
}
func (m *mockUpdateHandler) InitStfu() <-chan fn.Result[lntypes.ChannelParty] {
	// TODO(proofofkeags): Implement
	c := make(chan fn.Result[lntypes.ChannelParty], 1)

	c <- fn.Errf[lntypes.ChannelParty]("InitStfu not yet implemented")

	return c
}

func newMockConn(t *testing.T, expectedMessages int) *mockMessageConn {
	return &mockMessageConn{
		t:               t,
		writtenMessages: make(chan []byte, expectedMessages),
		readMessages:    make(chan []byte, 1),
	}
}

// SetWriteDeadline mocks setting write deadline for our conn.
func (m *mockMessageConn) SetWriteDeadline(time.Time) error {
	m.writeRaceDetectingCounter++
	return nil
}

// Flush mocks a message conn flush.
func (m *mockMessageConn) Flush() (int, error) {
	m.writeRaceDetectingCounter++
	return 0, nil
}

// WriteMessage mocks sending of a message on our connection. It will push
// the bytes sent into the mock's writtenMessages channel.
func (m *mockMessageConn) WriteMessage(msg []byte) error {
	m.writeRaceDetectingCounter++

	msgCopy := make([]byte, len(msg))
	copy(msgCopy, msg)

	select {
	case m.writtenMessages <- msgCopy:
	case <-time.After(timeout):
		m.t.Fatalf("timeout sending message: %v", msgCopy)
	}

	return nil
}

// assertWrite asserts that our mock as had WriteMessage called with the byte
// slice we expect.
func (m *mockMessageConn) assertWrite(expected []byte) {
	select {
	case actual := <-m.writtenMessages:
		require.Equal(m.t, expected, actual)

	case <-time.After(timeout):
		m.t.Fatalf("timeout waiting for write: %v", expected)
	}
}

func (m *mockMessageConn) SetReadDeadline(t time.Time) error {
	m.readRaceDetectingCounter++
	return nil
}

func (m *mockMessageConn) ReadNextHeader() (uint32, error) {
	m.readRaceDetectingCounter++
	m.curReadMessage = <-m.readMessages
	return uint32(len(m.curReadMessage)), nil
}

func (m *mockMessageConn) ReadNextBody(buf []byte) ([]byte, error) {
	m.readRaceDetectingCounter++
	return m.curReadMessage, nil
}

func (m *mockMessageConn) RemoteAddr() net.Addr {
	return nil
}

func (m *mockMessageConn) LocalAddr() net.Addr {
	return nil
}

func (m *mockMessageConn) Close() error {
	return nil
}

// createTestPeer creates a new peer for testing and returns a context struct
// containing necessary handles and mock objects for conducting tests on peer
// functionalities.
func createTestPeer(t *testing.T) *peerTestCtx {
	nodeKeyLocator := keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
	}

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(
		channels.AlicesPrivKey,
	)

	aliceKeySigner := keychain.NewPrivKeyMessageSigner(
		aliceKeyPriv, nodeKeyLocator,
	)

	aliceAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	cfgAddr := &lnwire.NetAddress{
		IdentityKey: aliceKeyPub,
		Address:     aliceAddr,
		ChainNet:    wire.SimNet,
	}

	errBuffer, err := queue.NewCircularBuffer(ErrorBufferSize)
	require.NoError(t, err)

	chainIO := &mock.ChainIO{
		BestHeight: broadcastHeight,
	}

	publishTx := make(chan *wire.MsgTx)
	wallet := &lnwallet.LightningWallet{
		WalletController: &mock.WalletController{
			RootKey:               aliceKeyPriv,
			PublishedTransactions: publishTx,
		},
	}

	const chanActiveTimeout = time.Minute

	dbAliceGraph := graphdb.MakeTestGraph(t)
	require.NoError(t, dbAliceGraph.Start())
	t.Cleanup(func() {
		require.NoError(t, dbAliceGraph.Stop())
	})

	dbAliceChannel := channeldb.OpenForTesting(t, t.TempDir())

	nodeSignerAlice := netann.NewNodeSigner(aliceKeySigner)

	chanStatusMgr, err := netann.NewChanStatusManager(&netann.
		ChanStatusConfig{
		ChanStatusSampleInterval: 30 * time.Second,
		ChanEnableTimeout:        chanActiveTimeout,
		ChanDisableTimeout:       2 * time.Minute,
		DB:                       dbAliceChannel.ChannelStateDB(),
		Graph:                    dbAliceGraph,
		MessageSigner:            nodeSignerAlice,
		OurPubKey:                aliceKeyPub,
		OurKeyLoc:                testKeyLoc,
		IsChannelActive: func(lnwire.ChannelID) bool {
			return true
		},
		ApplyChannelUpdate: func(*lnwire.ChannelUpdate1,
			*wire.OutPoint, bool) error {

			return nil
		},
	})
	require.NoError(t, err)

	interceptableSwitchNotifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	interceptableSwitchNotifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: 1,
	}

	interceptableSwitch, err := htlcswitch.NewInterceptableSwitch(
		&htlcswitch.InterceptableSwitchConfig{
			CltvRejectDelta:    testCltvRejectDelta,
			CltvInterceptDelta: testCltvRejectDelta + 3,
			Notifier:           interceptableSwitchNotifier,
		},
	)
	require.NoError(t, err)

	// TODO(yy): create interface for lnwallet.LightningChannel so we can
	// easily mock it without the following setups.
	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}

	mockSwitch := &mockMessageSwitch{}

	// TODO(yy): change ChannelNotifier to be an interface.
	channelNotifier := channelnotifier.New(dbAliceChannel.ChannelStateDB())
	require.NoError(t, channelNotifier.Start())
	t.Cleanup(func() {
		require.NoError(t, channelNotifier.Stop(),
			"stop channel notifier failed")
	})

	writeBufferPool := pool.NewWriteBuffer(
		pool.DefaultWriteBufferGCInterval,
		pool.DefaultWriteBufferExpiryInterval,
	)

	writePool := pool.NewWrite(
		writeBufferPool, 1, timeout,
	)
	require.NoError(t, writePool.Start())

	readBufferPool := pool.NewReadBuffer(
		pool.DefaultReadBufferGCInterval,
		pool.DefaultReadBufferExpiryInterval,
	)

	readPool := pool.NewRead(
		readBufferPool, 1, timeout,
	)
	require.NoError(t, readPool.Start())

	mockConn := newMockConn(t, 1)

	receivedCustomChan := make(chan *customMsg)

	var pubKey [33]byte
	copy(pubKey[:], aliceKeyPub.SerializeCompressed())

	estimator := chainfee.NewStaticEstimator(12500, 0)

	cfg := &Config{
		Addr:              cfgAddr,
		PubKeyBytes:       pubKey,
		ErrorBuffer:       errBuffer,
		ChainIO:           chainIO,
		Switch:            mockSwitch,
		ChanActiveTimeout: chanActiveTimeout,
		InterceptSwitch:   interceptableSwitch,
		ChannelDB:         dbAliceChannel.ChannelStateDB(),
		FeeEstimator:      estimator,
		Wallet:            wallet,
		ChainNotifier:     notifier,
		ChanStatusMgr:     chanStatusMgr,
		Features: lnwire.NewFeatureVector(
			nil, lnwire.Features,
		),
		DisconnectPeer: func(b *btcec.PublicKey) error {
			return nil
		},
		ChannelNotifier:               channelNotifier,
		PrunePersistentPeerConnection: func([33]byte) {},
		LegacyFeatures:                lnwire.EmptyFeatureVector(),
		WritePool:                     writePool,
		ReadPool:                      readPool,
		Conn:                          mockConn,
		HandleCustomMessage: func(
			peer [33]byte, msg *lnwire.Custom) error {

			receivedCustomChan <- &customMsg{
				peer: peer,
				msg:  *msg,
			}

			return nil
		},
		PongBuf: make([]byte, lnwire.MaxPongBytes),
		FetchLastChanUpdate: func(chanID lnwire.ShortChannelID,
		) (*lnwire.ChannelUpdate1, error) {

			return &lnwire.ChannelUpdate1{}, nil
		},
	}

	alicePeer := NewBrontide(*cfg)

	return &peerTestCtx{
		publishTx:     publishTx,
		mockSwitch:    mockSwitch,
		peer:          alicePeer,
		notifier:      notifier,
		db:            dbAliceChannel,
		privKey:       aliceKeyPriv,
		mockConn:      mockConn,
		customChan:    receivedCustomChan,
		chanStatusMgr: chanStatusMgr,
	}
}

// startPeer invokes the `Start` method on the specified peer and handles any
// initial startup messages for testing.
func startPeer(t *testing.T, mockConn *mockMessageConn,
	peer *Brontide) <-chan struct{} {

	// Start the peer in a goroutine so that we can handle and test for
	// startup messages. Successfully sending and receiving init message,
	// indicates a successful startup.
	done := make(chan struct{})
	go func() {
		require.NoError(t, peer.Start())
		close(done)
	}()

	// Receive the init message that should be the first message received on
	// startup.
	rawMsg, err := fn.RecvOrTimeout[[]byte](
		mockConn.writtenMessages, timeout,
	)
	require.NoError(t, err)

	msgReader := bytes.NewReader(rawMsg)
	nextMsg, err := lnwire.ReadMessage(msgReader, 0)
	require.NoError(t, err)

	_, ok := nextMsg.(*lnwire.Init)
	require.True(t, ok)

	// Write the reply for the init message to complete the startup.
	initReplyMsg := lnwire.NewInitMessage(
		lnwire.NewRawFeatureVector(
			lnwire.DataLossProtectRequired,
			lnwire.GossipQueriesOptional,
		),
		lnwire.NewRawFeatureVector(),
	)

	var b bytes.Buffer
	_, err = lnwire.WriteMessage(&b, initReplyMsg, 0)
	require.NoError(t, err)

	ok = fn.SendOrQuit[[]byte, struct{}](
		mockConn.readMessages, b.Bytes(), make(chan struct{}),
	)
	require.True(t, ok)

	return done
}
