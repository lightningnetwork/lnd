package peer

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

const (
	broadcastHeight = 100

	// timeout is a timeout value to use for tests which need to wait for
	// a return value on a channel.
	timeout = time.Second * 5
)

var (
	// Just use some arbitrary bytes as delivery script.
	dummyDeliveryScript = channels.AlicesPrivKey
)

// noUpdate is a function which can be used as a parameter in createTestPeer to
// call the setup code with no custom values on the channels set up.
var noUpdate = func(a, b *channeldb.OpenChannel) {}

// createTestPeer creates a channel between two nodes, and returns a peer for
// one of the nodes, together with the channel seen from both nodes. It takes
// an updateChan function which can be used to modify the default values on
// the channel states for each peer.
func createTestPeer(notifier chainntnfs.ChainNotifier,
	publTx chan *wire.MsgTx, updateChan func(a, b *channeldb.OpenChannel)) (
	*Brontide, *lnwallet.LightningChannel, func(), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.AlicesPrivKey,
	)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.BobsPrivKey,
	)

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(1300)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	prevOut := &wire.OutPoint{
		Hash:  channels.TestHdSeed,
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	aliceCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        aliceDustLimit,
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(csvTimeoutAlice),
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
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        bobDustLimit,
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(csvTimeoutBob),
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
		return nil, nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(
		channelBal, channelBal, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, channeldb.SingleFunderTweaklessBit,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	if err != nil {
		return nil, nil, nil, err
	}

	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	if err != nil {
		return nil, nil, nil, err
	}

	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, err
	}

	estimator := chainfee.NewStaticEstimator(12500, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, nil, nil, err
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
		return nil, nil, nil, err
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
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice,
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              channels.TestFundingTx,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob,
		Packager:                channeldb.NewChannelPackager(shortChanID),
	}

	// Set custom values on the channel states.
	updateChan(aliceChannelState, bobChannelState)

	aliceAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	if err := aliceChannelState.SyncPending(aliceAddr, 0); err != nil {
		return nil, nil, nil, err
	}

	bobAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}

	if err := bobChannelState.SyncPending(bobAddr, 0); err != nil {
		return nil, nil, nil, err
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mock.SingleSigner{Privkey: aliceKeyPriv}
	bobSigner := &mock.SingleSigner{Privkey: bobKeyPriv}

	alicePool := lnwallet.NewSigPool(1, aliceSigner)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = alicePool.Start()

	bobPool := lnwallet.NewSigPool(1, bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, bobChannelState, bobPool,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = bobPool.Start()

	chainIO := &mock.ChainIO{
		BestHeight: broadcastHeight,
	}
	wallet := &lnwallet.LightningWallet{
		WalletController: &mock.WalletController{
			RootKey:               aliceKeyPriv,
			PublishedTransactions: publTx,
		},
	}

	const chanActiveTimeout = time.Minute

	errBuffer, err := queue.NewCircularBuffer(ErrorBufferSize)
	if err != nil {
		return nil, nil, nil, err
	}

	var pubKey [33]byte
	copy(pubKey[:], aliceKeyPub.SerializeCompressed())

	cfgAddr := &lnwire.NetAddress{
		IdentityKey: aliceKeyPub,
		Address:     aliceAddr,
		ChainNet:    wire.SimNet,
	}

	cfg := &Config{
		Addr:        cfgAddr,
		PubKeyBytes: pubKey,
		ErrorBuffer: errBuffer,
		ChainIO:     chainIO,
		Switch:      newMockMessageSwitch(),

		ChanActiveTimeout: chanActiveTimeout,
		InterceptSwitch:   htlcswitch.NewInterceptableSwitch(nil),

		ChannelDB:      dbAlice,
		FeeEstimator:   estimator,
		Wallet:         wallet,
		ChainNotifier:  notifier,
		ChanStatusMgr:  newMockStatusMgr(),
		DisconnectPeer: func(b *btcec.PublicKey) error { return nil },
	}

	alicePeer := NewBrontide(*cfg)

	chanID := lnwire.NewChanIDFromOutPoint(channelAlice.ChannelPoint())
	alicePeer.activeChannels[chanID] = channelAlice

	alicePeer.wg.Add(1)
	go alicePeer.channelManager()

	return alicePeer, channelBob, cleanUpFunc, nil
}

type mockMessageLink struct {
	cid     lnwire.ChannelID
	cfg     htlcswitch.ChannelLinkConfig
	channel *lnwallet.LightningChannel
}

// newMockMessageLink makes a mockMessageLink from a ChannelID.
func newMockMessageLink(linkCfg htlcswitch.ChannelLinkConfig,
	lnChan *lnwallet.LightningChannel,
	cid lnwire.ChannelID) *mockMessageLink {

	messageLink := &mockMessageLink{
		cid:     cid,
		cfg:     linkCfg,
		channel: lnChan,
	}

	return messageLink
}

// ChanID returns the mockMessageLink's ChannelID.
func (l *mockMessageLink) ChanID() lnwire.ChannelID {
	return l.cid
}

// HandleChannelUpdate currently does nothing.
func (l *mockMessageLink) HandleChannelUpdate(msg lnwire.Message) {}

// start begins the message-processing part of the mockMessageLink.
func (l *mockMessageLink) start() {
	l.cfg.NotifyActiveLink(l.channel.State().FundingOutpoint)
}

type mockMessageSwitch struct {
	linkIndex map[lnwire.ChannelID]*mockMessageLink
}

// newMockMessageSwitch creates a new *mockMessageSwitch.
func newMockMessageSwitch() *mockMessageSwitch {
	messageSwitch := &mockMessageSwitch{
		linkIndex: make(map[lnwire.ChannelID]*mockMessageLink),
	}

	return messageSwitch
}

// BestHeight returns 0 since it is unused in testing.
func (s *mockMessageSwitch) BestHeight() uint32 {
	return 0
}

// CircuitModifier returns nil since it is unused in testing.
func (s *mockMessageSwitch) CircuitModifier() htlcswitch.CircuitModifier {
	return nil
}

// GetLink retrieves a *mockMessageLink from linkIndex given a ChannelID.
func (s *mockMessageSwitch) GetLink(cid lnwire.ChannelID) (MessageLink,
	error) {

	messageLink := s.linkIndex[cid]
	return messageLink, nil
}

// InitLink starts a *mockMessageLink and adds it to linkIndex.
func (s *mockMessageSwitch) InitLink(linkCfg htlcswitch.ChannelLinkConfig,
	lnChan *lnwallet.LightningChannel) error {

	cid := lnwire.NewChanIDFromOutPoint(&lnChan.State().FundingOutpoint)
	messageLink := newMockMessageLink(linkCfg, lnChan, cid)
	messageLink.start()
	s.linkIndex[cid] = messageLink
	return nil
}

// RemoveLink stops and removes a *mockMessageLink given its ChannelID.
func (s *mockMessageSwitch) RemoveLink(cid lnwire.ChannelID) {
	messageLink := s.linkIndex[cid]
	if messageLink == nil {
		return
	}

	delete(s.linkIndex, cid)
}

type mockChannelGraph struct{}

// newMockChannelGraph returns an instance of *mockChannelGraph.
func newMockChannelGraph() *mockChannelGraph { return &mockChannelGraph{} }

// FetchChannelEdgesByOutpoint currently returns nil for all values.
func (g *mockChannelGraph) FetchChannelEdgesByOutpoint(_ *wire.OutPoint) (
	*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	return nil, nil, nil, nil
}

type mockStatusMgr struct{}

// newMockStatusMgr returns an instance of *mockStatusMgr.
func newMockStatusMgr() *mockStatusMgr { return &mockStatusMgr{} }

// RequestEnable returns nil.
func (s *mockStatusMgr) RequestEnable(_ wire.OutPoint, _ bool) error {
	return nil
}

// RequestDisable returns nil.
func (s *mockStatusMgr) RequestDisable(_ wire.OutPoint, _ bool) error {
	return nil
}

type mockChainArb struct{}

// newMockChainArb returns an instance of *mockChainArb.
func newMockChainArb() *mockChainArb { return &mockChainArb{} }

// SubscribeChannelEvents returns nil values.
func (c *mockChainArb) SubscribeChannelEvents(_ wire.OutPoint) (
	*contractcourt.ChainEventSubscription, error) {

	return nil, nil
}

// UpdateContractSignals returns nil.
func (c *mockChainArb) UpdateContractSignals(_ wire.OutPoint,
	_ *contractcourt.ContractSignals) error {

	return nil
}

// ForceCloseContract currently returns an error.
func (c *mockChainArb) ForceCloseContract(_ wire.OutPoint) (*wire.MsgTx,
	error) {

	return nil, fmt.Errorf("could not force close channel")
}

type mockSphinx struct{}

// newMockSphinx returns an instance of *mockSphinx.
func newMockSphinx() *mockSphinx { return &mockSphinx{} }

// DecodeHopIterators returns nil values.
func (s *mockSphinx) DecodeHopIterators(_ []byte,
	_ []hop.DecodeHopIteratorRequest) ([]hop.DecodeHopIteratorResponse,
	error) {

	return nil, nil
}

// ExtractErrorEncrypter returns nil values.
func (s *mockSphinx) ExtractErrorEncrypter(_ *btcec.PublicKey) (
	hop.ErrorEncrypter, lnwire.FailCode) {

	return nil, 0
}

type mockMessageConn struct {
	t *testing.T

	// MessageConn embeds our interface so that the mock does not need to
	// implement every function. The mock will panic if an unspecified function
	// is called.
	MessageConn

	// writtenMessages is a channel that our mock pushes written messages into.
	writtenMessages chan []byte
}

func newMockConn(t *testing.T, expectedMessages int) *mockMessageConn {
	return &mockMessageConn{
		t:               t,
		writtenMessages: make(chan []byte, expectedMessages),
	}
}

// SetWriteDeadline mocks setting write deadline for our conn.
func (m *mockMessageConn) SetWriteDeadline(time.Time) error {
	return nil
}

// Flush mocks a message conn flush.
func (m *mockMessageConn) Flush() (int, error) {
	return 0, nil
}

// WriteMessage mocks sending of a message on our connection. It will push
// the bytes sent into the mock's writtenMessages channel.
func (m *mockMessageConn) WriteMessage(msg []byte) error {
	select {
	case m.writtenMessages <- msg:
	case <-time.After(timeout):
		m.t.Fatalf("timeout sending message: %v", msg)
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
