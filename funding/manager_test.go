package funding

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainreg"
	acpt "github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	// testPollNumTries is the number of times we attempt to query
	// for a certain expected database state before we give up and
	// consider the test failed. Since it sometimes can take a
	// while to update the database, we poll a certain amount of
	// times, until it gets into the state we expect, or we are out
	// of tries.
	testPollNumTries = 10

	// testPollSleepMs is the number of milliseconds to sleep between
	// each attempt to access the database to check its state.
	testPollSleepMs = 500

	// maxPending is the maximum number of channels we allow opening to the
	// same peer in the max pending channels test.
	maxPending = 4

	// A dummy value to use for the funding broadcast height.
	fundingBroadcastHeight = 123

	// defaultMaxLocalCSVDelay is the maximum delay we accept on our
	// commitment output.
	defaultMaxLocalCSVDelay = 2016
)

var (
	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(
		alicePrivKeyBytes[:],
	)

	aliceTCPAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.2:9001")

	aliceAddr = &lnwire.NetAddress{
		IdentityKey: alicePubKey,
		Address:     aliceTCPAddr,
	}

	bobPrivKeyBytes = [32]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	bobPrivKey, bobPubKey = btcec.PrivKeyFromBytes(bobPrivKeyBytes[:])

	bobTCPAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.2:9000")

	bobAddr = &lnwire.NetAddress{
		IdentityKey: bobPubKey,
		Address:     bobTCPAddr,
	}

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar   = new(btcec.ModNScalar)
	testSScalar   = new(btcec.ModNScalar)
	_             = testRScalar.SetByteSlice(testRBytes)
	_             = testSScalar.SetByteSlice(testSBytes)
	testSig       = ecdsa.NewSignature(testRScalar, testSScalar)

	testKeyLoc = keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey}

	fundingNetParams = chainreg.BitcoinTestNetParams

	alias = lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     0,
		TxPosition:  0,
	}
)

type mockChanFunder struct {
	fundingAmt btcutil.Amount
	localKey   *keychain.KeyDescriptor
	remoteKey  *btcec.PublicKey
	chanPoint  *wire.OutPoint
}

func (m *mockChanFunder) ProvisionChannel(r *chanfunding.Request) (
	chanfunding.Intent, error) {

	shimIntent := chanfunding.NewShimIntent(
		m.fundingAmt, 0, m.localKey, m.remoteKey, m.chanPoint, 0,
		false,
	)

	return shimIntent, nil
}

type mockAliasMgr struct{}

func (m *mockAliasMgr) RequestAlias() (lnwire.ShortChannelID, error) {
	return alias, nil
}

func (m *mockAliasMgr) PutPeerAlias(lnwire.ChannelID,
	lnwire.ShortChannelID) error {

	return nil
}

func (m *mockAliasMgr) GetPeerAlias(lnwire.ChannelID) (lnwire.ShortChannelID,
	error) {

	return alias, nil
}

func (m *mockAliasMgr) AddLocalAlias(lnwire.ShortChannelID,
	lnwire.ShortChannelID, bool, bool,
	...aliasmgr.AddLocalAliasOption) error {

	return nil
}

func (m *mockAliasMgr) GetAliases(
	lnwire.ShortChannelID) []lnwire.ShortChannelID {

	return []lnwire.ShortChannelID{alias}
}

func (m *mockAliasMgr) DeleteSixConfs(lnwire.ShortChannelID) error {
	return nil
}

type mockNotifier struct {
	oneConfChannel   chan *chainntnfs.TxConfirmation
	sixConfChannel   chan *chainntnfs.TxConfirmation
	epochChan        chan *chainntnfs.BlockEpoch
	oneUpdateChannel chan chainntnfs.TxUpdateInfo
	reOrgChan        chan int32
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	if numConfs == 6 {
		return &chainntnfs.ConfirmationEvent{
			Confirmed:    m.sixConfChannel,
			NegativeConf: m.reOrgChan,
		}, nil
	}
	return &chainntnfs.ConfirmationEvent{
		Confirmed:    m.oneConfChannel,
		Updates:      m.oneUpdateChannel,
		NegativeConf: m.reOrgChan,
	}, nil
}

func (m *mockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: m.epochChan,
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

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

type mockChanEvent struct {
	openEvent        chan wire.OutPoint
	pendingOpenEvent chan channelnotifier.PendingOpenChannelEvent
}

func (m *mockChanEvent) NotifyOpenChannelEvent(outpoint wire.OutPoint,
	remotePub *btcec.PublicKey) {

	m.openEvent <- outpoint
}

func (m *mockChanEvent) NotifyPendingOpenChannelEvent(outpoint wire.OutPoint,
	pendingChannel *channeldb.OpenChannel,
	remotePub *btcec.PublicKey) {

	m.pendingOpenEvent <- channelnotifier.PendingOpenChannelEvent{
		ChannelPoint:   &outpoint,
		PendingChannel: pendingChannel,
	}
}

func (m *mockChanEvent) NotifyFundingTimeout(outpoint wire.OutPoint,
	remotePub *btcec.PublicKey) {
}

// mockZeroConfAcceptor always accepts the channel open request for zero-conf
// channels. It will set the ZeroConf bool in the ChannelAcceptResponse. This
// is needed to properly unit test the zero-conf logic in the funding manager.
type mockZeroConfAcceptor struct{}

func (m *mockZeroConfAcceptor) Accept(
	req *acpt.ChannelAcceptRequest) *acpt.ChannelAcceptResponse {

	return &acpt.ChannelAcceptResponse{
		ZeroConf: true,
	}
}

type newChannelMsg struct {
	channel *lnpeer.NewChannel
	err     chan error
}

type testNode struct {
	privKey         *btcec.PrivateKey
	addr            *lnwire.NetAddress
	msgChan         chan lnwire.Message
	announceChan    chan lnwire.Message
	publTxChan      chan *wire.MsgTx
	fundingMgr      *Manager
	newChannels     chan *newChannelMsg
	mockNotifier    *mockNotifier
	mockChanEvent   *mockChanEvent
	testDir         string
	shutdownChannel chan struct{}
	reportScidChan  chan struct{}
	localFeatures   []lnwire.FeatureBit
	remoteFeatures  []lnwire.FeatureBit

	remotePeer  *testNode
	sendMessage func(lnwire.Message) error
}

var _ lnpeer.Peer = (*testNode)(nil)

func (n *testNode) Disconnect(err error) {}

func (n *testNode) IdentityKey() *btcec.PublicKey {
	return n.addr.IdentityKey
}

func (n *testNode) Address() net.Addr {
	return n.addr.Address
}

func (n *testNode) PubKey() [33]byte {
	return newSerializedKey(n.addr.IdentityKey)
}

func (n *testNode) SendMessage(_ bool, msg ...lnwire.Message) error {
	return n.sendMessage(msg[0])
}

func (n *testNode) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	return n.SendMessage(sync, msgs...)
}

func (n *testNode) WipeChannel(_ *wire.OutPoint) {}

func (n *testNode) QuitSignal() <-chan struct{} {
	return n.shutdownChannel
}

func (n *testNode) LocalFeatures() *lnwire.FeatureVector {
	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(n.localFeatures...), nil,
	)
}

func (n *testNode) RemoteFeatures() *lnwire.FeatureVector {
	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(n.remoteFeatures...), nil,
	)
}

func (n *testNode) AddNewChannel(channel *lnpeer.NewChannel,
	quit <-chan struct{}) error {

	errChan := make(chan error)
	msg := &newChannelMsg{
		channel: channel,
		err:     errChan,
	}

	select {
	case n.newChannels <- msg:
	case <-quit:
		return ErrFundingManagerShuttingDown
	}

	select {
	case err := <-errChan:
		return err
	case <-quit:
		return ErrFundingManagerShuttingDown
	}
}

func (n *testNode) AddPendingChannel(_ lnwire.ChannelID,
	quit <-chan struct{}) error {

	return nil
}

func (n *testNode) RemovePendingChannel(_ lnwire.ChannelID) error {
	return nil
}

func createTestWallet(cdb *channeldb.ChannelStateDB, netParams *chaincfg.Params,
	notifier chainntnfs.ChainNotifier, wc lnwallet.WalletController,
	signer input.Signer, keyRing keychain.SecretKeyRing,
	bio lnwallet.BlockChainIO,
	estimator chainfee.Estimator) (*lnwallet.LightningWallet, error) {

	wallet, err := lnwallet.NewLightningWallet(lnwallet.Config{
		Database:              cdb,
		Notifier:              notifier,
		SecretKeyRing:         keyRing,
		WalletController:      wc,
		Signer:                signer,
		ChainIO:               bio,
		FeeEstimator:          estimator,
		NetParams:             *netParams,
		CoinSelectionStrategy: wallet.CoinSelectionLargest,
	})
	if err != nil {
		return nil, err
	}

	if err := wallet.Startup(); err != nil {
		return nil, err
	}

	return wallet, nil
}

func createTestFundingManager(t *testing.T, privKey *btcec.PrivateKey,
	addr *lnwire.NetAddress, tempTestDir string,
	options ...cfgOption) (*testNode, error) {

	netParams := fundingNetParams.Params
	estimator := chainfee.NewStaticEstimator(62500, 0)

	chainNotifier := &mockNotifier{
		oneConfChannel:   make(chan *chainntnfs.TxConfirmation, 1),
		sixConfChannel:   make(chan *chainntnfs.TxConfirmation, 1),
		epochChan:        make(chan *chainntnfs.BlockEpoch, 2),
		oneUpdateChannel: make(chan chainntnfs.TxUpdateInfo, 1),
		reOrgChan:        make(chan int32, 1),
	}

	aliasMgr := &mockAliasMgr{}

	sentMessages := make(chan lnwire.Message)
	sentAnnouncements := make(chan lnwire.Message)
	publTxChan := make(chan *wire.MsgTx, 1)
	shutdownChan := make(chan struct{})
	reportScidChan := make(chan struct{})

	wc := &mock.WalletController{
		RootKey: alicePrivKey,
	}
	signer := mock.NewSingleSigner(alicePrivKey)
	bio := &mock.ChainIO{
		BestHeight: fundingBroadcastHeight,
	}

	// The mock channel event notifier will receive events for each pending
	// open and open channel. Because some tests will create multiple
	// channels in a row before advancing to the next step, these channels
	// need to be buffered.
	evt := &mockChanEvent{
		openEvent: make(chan wire.OutPoint, maxPending),
		pendingOpenEvent: make(
			chan channelnotifier.PendingOpenChannelEvent,
			maxPending,
		),
	}

	dbDir := filepath.Join(tempTestDir, "cdb")
	fullDB := channeldb.OpenForTesting(t, dbDir)

	cdb := fullDB.ChannelStateDB()

	keyRing := &mock.SecretKeyRing{
		RootKey: alicePrivKey,
	}

	lnw, err := createTestWallet(
		cdb, netParams, chainNotifier, wc, signer, keyRing, bio,
		estimator,
	)
	require.NoError(t, err, "unable to create test ln wallet")

	var chanIDSeed [32]byte

	chainedAcceptor := acpt.NewChainedAcceptor()

	fundingCfg := Config{
		IDKey:        privKey.PubKey(),
		IDKeyLoc:     testKeyLoc,
		Wallet:       lnw,
		Notifier:     chainNotifier,
		ChannelDB:    cdb,
		FeeEstimator: estimator,
		SignMessage: func(_ keychain.KeyLocator,
			_ []byte, _ bool) (*ecdsa.Signature, error) {

			return testSig, nil
		},
		SendAnnouncement: func(msg lnwire.Message,
			_ ...discovery.OptionalMsgField) chan error {

			errChan := make(chan error, 1)
			select {
			case sentAnnouncements <- msg:
				errChan <- nil
			case <-shutdownChan:
				errChan <- fmt.Errorf("shutting down")
			}
			return errChan
		},
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement1,
			error) {

			return lnwire.NodeAnnouncement1{}, nil
		},
		TempChanIDSeed: chanIDSeed,
		FindChannel: func(node *btcec.PublicKey,
			chanID lnwire.ChannelID) (*channeldb.OpenChannel,
			error) {

			nodeChans, err := cdb.FetchOpenChannels(node)
			if err != nil {
				return nil, err
			}

			for _, channel := range nodeChans {
				if chanID.IsChanPoint(
					&channel.FundingOutpoint,
				) {

					return channel, nil
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		},
		DefaultRoutingPolicy: models.ForwardingPolicy{
			MinHTLCOut:    5,
			BaseFee:       100,
			FeeRate:       1000,
			TimeLockDelta: 10,
		},
		DefaultMinHtlcIn: 5,
		NumRequiredConfs: func(chanAmt btcutil.Amount,
			pushAmt lnwire.MilliSatoshi) uint16 {
			return 3
		},
		RequiredRemoteDelay: func(amt btcutil.Amount) uint16 {
			return 4
		},
		RequiredRemoteChanReserve: func(chanAmt,
			dustLimit btcutil.Amount) btcutil.Amount {

			reserve := chanAmt / 100
			if reserve < dustLimit {
				reserve = dustLimit
			}

			return reserve
		},
		RequiredRemoteMaxValue: func(chanAmt btcutil.Amount) lnwire.MilliSatoshi {
			reserve := lnwire.NewMSatFromSatoshis(chanAmt / 100)
			return lnwire.NewMSatFromSatoshis(chanAmt) - reserve
		},
		RequiredRemoteMaxHTLCs: func(chanAmt btcutil.Amount) uint16 {
			return uint16(input.MaxHTLCNumber / 2)
		},
		WatchNewChannel: func(*channeldb.OpenChannel,
			*btcec.PublicKey) error {

			return nil
		},
		ReportShortChanID: func(wire.OutPoint) error {
			reportScidChan <- struct{}{}
			return nil
		},
		PublishTransaction: func(txn *wire.MsgTx, _ string) error {
			publTxChan <- txn
			return nil
		},
		UpdateLabel: func(chainhash.Hash, string) error {
			return nil
		},
		ZombieSweeperInterval:         1 * time.Hour,
		ReservationTimeout:            1 * time.Nanosecond,
		MaxChanSize:                   MaxBtcFundingAmount,
		MaxLocalCSVDelay:              defaultMaxLocalCSVDelay,
		MaxPendingChannels:            lncfg.DefaultMaxPendingChannels,
		NotifyOpenChannelEvent:        evt.NotifyOpenChannelEvent,
		OpenChannelPredicate:          chainedAcceptor,
		NotifyPendingOpenChannelEvent: evt.NotifyPendingOpenChannelEvent,
		NotifyFundingTimeout:          evt.NotifyFundingTimeout,
		DeleteAliasEdge: func(scid lnwire.ShortChannelID) (
			*models.ChannelEdgePolicy, error) {

			return nil, nil
		},
		AliasManager: aliasMgr,
		// For unit tests we default to false meaning that no funds
		// originated from the sweeper.
		IsSweeperOutpoint: func(wire.OutPoint) bool {
			return false
		},
		AuxLeafStore: fn.Some[lnwallet.AuxLeafStore](
			&lnwallet.MockAuxLeafStore{},
		),
		AuxSigner: fn.Some[lnwallet.AuxSigner](
			lnwallet.NewAuxSignerMock(lnwallet.EmptyMockJobHandler),
		),
	}

	for _, op := range options {
		op(&fundingCfg)
	}

	f, err := NewFundingManager(fundingCfg)
	require.NoError(t, err, "failed creating fundingManager")
	if err = f.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}

	testNode := &testNode{
		privKey:         privKey,
		msgChan:         sentMessages,
		newChannels:     make(chan *newChannelMsg),
		announceChan:    sentAnnouncements,
		publTxChan:      publTxChan,
		fundingMgr:      f,
		mockNotifier:    chainNotifier,
		mockChanEvent:   evt,
		testDir:         tempTestDir,
		shutdownChannel: shutdownChan,
		reportScidChan:  reportScidChan,
		addr:            addr,
	}

	f.cfg.NotifyWhenOnline = func(peer [33]byte,
		connectedChan chan<- lnpeer.Peer) {

		connectedChan <- testNode.remotePeer
	}

	return testNode, nil
}

func recreateAliceFundingManager(t *testing.T, alice *testNode) {
	// Stop the old fundingManager before creating a new one.
	close(alice.shutdownChannel)
	if err := alice.fundingMgr.Stop(); err != nil {
		t.Fatalf("failed stop funding manager: %v", err)
	}

	aliceMsgChan := make(chan lnwire.Message)
	aliceAnnounceChan := make(chan lnwire.Message)
	shutdownChan := make(chan struct{})
	publishChan := make(chan *wire.MsgTx, 10)

	oldCfg := alice.fundingMgr.cfg

	chainedAcceptor := acpt.NewChainedAcceptor()

	f, err := NewFundingManager(Config{
		IDKey:        oldCfg.IDKey,
		IDKeyLoc:     oldCfg.IDKeyLoc,
		Wallet:       oldCfg.Wallet,
		Notifier:     oldCfg.Notifier,
		ChannelDB:    oldCfg.ChannelDB,
		FeeEstimator: oldCfg.FeeEstimator,
		SignMessage: func(_ keychain.KeyLocator,
			_ []byte, _ bool) (*ecdsa.Signature, error) {

			return testSig, nil
		},
		SendAnnouncement: func(msg lnwire.Message,
			_ ...discovery.OptionalMsgField) chan error {

			errChan := make(chan error, 1)
			select {
			case aliceAnnounceChan <- msg:
				errChan <- nil
			case <-shutdownChan:
				errChan <- fmt.Errorf("shutting down")
			}
			return errChan
		},
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement1,
			error) {

			return lnwire.NodeAnnouncement1{}, nil
		},
		NotifyWhenOnline: func(peer [33]byte,
			connectedChan chan<- lnpeer.Peer) {

			connectedChan <- alice.remotePeer
		},
		TempChanIDSeed: oldCfg.TempChanIDSeed,
		FindChannel:    oldCfg.FindChannel,
		DefaultRoutingPolicy: models.ForwardingPolicy{
			MinHTLCOut:    5,
			BaseFee:       100,
			FeeRate:       1000,
			TimeLockDelta: 10,
		},
		DefaultMinHtlcIn:       5,
		RequiredRemoteMaxValue: oldCfg.RequiredRemoteMaxValue,
		ReportShortChanID:      oldCfg.ReportShortChanID,
		PublishTransaction: func(txn *wire.MsgTx, _ string) error {
			publishChan <- txn
			return nil
		},
		UpdateLabel: func(chainhash.Hash, string) error {
			return nil
		},
		ZombieSweeperInterval: oldCfg.ZombieSweeperInterval,
		ReservationTimeout:    oldCfg.ReservationTimeout,
		OpenChannelPredicate:  chainedAcceptor,
		DeleteAliasEdge:       oldCfg.DeleteAliasEdge,
		AliasManager:          oldCfg.AliasManager,
		AuxLeafStore:          oldCfg.AuxLeafStore,
		AuxSigner:             oldCfg.AuxSigner,
	})
	require.NoError(t, err, "failed recreating aliceFundingManager")

	alice.fundingMgr = f
	alice.msgChan = aliceMsgChan
	alice.announceChan = aliceAnnounceChan
	alice.publTxChan = publishChan
	alice.shutdownChannel = shutdownChan

	if err = f.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}
}

type cfgOption func(*Config)

func setupFundingManagers(t *testing.T,
	options ...cfgOption) (*testNode, *testNode) {

	alice, err := createTestFundingManager(
		t, alicePrivKey, aliceAddr, t.TempDir(), options...,
	)
	require.NoError(t, err, "failed creating fundingManager")

	bob, err := createTestFundingManager(
		t, bobPrivKey, bobAddr, t.TempDir(), options...,
	)
	require.NoError(t, err, "failed creating fundingManager")

	// With the funding manager's created, we'll now attempt to mimic a
	// connection pipe between them. In order to intercept the messages
	// within it, we'll redirect all messages back to the msgChan of the
	// sender. Since the fundingManager now has a reference to peers itself,
	// alice.sendMessage will be triggered when Bob's funding manager
	// attempts to send a message to Alice and vice versa.
	alice.remotePeer = bob
	alice.sendMessage = func(msg lnwire.Message) error {
		select {
		case alice.remotePeer.msgChan <- msg:
		case <-alice.shutdownChannel:
			return errors.New("shutting down")
		}
		return nil
	}

	bob.remotePeer = alice
	bob.sendMessage = func(msg lnwire.Message) error {
		select {
		case bob.remotePeer.msgChan <- msg:
		case <-bob.shutdownChannel:
			return errors.New("shutting down")
		}
		return nil
	}

	return alice, bob
}

func tearDownFundingManagers(t *testing.T, a, b *testNode) {
	close(a.shutdownChannel)
	close(b.shutdownChannel)

	if err := a.fundingMgr.Stop(); err != nil {
		t.Fatalf("failed stop funding manager: %v", err)
	}
	if err := b.fundingMgr.Stop(); err != nil {
		t.Fatalf("failed stop funding manager: %v", err)
	}
}

// openChannel takes the funding process to the point where the funding
// transaction is confirmed on-chain. Returns the funding out point.
func openChannel(t *testing.T, alice, bob *testNode, localFundingAmt,
	pushAmt btcutil.Amount, numConfs uint32,
	updateChan chan *lnrpc.OpenStatusUpdate, announceChan bool,
	chanType *lnwire.ChannelType) (
	*wire.OutPoint, *wire.MsgTx) {

	publ := fundChannel(
		t, alice, bob, localFundingAmt, pushAmt, false, 0, 0, numConfs,
		updateChan, announceChan, chanType,
	)
	fundingOutPoint := &wire.OutPoint{
		Hash:  publ.TxHash(),
		Index: 0,
	}
	return fundingOutPoint, publ
}

func isTaprootChanType(chanType *lnwire.ChannelType) bool {
	if chanType == nil {
		return false
	}

	featVec := lnwire.RawFeatureVector(*chanType)

	return featVec.IsSet(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	)
}

// fundChannel takes the funding process to the point where the funding
// transaction is confirmed on-chain. Returns the funding tx.
func fundChannel(t *testing.T, alice, bob *testNode, localFundingAmt,
	pushAmt btcutil.Amount, subtractFees bool, fundUpToMaxAmt,
	minFundAmt btcutil.Amount, numConfs uint32,
	updateChan chan *lnrpc.OpenStatusUpdate, announceChan bool,
	chanType *lnwire.ChannelType) *wire.MsgTx {

	// Create a funding request and start the workflow.
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		SubtractFees:    subtractFees,
		FundUpToMaxAmt:  fundUpToMaxAmt,
		MinConfs:        int32(numConfs),
		MinFundAmt:      minFundAmt,
		LocalFundingAmt: localFundingAmt,
		PushAmt:         lnwire.NewMSatFromSatoshis(pushAmt),
		FundingFeePerKw: 1000,
		Private:         !announceChan,
		ChannelType:     chanType,
		Updates:         updateChan,
		Err:             errChan,
	}

	// If this is a taproot channel, then we want to force it to be a
	// private channel, as that's the only channel type supported for now.
	if isTaprootChanType(chanType) {
		initReq.Private = true
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// For taproot channel types, the nonce should be set.
	if isTaprootChanType(chanType) {
		require.NotNil(t, openChannelReq.LocalNonce)
	}

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel message.
	acceptChannelResponse := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// For taproot channel types, the nonce should be set.
	if isTaprootChanType(chanType) {
		require.NotNil(t, acceptChannelResponse.LocalNonce)
	}

	// They now should both have pending reservations for this channel
	// active.
	assertNumPendingReservations(t, alice, bobPubKey, 1)
	assertNumPendingReservations(t, bob, alicePubKey, 1)

	// Forward the response to Alice.
	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// Check that sending warning messages does not abort the funding
	// process.
	warningMsg := &lnwire.Warning{
		Data: []byte("random warning"),
	}
	alice.fundingMgr.ProcessFundingMsg(warningMsg, bob)
	bob.fundingMgr.ProcessFundingMsg(warningMsg, alice)

	// Alice responds with a FundingCreated message.
	fundingCreated := assertFundingMsgSent(
		t, alice.msgChan, "FundingCreated",
	).(*lnwire.FundingCreated)

	// For taproot channel types, the partial signature should be set.
	if isTaprootChanType(chanType) {
		require.NotNil(t, fundingCreated.PartialSig)
	}

	// Give the message to Bob.
	bob.fundingMgr.ProcessFundingMsg(fundingCreated, alice)

	// Finally, Bob should send the FundingSigned message.
	fundingSigned := assertFundingMsgSent(
		t, bob.msgChan, "FundingSigned",
	).(*lnwire.FundingSigned)

	// For taproot channel types, the partial signature should be set.
	if isTaprootChanType(chanType) {
		require.NotNil(t, fundingSigned.PartialSig)
	}

	// Forward the signature to Alice.
	alice.fundingMgr.ProcessFundingMsg(fundingSigned, bob)

	// After Alice processes the singleFundingSignComplete message, she will
	// broadcast the funding transaction to the network. We expect to get a
	// channel update saying the channel is pending.
	var pendingUpdate *lnrpc.OpenStatusUpdate
	select {
	case pendingUpdate = <-updateChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenStatusUpdate_ChanPending")
	}

	_, ok = pendingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	if !ok {
		t.Fatal("OpenStatusUpdate was not OpenStatusUpdate_ChanPending")
	}

	// Get and return the transaction Alice published to the network.
	var publ *wire.MsgTx
	select {
	case publ = <-alice.publTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not publish funding tx")
	}

	// Make sure the notification about the pending channel was sent out.
	select {
	case <-alice.mockChanEvent.pendingOpenEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send pending channel event")
	}
	select {
	case <-bob.mockChanEvent.pendingOpenEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send pending channel event")
	}

	// Finally, make sure neither have active reservation for the channel
	// now pending open in the database.
	assertNumPendingReservations(t, alice, bobPubKey, 0)
	assertNumPendingReservations(t, bob, alicePubKey, 0)

	return publ
}

func assertErrorNotSent(t *testing.T, msgChan chan lnwire.Message) {
	t.Helper()

	select {
	case <-msgChan:
		t.Fatalf("error sent unexpectedly")
	case <-time.After(100 * time.Millisecond):
		// Expected, return.
	}
}

func assertErrorSent(t *testing.T, msgChan chan lnwire.Message) {
	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("node did not send Error message")
	}
	_, ok := msg.(*lnwire.Error)
	if !ok {
		t.Fatalf("expected Error to be sent from "+
			"node, instead got %T", msg)
	}
}

func assertFundingMsgSent(t *testing.T, msgChan chan lnwire.Message,
	msgType string) lnwire.Message {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("peer did not send %s message", msgType)
	}

	var (
		sentMsg lnwire.Message
		ok      bool
	)
	switch msgType {
	case "AcceptChannel":
		sentMsg, ok = msg.(*lnwire.AcceptChannel)
	case "FundingCreated":
		sentMsg, ok = msg.(*lnwire.FundingCreated)
	case "FundingSigned":
		sentMsg, ok = msg.(*lnwire.FundingSigned)
	case "ChannelReady":
		sentMsg, ok = msg.(*lnwire.ChannelReady)
	case "Error":
		sentMsg, ok = msg.(*lnwire.Error)
	default:
		t.Fatalf("unknown message type: %s", msgType)
	}

	if !ok {
		errorMsg, gotError := msg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected %s to be sent, instead got "+
				"error: %v", msgType, errorMsg.Error())
		}

		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("expected %s to be sent, instead got %T at %v",
			msgType, msg, line)
	}

	return sentMsg
}

func assertNumPendingReservations(t *testing.T, node *testNode,
	peerPubKey *btcec.PublicKey, expectedNum int) {

	t.Helper()

	serializedPubKey := newSerializedKey(peerPubKey)
	actualNum := len(node.fundingMgr.activeReservations[serializedPubKey])
	if actualNum == expectedNum {
		// Success, return.
		return
	}

	t.Fatalf("Expected node to have %d pending reservations, had %v",
		expectedNum, actualNum)
}

func assertNumPendingChannelsBecomes(t *testing.T, node *testNode,
	expectedNum int) {

	t.Helper()

	var numPendingChans int
	for i := 0; i < testPollNumTries; i++ {
		// If this is not the first try, sleep before retrying.
		if i > 0 {
			time.Sleep(testPollSleepMs * time.Millisecond)
		}
		pendingChannels, err := node.fundingMgr.
			cfg.Wallet.Cfg.Database.FetchPendingChannels()
		if err != nil {
			t.Fatalf("unable to fetch pending channels: %v", err)
		}

		numPendingChans = len(pendingChannels)
		if numPendingChans == expectedNum {
			// Success, return.
			return
		}
	}

	t.Fatalf("Expected node to have %d pending channels, had %v",
		expectedNum, numPendingChans)
}

func assertNumPendingChannelsRemains(t *testing.T, node *testNode,
	expectedNum int) {

	t.Helper()

	var numPendingChans int
	for i := 0; i < 5; i++ {
		// If this is not the first try, sleep before retrying.
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		pendingChannels, err := node.fundingMgr.
			cfg.Wallet.Cfg.Database.FetchPendingChannels()
		if err != nil {
			t.Fatalf("unable to fetch pending channels: %v", err)
		}

		numPendingChans = len(pendingChannels)
		if numPendingChans != expectedNum {

			t.Fatalf("Expected node to have %d pending channels, had %v",
				expectedNum, numPendingChans)
		}
	}
}

// assertConfirmationHeight checks that the channel with the given chanID has
// the expected confirmation height in the database. It will retry for a few
// times in case the confirmation height is not yet set in the database.
func assertConfirmationHeight(t *testing.T, node *testNode,
	chanID lnwire.ChannelID, expectedConfHeight uint32) {

	t.Helper()

	err := wait.NoError(func() error {
		pendingChannel, err := node.fundingMgr.cfg.Wallet.Cfg.Database.
			FetchChannelByID(nil, chanID)
		if err != nil {
			return fmt.Errorf("unable to fetch pending channel: %w",
				err)
		}

		// Check if the confirmation height is as expected.
		actualConfHeight := pendingChannel.ConfirmationHeight
		if actualConfHeight != expectedConfHeight {
			return fmt.Errorf("Expected node to have %d "+
				"confirmation height, had %v",
				expectedConfHeight, actualConfHeight)
		}

		// Success, return.
		return nil
	}, wait.DefaultTimeout)

	require.NoError(t, err)
}

func assertDatabaseState(t *testing.T, node *testNode,
	fundingOutPoint *wire.OutPoint, expectedState channelOpeningState) {

	t.Helper()

	var state channelOpeningState
	var err error
	for i := 0; i < testPollNumTries; i++ {
		// If this is not the first try, sleep before retrying.
		if i > 0 {
			time.Sleep(testPollSleepMs * time.Millisecond)
		}
		state, _, err = node.fundingMgr.getChannelOpeningState(
			fundingOutPoint)
		if err != nil && err != channeldb.ErrChannelNotFound {
			t.Fatalf("unable to get channel state: %v", err)
		}

		// If we found the channel, check if it had the expected state.
		if !errors.Is(err, channeldb.ErrChannelNotFound) &&
			state == expectedState {

			// Got expected state, return with success.
			return
		}
	}

	// 10 tries without success.
	if err != nil {
		t.Fatalf("error getting channelOpeningState: %v", err)
	} else {
		t.Fatalf("expected state to be %v, was %v", expectedState,
			state)
	}
}

func assertMarkedOpen(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {

	t.Helper()

	// Make sure the notification about the pending channel was sent out.
	select {
	case <-alice.mockChanEvent.openEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send open channel event")
	}
	select {
	case <-bob.mockChanEvent.openEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send open channel event")
	}

	assertDatabaseState(t, alice, fundingOutPoint, markedOpen)
	assertDatabaseState(t, bob, fundingOutPoint, markedOpen)
}

func assertChannelReadySent(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {

	t.Helper()

	assertDatabaseState(t, alice, fundingOutPoint, channelReadySent)
	assertDatabaseState(t, bob, fundingOutPoint, channelReadySent)
}

func assertAddedToGraph(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {

	t.Helper()

	assertDatabaseState(t, alice, fundingOutPoint, addedToGraph)
	assertDatabaseState(t, bob, fundingOutPoint, addedToGraph)
}

// assertChannelAnnouncements checks that alice and bob both sends the expected
// announcements (ChannelAnnouncement, ChannelUpdate) after the funding tx has
// confirmed. The last arguments can be set if we expect the nodes to advertise
// custom min_htlc values as part of their ChannelUpdate. We expect Alice to
// advertise the value required by Bob and vice versa. If they are not set the
// advertised value will be checked against the other node's default min_htlc,
// base fee and fee rate values.
func assertChannelAnnouncements(t *testing.T, alice, bob *testNode,
	capacity btcutil.Amount, customMinHtlc []lnwire.MilliSatoshi,
	customMaxHtlc []lnwire.MilliSatoshi, baseFees []lnwire.MilliSatoshi,
	feeRates []lnwire.MilliSatoshi) {

	t.Helper()

	// The following checks are to make sure the parameters are used
	// correctly, as we currently only support 2 values, one for each node.
	aliceCfg := alice.fundingMgr.cfg
	if len(customMinHtlc) > 0 {
		require.Len(t, customMinHtlc, 2, "incorrect usage")
	}
	if len(customMaxHtlc) > 0 {
		require.Len(t, customMaxHtlc, 2, "incorrect usage")
	}
	if len(baseFees) > 0 {
		require.Len(t, baseFees, 2, "incorrect usage")
	}
	if len(feeRates) > 0 {
		require.Len(t, feeRates, 2, "incorrect usage")
	}

	// After the ChannelReady message is sent, Alice and Bob will each send
	// the following messages to their gossiper:
	//	1) ChannelAnnouncement
	//	2) ChannelUpdate
	// The ChannelAnnouncement is kept locally, while the ChannelUpdate is
	// sent directly to the other peer, so the edge policies are known to
	// both peers.
	nodes := []*testNode{alice, bob}
	for j, node := range nodes {
		announcements := make([]lnwire.Message, 2)
		for i := 0; i < len(announcements); i++ {
			select {
			case announcements[i] = <-node.announceChan:
			case <-time.After(time.Second * 5):
				t.Fatalf("node didn't send announcement: %v", i)
			}
		}

		gotChannelAnnouncement := false
		gotChannelUpdate := false
		for _, msg := range announcements {
			switch m := msg.(type) {
			case *lnwire.ChannelAnnouncement1:
				gotChannelAnnouncement = true
			case *lnwire.ChannelUpdate1:

				// The channel update sent by the node should
				// advertise the MinHTLC value required by the
				// _other_ node.
				other := (j + 1) % 2
				otherCfg := nodes[other].fundingMgr.cfg

				minHtlc := otherCfg.DefaultMinHtlcIn
				maxHtlc := aliceCfg.RequiredRemoteMaxValue(
					capacity,
				)
				baseFee := aliceCfg.DefaultRoutingPolicy.BaseFee
				feeRate := aliceCfg.DefaultRoutingPolicy.FeeRate

				require.EqualValues(t, 1, m.MessageFlags)

				// We might expect a custom MinHTLC value.
				if len(customMinHtlc) > 0 {
					minHtlc = customMinHtlc[j]
				}
				require.Equal(t, minHtlc, m.HtlcMinimumMsat)

				// We might expect a custom MaxHltc value.
				if len(customMaxHtlc) > 0 {
					maxHtlc = customMaxHtlc[j]
				}
				require.Equal(t, maxHtlc, m.HtlcMaximumMsat)

				// We might expect a custom baseFee value.
				if len(baseFees) > 0 {
					baseFee = baseFees[j]
				}
				require.EqualValues(t, baseFee, m.BaseFee)

				// We might expect a custom feeRate value.
				if len(feeRates) > 0 {
					feeRate = feeRates[j]
				}
				require.EqualValues(t, feeRate, m.FeeRate)

				gotChannelUpdate = true
			}
		}

		require.Truef(
			t, gotChannelAnnouncement,
			"ChannelAnnouncement from %d", j,
		)
		require.Truef(t, gotChannelUpdate, "ChannelUpdate from %d", j)

		// Make sure no other message is sent.
		select {
		case <-node.announceChan:
			t.Fatalf("received unexpected announcement")
		case <-time.After(300 * time.Millisecond):
			// Expected
		}
	}
}

func assertAnnouncementSignatures(t *testing.T, alice, bob *testNode) {
	t.Helper()

	// After the ChannelReady message is sent and six confirmations have
	// been reached, the channel will be announced to the greater network
	// by having the nodes exchange announcement signatures.
	// Two distinct messages will be sent:
	//	1) AnnouncementSignatures
	//	2) NodeAnnouncement1
	// These may arrive in no particular order.
	// Note that sending the NodeAnnouncement1 at this point is an
	// implementation detail, and not something required by the LN spec.
	for j, node := range []*testNode{alice, bob} {
		announcements := make([]lnwire.Message, 2)
		for i := 0; i < len(announcements); i++ {
			select {
			case announcements[i] = <-node.announceChan:
			case <-time.After(time.Second * 5):
				t.Fatalf("node did not send announcement %v", i)
			}
		}

		gotAnnounceSignatures := false
		gotNodeAnnouncement := false
		for _, msg := range announcements {
			switch msg.(type) {
			case *lnwire.AnnounceSignatures1:
				gotAnnounceSignatures = true
			case *lnwire.NodeAnnouncement1:
				gotNodeAnnouncement = true
			}
		}

		if !gotAnnounceSignatures {
			t.Fatalf("did not get AnnounceSignatures from node %d",
				j)
		}
		if !gotNodeAnnouncement {
			t.Fatalf("did not get NodeAnnouncement1 from node %d",
				j)
		}
	}
}

func assertType[T any](t *testing.T, typ any) T {
	value, ok := typ.(T)
	require.True(t, ok)

	return value
}

func assertNodeAnnSent(t *testing.T, alice, bob *testNode) {
	t.Helper()

	for _, node := range []*testNode{alice, bob} {
		nodeAnn, err := lnutils.RecvOrTimeout(
			node.msgChan, time.Second*5,
		)
		require.NoError(t, err)
		assertType[*lnwire.NodeAnnouncement1](t, *nodeAnn)
	}
}

func waitForOpenUpdate(t *testing.T, updateChan chan *lnrpc.OpenStatusUpdate) {
	var openUpdate *lnrpc.OpenStatusUpdate
	select {
	case openUpdate = <-updateChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenStatusUpdate")
	}

	_, ok := openUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanOpen)
	if !ok {
		t.Fatal("OpenStatusUpdate was not OpenStatusUpdate_ChanOpen")
	}
}

func assertNoChannelState(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {
	t.Helper()

	assertErrChannelNotFound(t, alice, fundingOutPoint)
	assertErrChannelNotFound(t, bob, fundingOutPoint)
}

func assertNoFwdingPolicy(t *testing.T, alice, bob *testNode,
	chanID lnwire.ChannelID) {

	t.Helper()

	assertInitialFwdingPolicyNotFound(t, alice, &chanID)
	assertInitialFwdingPolicyNotFound(t, bob, &chanID)
}

func assertErrChannelNotFound(t *testing.T, node *testNode,
	fundingOutPoint *wire.OutPoint) {
	t.Helper()

	var state channelOpeningState
	var err error
	for i := 0; i < testPollNumTries; i++ {
		// If this is not the first try, sleep before retrying.
		if i > 0 {
			time.Sleep(testPollSleepMs * time.Millisecond)
		}
		state, _, err = node.fundingMgr.getChannelOpeningState(
			fundingOutPoint)
		if err == channeldb.ErrChannelNotFound {
			// Got expected state, return with success.
			return
		} else if err != nil {
			t.Fatalf("unable to get channel state: %v", err)
		}
	}

	// 10 tries without success.
	t.Fatalf("expected to not find state, found state %v", state)
}

func assertInitialFwdingPolicyNotFound(t *testing.T, node *testNode,
	chanID *lnwire.ChannelID) {

	t.Helper()

	err := wait.NoError(func() error {
		_, err := node.fundingMgr.getInitialForwardingPolicy(*chanID)
		if errors.Is(err, channeldb.ErrChannelNotFound) {
			return nil
		}

		return fmt.Errorf("expected ErrChannelNotFound, got %w", err)
	}, wait.DefaultTimeout)

	require.NoError(t, err)
}

func assertHandleChannelReady(t *testing.T, alice, bob *testNode,
	checkChannel ...func(node *testNode, msg *newChannelMsg) bool) {

	t.Helper()

	const timeout = time.Second * 15
	// They should both send the new channel state to their peer.
	select {
	case c := <-alice.newChannels:
		for _, check := range checkChannel {
			require.NoError(t, wait.Predicate(func() bool {
				return check(alice, c)
			}, timeout))
		}
		close(c.err)
	case <-time.After(timeout):
		t.Fatalf("alice did not send new channel to peer")
	}

	select {
	case c := <-bob.newChannels:
		for _, check := range checkChannel {
			require.NoError(t, wait.Predicate(func() bool {
				return check(bob, c)
			}, timeout))
		}
		close(c.err)
	case <-time.After(timeout):
		t.Fatalf("bob did not send new channel to peer")
	}
}

// sendAndCheckFirstConfirmation sends a transaction confirmation update to the
// given test node and verifies that the confirmation height has been set to 1
// for the specified channel. This is used when the required number of
// confirmations is a single block.
func sendAndCheckFirstConfirmation(t *testing.T, node *testNode,
	chanID lnwire.ChannelID, fundingTx *wire.MsgTx) {

	t.Helper()

	// Send an update that the transaction has been confirmed.
	node.mockNotifier.oneUpdateChannel <- chainntnfs.TxUpdateInfo{
		NumConfsLeft: 0,
		BlockHeight:  1,
	}

	// Notify the node that the transaction was mined at block height 1.
	node.mockNotifier.oneConfChannel <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: 1,
	}

	// Verify that the confirmation height is correctly set to 1 for the
	// given node and channel ID.
	assertConfirmationHeight(t, node, chanID, 1)
}

// TestFundingManagerTxReorg verifies that when the funding transaction is
// reorged out of the chain, the channel's confirmation height resets to zero,
// and that re-confirmation proceed as normal.
func TestFundingManagerTxReorg(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	fundingOutPoint, _ := openChannel(t, alice, bob, 500000, 0, 3,
		updateChan, true, nil)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Send an update that the transaction has received confirmation.
	alice.mockNotifier.oneUpdateChannel <- chainntnfs.TxUpdateInfo{
		BlockHeight:  1,
		NumConfsLeft: 2,
	}
	bob.mockNotifier.oneUpdateChannel <- chainntnfs.TxUpdateInfo{
		BlockHeight:  1,
		NumConfsLeft: 2,
	}

	// Check that the confirmation height is set to 1 for both alice and
	// bob.
	assertConfirmationHeight(t, alice, chanID, 1)
	assertConfirmationHeight(t, bob, chanID, 1)

	// Now we'll simulate a reorg of the funding transaction. This will
	// cause the confirmation height to be set to 0.
	alice.mockNotifier.reOrgChan <- 1
	bob.mockNotifier.reOrgChan <- 1

	// Check that the confirmation height is set to 0 for both alice and
	// bob.
	assertConfirmationHeight(t, alice, chanID, 0)
	assertConfirmationHeight(t, bob, chanID, 0)

	// Since the transaction is not confirmerd, there should be no channel
	// state in the database.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Send an update that the transaction has been again confirmed.
	alice.mockNotifier.oneUpdateChannel <- chainntnfs.TxUpdateInfo{
		BlockHeight:  3,
		NumConfsLeft: 2,
	}
	bob.mockNotifier.oneUpdateChannel <- chainntnfs.TxUpdateInfo{
		BlockHeight:  3,
		NumConfsLeft: 2,
	}

	// Check that the confirmation height is set to 3 for both alice and
	// bob.
	assertConfirmationHeight(t, alice, chanID, 3)
	assertConfirmationHeight(t, bob, chanID, 3)
}

func testNormalWorkflow(t *testing.T, chanType *lnwire.ChannelType) {
	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// If the channel type is set, then we need to make sure both parties
	// support explicit channel type negotiation.
	if chanType != nil {
		// Alice and Bob will have the same set of feature bits in our
		// test.
		featureBits := []lnwire.FeatureBit{
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.SimpleTaprootChannelsOptionalStaging,
		}
		alice.localFeatures = featureBits
		alice.remoteFeatures = featureBits
		bob.localFeatures = featureBits
		bob.remoteFeatures = featureBits
	}

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true,
		chanType,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Check that neither Alice nor Bob sent an error message.
	assertErrorNotSent(t, alice.msgChan)
	assertErrorNotSent(t, bob.msgChan)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// For taproot channels, the funding locked messages should have the
	// next set of verification nonces.
	if isTaprootChanType(chanType) {
		require.NotNil(t, channelReadyAlice.NextLocalNonce)
		require.NotNil(t, channelReadyBob.NextLocalNonce)
	}

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	switch {
	// For taproot channels, we expect them to only send a node
	// announcement message at this point. These channels aren't advertised
	// so we don't expect the other messages.
	case isTaprootChanType(chanType):
		assertNodeAnnSent(t, alice, bob)

	// For regular channels, we'll make sure the fundingManagers exchange
	// announcement signatures.
	case chanType == nil:
		fallthrough
	default:
		assertAnnouncementSignatures(t, alice, bob)
	}

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerNormalWorkflow tests that the funding manager is able to
// do a routine channel funding flow back and forth.
func TestFundingManagerNormalWorkflow(t *testing.T) {
	t.Parallel()

	taprootChanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	))

	testCases := []struct {
		typeName string
		chanType *lnwire.ChannelType
	}{
		{
			typeName: "normal",
			chanType: nil,
		},
		{
			typeName: "taproot",
			chanType: &taprootChanType,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.typeName, func(t *testing.T) {
			testNormalWorkflow(t, testCase.chanType)
		})
	}
}

// TestFundingManagerRejectCSV tests checking of local CSV values against our
// local CSV limit for incoming and outgoing channels.
func TestFundingManagerRejectCSV(t *testing.T) {
	t.Run("csv too high", func(t *testing.T) {
		testLocalCSVLimit(t, 400, 500)
	})
	t.Run("csv within limit", func(t *testing.T) {
		testLocalCSVLimit(t, 600, 500)
	})
}

// testLocalCSVLimit creates two funding managers, alice and bob, where alice
// has a limit on her maximum local CSV and bob sets his required CSV for alice.
// We test an incoming and outgoing channel, ensuring that alice accepts csvs
// below her maximum, and rejects those above it.
func testLocalCSVLimit(t *testing.T, aliceMaxCSV, bobRequiredCSV uint16) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Set a maximum local delay in alice's config to aliceMaxCSV and
	// overwrite bob's required remote delay function to return
	// bobRequiredCSV.
	alice.fundingMgr.cfg.MaxLocalCSVDelay = aliceMaxCSV
	bob.fundingMgr.cfg.RequiredRemoteDelay = func(_ btcutil.Amount) uint16 {
		return bobRequiredCSV
	}

	// For convenience, we bump our max pending channels to 2 so that we
	// can test incoming and outgoing channels without needing to step
	// through the full funding process.
	alice.fundingMgr.cfg.MaxPendingChannels = 2
	bob.fundingMgr.cfg.MaxPendingChannels = 2

	// If our maximum is less than the value bob sets, we expect this test
	// to fail.
	expectFail := aliceMaxCSV < bobRequiredCSV

	// First, we will initiate an outgoing channel from Alice -> Bob.
	errChan := make(chan error, 1)
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 200000,
		FundingFeePerKw: 1000,
		Updates:         updateChan,
		Err:             errChan,
	}

	// Alice should have sent the OpenChannel message to Bob.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:

	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)

	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	require.True(t, ok)

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel message.
	acceptChannelResponse := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// They now should both have pending reservations for this channel
	// active.
	assertNumPendingReservations(t, alice, bobPubKey, 1)
	assertNumPendingReservations(t, bob, alicePubKey, 1)

	// Forward the response to Alice.
	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// At this point, Alice has received an AcceptChannel message from
	// bob with the CSV value that he has set for her, and has to evaluate
	// whether she wants to accept this channel. If we get an error, we
	// assert that we expected the channel to fail, otherwise we assert that
	// she proceeded with the channel open as usual.
	select {
	case err := <-errChan:
		require.Error(t, err)
		require.True(t, expectFail)

	case msg := <-alice.msgChan:
		_, ok := msg.(*lnwire.FundingCreated)
		require.True(t, ok)
		require.False(t, expectFail)

	case <-time.After(time.Second):
		t.Fatal("funding flow was not failed")
	}

	// We do not need to complete the rest of the funding flow (it is
	// covered in other tests). So now we test that Alice will appropriately
	// handle incoming channels, opening a channel from Bob->Alice.
	errChan = make(chan error, 1)
	updateChan = make(chan *lnrpc.OpenStatusUpdate)
	initReq = &InitFundingMsg{
		Peer:            alice,
		TargetPubkey:    alice.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 200000,
		FundingFeePerKw: 1000,
		Updates:         updateChan,
		Err:             errChan,
	}

	bob.fundingMgr.InitFundingWorkflow(initReq)

	// Bob should have sent the OpenChannel message to Alice.
	var bobMsg lnwire.Message
	select {
	case bobMsg = <-bob.msgChan:

	case err := <-initReq.Err:
		t.Fatalf("bob OpenChannel message failed: %v", err)

	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send OpenChannel message")
	}

	openChannelReq, ok = bobMsg.(*lnwire.OpenChannel)
	require.True(t, ok)

	// Let Alice handle the init message.
	alice.fundingMgr.ProcessFundingMsg(openChannelReq, bob)

	// We expect a error message from Alice if we're expecting the channel
	// to fail, otherwise we expect her to proceed with the channel as
	// usual.
	select {
	case msg := <-alice.msgChan:
		var ok bool
		if expectFail {
			_, ok = msg.(*lnwire.Error)
		} else {
			_, ok = msg.(*lnwire.AcceptChannel)
		}
		require.True(t, ok)

	case <-time.After(time.Second * 5):
		t.Fatal("funding flow was not failed")
	}
}

func TestFundingManagerRestartBehavior(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true,
		nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// After the funding transaction gets mined, both nodes will send the
	// channelReady message to the other peer. If the funding node fails
	// before this message has been successfully sent, it should retry
	// sending it on restart. We mimic this behavior by letting the
	// SendToPeer method return an error, as if the message was not
	// successfully sent. We then recreate the fundingManager and make sure
	// it continues the process as expected. We'll save the current
	// implementation of sendMessage to restore the original behavior later
	// on.
	workingSendMessage := bob.sendMessage
	bob.sendMessage = func(msg lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	notifyWhenOnline := func(peer [33]byte, con chan<- lnpeer.Peer) {
		// Intentionally empty
	}
	alice.fundingMgr.cfg.NotifyWhenOnline = notifyWhenOnline

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction was mined, Bob should have successfully
	// sent the channelReady message, while Alice failed sending it. In
	// Alice's case this means that there should be no messages for Bob, and
	// the channel should still be in state 'markedOpen'
	select {
	case msg := <-alice.msgChan:
		t.Fatalf("did not expect any message from Alice: %v", msg)
	default:
		// Expected.
	}

	// Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Alice should still be markedOpen
	assertDatabaseState(t, alice, fundingOutPoint, markedOpen)

	// While Bob successfully sent channelReady.
	assertDatabaseState(t, bob, fundingOutPoint, channelReadySent)

	// We now recreate Alice's fundingManager with the correct sendMessage
	// implementation, and expect it to retry sending the channelReady
	// message. We'll explicitly shut down Alice's funding manager to
	// prevent a race when overriding the sendMessage implementation.
	if err := alice.fundingMgr.Stop(); err != nil {
		t.Fatalf("failed stop funding manager: %v", err)
	}
	bob.sendMessage = workingSendMessage
	recreateAliceFundingManager(t, alice)

	// Intentionally make the channel announcements fail
	alice.fundingMgr.cfg.SendAnnouncement = func(msg lnwire.Message,
		_ ...discovery.OptionalMsgField) chan error {

		errChan := make(chan error, 1)
		errChan <- fmt.Errorf("intentional error in SendAnnouncement")
		return errChan
	}

	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// The state should now be channelReadySent
	assertDatabaseState(t, alice, fundingOutPoint, channelReadySent)

	// Check that the channel announcements were never sent
	select {
	case ann := <-alice.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	default:
		// Expected
	}

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Next up, we check that Alice rebroadcasts the announcement
	// messages on restart. Bob should as expected send announcements.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// Next, we check that Alice sends the announcement signatures
	// on restart after six confirmations. Bob should as expected send
	// them as well.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Make sure the fundingManagers exchange announcement signatures.
	assertAnnouncementSignatures(t, alice, bob)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerOfflinePeer checks that the fundingManager waits for the
// server to notify when the peer comes online, in case sending the
// channelReady message fails the first time.
func TestFundingManagerOfflinePeer(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true,
		nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// After the funding transaction gets mined, both nodes will send the
	// channelReady message to the other peer. If the funding node fails
	// to send the channelReady message to the peer, it should wait for
	// the server to notify it that the peer is back online, and try again.
	// We'll save the current implementation of sendMessage to restore the
	// original behavior later on.
	workingSendMessage := bob.sendMessage
	bob.sendMessage = func(msg lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	peerChan := make(chan [33]byte, 1)
	conChan := make(chan chan<- lnpeer.Peer, 1)
	alice.fundingMgr.cfg.NotifyWhenOnline = func(peer [33]byte,
		connected chan<- lnpeer.Peer) {

		peerChan <- peer
		conChan <- connected
	}

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction was mined, Bob should have successfully
	// sent the channelReady message, while Alice failed sending it. In
	// Alice's case this means that there should be no messages for Bob, and
	// the channel should still be in state 'markedOpen'
	select {
	case msg := <-alice.msgChan:
		t.Fatalf("did not expect any message from Alice: %v", msg)
	default:
		// Expected.
	}

	// Bob will send channel_ready to Alice
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Alice should still be markedOpen
	assertDatabaseState(t, alice, fundingOutPoint, markedOpen)

	// While Bob successfully sent channelReady.
	assertDatabaseState(t, bob, fundingOutPoint, channelReadySent)

	// Alice should be waiting for the server to notify when Bob comes back
	// online.
	var peer [33]byte
	var con chan<- lnpeer.Peer
	select {
	case peer = <-peerChan:
		// Expected
	case <-time.After(time.Second * 3):
		t.Fatalf("alice did not register peer with server")
	}

	select {
	case con = <-conChan:
		// Expected
	case <-time.After(time.Second * 3):
		t.Fatalf("alice did not register connectedChan with server")
	}

	if !bytes.Equal(peer[:], bobPubKey.SerializeCompressed()) {
		t.Fatalf("expected to receive Bob's pubkey (%v), instead "+
			"got %v", bobPubKey, peer)
	}

	// Before sending on the con chan, update Alice's NotifyWhenOnline
	// function so that the next invocation in receivedChannelReady will
	// use this new function.
	alice.fundingMgr.cfg.NotifyWhenOnline = func(peer [33]byte,
		connected chan<- lnpeer.Peer) {

		connected <- bob
	}

	// Restore the correct sendMessage implementation, and notify that Bob
	// is back online.
	bob.sendMessage = workingSendMessage
	con <- bob

	// This should make Alice send the channelReady.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// The state should now be channelReadySent
	assertDatabaseState(t, alice, fundingOutPoint, channelReadySent)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Make sure both fundingManagers send the expected announcement
	// signatures.
	assertAnnouncementSignatures(t, alice, bob)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerPeerTimeoutAfterInitFunding checks that the zombie sweeper
// will properly clean up a zombie reservation that times out after the
// InitFundingMsg has been handled.
func TestFundingManagerPeerTimeoutAfterInitFunding(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Create a funding request and start the workflow.
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		PushAmt:         lnwire.NewMSatFromSatoshis(0),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	_, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Alice should have a new pending reservation.
	assertNumPendingReservations(t, alice, bobPubKey, 1)

	// Make sure Alice's reservation times out and then run her zombie
	// sweeper.
	time.Sleep(1 * time.Millisecond)
	go alice.fundingMgr.pruneZombieReservations()

	// Alice should have sent an Error message to Bob.
	assertErrorSent(t, alice.msgChan)

	// Alice's zombie reservation should have been pruned.
	assertNumPendingReservations(t, alice, bobPubKey, 0)
}

// TestFundingManagerPeerTimeoutAfterFundingOpen checks that the zombie sweeper
// will properly clean up a zombie reservation that times out after the
// fundingOpenMsg has been handled.
func TestFundingManagerPeerTimeoutAfterFundingOpen(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Create a funding request and start the workflow.
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		PushAmt:         lnwire.NewMSatFromSatoshis(0),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Alice should have a new pending reservation.
	assertNumPendingReservations(t, alice, bobPubKey, 1)

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel.
	assertFundingMsgSent(t, bob.msgChan, "AcceptChannel")

	// Bob should have a new pending reservation.
	assertNumPendingReservations(t, bob, alicePubKey, 1)

	// Make sure Bob's reservation times out and then run his zombie
	// sweeper.
	time.Sleep(1 * time.Millisecond)
	go bob.fundingMgr.pruneZombieReservations()

	// Bob should have sent an Error message to Alice.
	assertErrorSent(t, bob.msgChan)

	// Bob's zombie reservation should have been pruned.
	assertNumPendingReservations(t, bob, alicePubKey, 0)
}

// TestFundingManagerPeerTimeoutAfterFundingAccept checks that the zombie
// sweeper will properly clean up a zombie reservation that times out after the
// fundingAcceptMsg has been handled.
func TestFundingManagerPeerTimeoutAfterFundingAccept(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Create a funding request and start the workflow.
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		PushAmt:         lnwire.NewMSatFromSatoshis(0),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Alice should have a new pending reservation.
	assertNumPendingReservations(t, alice, bobPubKey, 1)

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel.
	acceptChannelResponse := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// Bob should have a new pending reservation.
	assertNumPendingReservations(t, bob, alicePubKey, 1)

	// Forward the response to Alice.
	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// Alice responds with a FundingCreated messages.
	assertFundingMsgSent(t, alice.msgChan, "FundingCreated")

	// Make sure Alice's reservation times out and then run her zombie
	// sweeper.
	time.Sleep(1 * time.Millisecond)
	go alice.fundingMgr.pruneZombieReservations()

	// Alice should have sent an Error message to Bob.
	assertErrorSent(t, alice.msgChan)

	// Alice's zombie reservation should have been pruned.
	assertNumPendingReservations(t, alice, bobPubKey, 0)
}

func TestFundingManagerFundingTimeout(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	_, _ = openChannel(t, alice, bob, 500000, 0, 1, updateChan, true, nil)

	// Bob will at this point be waiting for the funding transaction to be
	// confirmed, so the channel should be considered pending.
	pendingChannels, err := bob.fundingMgr.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	require.NoError(t, err, "unable to fetch pending channels")
	if len(pendingChannels) != 1 {
		t.Fatalf("Expected Bob to have 1 pending channel, had  %v",
			len(pendingChannels))
	}

	// We expect Bob to forget the channel after 2016 blocks (2 weeks), so
	// mine 2016-1, and check that it is still pending.
	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf - 1,
	}

	// Bob should still be waiting for the channel to open.
	assertNumPendingChannelsRemains(t, bob, 1)

	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf,
	}

	// Bob should have sent an Error message to Alice.
	assertErrorSent(t, bob.msgChan)

	// Should not be pending anymore.
	assertNumPendingChannelsBecomes(t, bob, 0)
}

// TestFundingManagerFundingNotTimeoutInitiator checks that if the user was
// the channel initiator, that it does not timeout when the lnd restarts.
func TestFundingManagerFundingNotTimeoutInitiator(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	_, _ = openChannel(t, alice, bob, 500000, 0, 1, updateChan, true, nil)

	// Alice will at this point be waiting for the funding transaction to be
	// confirmed, so the channel should be considered pending.
	pendingChannels, err := alice.fundingMgr.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	require.NoError(t, err, "unable to fetch pending channels")
	if len(pendingChannels) != 1 {
		t.Fatalf("Expected Alice to have 1 pending channel, had  %v",
			len(pendingChannels))
	}

	recreateAliceFundingManager(t, alice)

	// We should receive the rebroadcasted funding txn.
	select {
	case <-alice.publTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not publish funding tx")
	}

	// Increase the height to 1 minus the DefaultMaxWaitNumBlocksFundingConf
	// height.
	alice.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf - 1,
	}

	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf - 1,
	}

	// Assert both and Alice and Bob still have 1 pending channels.
	assertNumPendingChannelsRemains(t, alice, 1)

	assertNumPendingChannelsRemains(t, bob, 1)

	// Increase both Alice and Bob to DefaultMaxWaitNumBlocksFundingConf
	// height.
	alice.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf,
	}

	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight +
			lncfg.DefaultMaxWaitNumBlocksFundingConf,
	}

	// Since Alice was the initiator, the channel should not have timed out.
	assertNumPendingChannelsRemains(t, alice, 1)

	// Bob should have sent an Error message to Alice.
	assertErrorSent(t, bob.msgChan)

	// Since Bob was not the initiator, the channel should timeout.
	assertNumPendingChannelsBecomes(t, bob, 0)
}

// TestFundingManagerReceiveChannelReadyTwice checks that the fundingManager
// continues to operate as expected in case we receive a duplicate channelReady
// message.
func TestFundingManagerReceiveChannelReadyTwice(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Send the channelReady message twice to Alice, and once to Bob.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Alice should not send the channel state the second time, as the
	// second channel_ready should just be ignored.
	select {
	case <-alice.newChannels:
		t.Fatalf("alice sent new channel to peer a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}

	// Another channelReady should also be ignored, since Alice should
	// have updated her database at this point.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	select {
	case <-alice.newChannels:
		t.Fatalf("alice sent new channel to peer a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Make sure the fundingManagers exchange announcement signatures.
	assertAnnouncementSignatures(t, alice, bob)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerRestartAfterChanAnn checks that the fundingManager properly
// handles receiving a channelReady after the its own channelReady and channel
// announcement is sent and gets restarted.
func TestFundingManagerRestartAfterChanAnn(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// At this point we restart Alice's fundingManager, before she receives
	// the channelReady message. After restart, she will receive it, and
	// we expect her to be able to handle it correctly.
	recreateAliceFundingManager(t, alice)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertAnnouncementSignatures(t, alice, bob)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerRestartAfterReceivingChannelReady checks that the
// fundingManager continues to operate as expected after it has received
// channelReady and then gets restarted.
func TestFundingManagerRestartAfterReceivingChannelReady(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, true, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Let Alice immediately get the channelReady message.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)

	// Also let Bob get the channelReady message.
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// At this point we restart Alice's fundingManager.
	recreateAliceFundingManager(t, alice)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Check that the state machine is updated accordingly
	assertAddedToGraph(t, alice, bob, fundingOutPoint)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertAnnouncementSignatures(t, alice, bob)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerPrivateChannel tests that if we open a private channel
// (a channel not supposed to be announced to the rest of the network),
// the announcementSignatures nor the nodeAnnouncement messages are sent.
func TestFundingManagerPrivateChannel(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, false, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Since this is a private channel, we shouldn't receive the
	// announcement signatures.
	select {
	case ann := <-alice.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
		// Expected
	}

	select {
	case ann := <-bob.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
		// Expected
	}

	// We should however receive each side's node announcement.
	select {
	case msg := <-alice.msgChan:
		if _, ok := msg.(*lnwire.NodeAnnouncement1); !ok {
			t.Fatalf("expected to receive node announcement")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected to receive node announcement")
	}

	select {
	case msg := <-bob.msgChan:
		if _, ok := msg.(*lnwire.NodeAnnouncement1); !ok {
			t.Fatalf("expected to receive node announcement")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected to receive node announcement")
	}

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerPrivateRestart tests that the privacy guarantees granted
// by the private channel persist even on restart. This means that the
// announcement signatures nor the node announcement messages are sent upon
// restart.
func TestFundingManagerPrivateRestart(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	localAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, localAmt, pushAmt, 1, updateChan, false, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOutPoint)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	assertChannelAnnouncements(t, alice, bob, capacity, nil, nil, nil, nil)

	// Note: We don't check for the addedToGraph state because in
	// the private channel mode, the state is quickly changed from
	// addedToGraph to deleted from the database since the public
	// announcement phase is skipped.

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Notify that six confirmations has been reached on funding
	// transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Since this is a private channel, we shouldn't receive the public
	// channel announcement messages.
	select {
	case ann := <-alice.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
	}

	select {
	case ann := <-bob.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
	}

	// We should however receive each side's node announcement.
	select {
	case msg := <-alice.msgChan:
		if _, ok := msg.(*lnwire.NodeAnnouncement1); !ok {
			t.Fatalf("expected to receive node announcement")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected to receive node announcement")
	}

	select {
	case msg := <-bob.msgChan:
		if _, ok := msg.(*lnwire.NodeAnnouncement1); !ok {
			t.Fatalf("expected to receive node announcement")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected to receive node announcement")
	}

	// Restart Alice's fundingManager so we can prove that the public
	// channel announcements are not sent upon restart and that the private
	// setting persists upon restart.
	recreateAliceFundingManager(t, alice)

	select {
	case ann := <-alice.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
		// Expected
	}

	select {
	case ann := <-bob.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v",
			ann)
	case <-time.After(300 * time.Millisecond):
		// Expected
	}

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerCustomChannelParameters checks that custom requirements we
// specify during the channel funding flow is preserved correctly on both sides.
func TestFundingManagerCustomChannelParameters(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// This is the custom parameters we'll use.
	const csvDelay = 67
	const minHtlcIn = 1234
	const maxValueInFlight = 50000
	const fundingAmt = 5000000
	const chanReserve = 100000

	// Use custom channel fees. These will show up in the channel
	// reservation context.
	var (
		baseFee uint64 = 42
		feeRate uint64 = 1337
	)

	// We will consume the channel updates as we go, so no buffering is
	// needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	localAmt := btcutil.Amount(5000000)
	pushAmt := btcutil.Amount(0)
	capacity := localAmt + pushAmt

	// Create a funding request with the custom parameters and start the
	// workflow.
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:              bob,
		TargetPubkey:      bob.privKey.PubKey(),
		ChainHash:         *fundingNetParams.GenesisHash,
		LocalFundingAmt:   localAmt,
		PushAmt:           lnwire.NewMSatFromSatoshis(pushAmt),
		Private:           false,
		MaxValueInFlight:  maxValueInFlight,
		MinHtlcIn:         minHtlcIn,
		RemoteCsvDelay:    csvDelay,
		RemoteChanReserve: chanReserve,
		Updates:           updateChan,
		Err:               errChan,
		BaseFee:           &baseFee,
		FeeRate:           &feeRate,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Check that the custom CSV delay is sent as part of OpenChannel.
	if openChannelReq.CsvDelay != csvDelay {
		t.Fatalf("expected OpenChannel to have CSV delay %v, got %v",
			csvDelay, openChannelReq.CsvDelay)
	}

	// Check that the custom minHTLC value is sent.
	if openChannelReq.HtlcMinimum != minHtlcIn {
		t.Fatalf("expected OpenChannel to have minHtlc %v, got %v",
			minHtlcIn, openChannelReq.HtlcMinimum)
	}

	// Check that the max value in flight is sent as part of OpenChannel.
	if openChannelReq.MaxValueInFlight != maxValueInFlight {
		t.Fatalf("expected OpenChannel to have MaxValueInFlight %v, "+
			"got %v", maxValueInFlight,
			openChannelReq.MaxValueInFlight)
	}

	// Check that the custom remoteChanReserve value is sent.
	if openChannelReq.ChannelReserve != chanReserve {
		t.Fatalf("expected OpenChannel to have chanReserve %v, got %v",
			chanReserve, openChannelReq.ChannelReserve)
	}

	chanID := openChannelReq.PendingChannelID

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel message.
	acceptChannelResponse := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// Bob should require the default delay of 4.
	if acceptChannelResponse.CsvDelay != 4 {
		t.Fatalf("expected AcceptChannel to have CSV delay %v, got %v",
			4, acceptChannelResponse.CsvDelay)
	}

	// And the default MinHTLC value of 5.
	if acceptChannelResponse.HtlcMinimum != 5 {
		t.Fatalf("expected AcceptChannel to have minHtlc %v, got %v",
			5, acceptChannelResponse.HtlcMinimum)
	}

	reserve := lnwire.NewMSatFromSatoshis(fundingAmt / 100)
	maxValueAcceptChannel := lnwire.NewMSatFromSatoshis(fundingAmt) - reserve

	if acceptChannelResponse.MaxValueInFlight != maxValueAcceptChannel {
		t.Fatalf("expected AcceptChannel to have MaxValueInFlight %v, "+
			"got %v", maxValueAcceptChannel,
			acceptChannelResponse.MaxValueInFlight)
	}

	// Forward the response to Alice.
	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// Alice responds with a FundingCreated message.
	fundingCreated := assertFundingMsgSent(
		t, alice.msgChan, "FundingCreated",
	).(*lnwire.FundingCreated)

	// Helper method for checking the CSV delay stored for a reservation.
	assertDelay := func(resCtx *reservationWithCtx,
		ourDelay, theirDelay uint16) error {

		ourCsvDelay := resCtx.reservation.OurContribution().CsvDelay
		if ourCsvDelay != ourDelay {
			return fmt.Errorf("expected our CSV delay to be %v, "+
				"was %v", ourDelay, ourCsvDelay)
		}

		theirCsvDelay := resCtx.reservation.TheirContribution().CsvDelay
		if theirCsvDelay != theirDelay {
			return fmt.Errorf("expected their CSV delay to be %v, "+
				"was %v", theirDelay, theirCsvDelay)
		}
		return nil
	}

	// Helper method for checking the MinHtlc value stored for a
	// reservation.
	assertMinHtlc := func(resCtx *reservationWithCtx,
		expOurMinHtlc, expTheirMinHtlc lnwire.MilliSatoshi) error {

		ourMinHtlc := resCtx.reservation.OurContribution().MinHTLC
		if ourMinHtlc != expOurMinHtlc {
			return fmt.Errorf("expected our minHtlc to be %v, "+
				"was %v", expOurMinHtlc, ourMinHtlc)
		}

		theirMinHtlc := resCtx.reservation.TheirContribution().MinHTLC
		if theirMinHtlc != expTheirMinHtlc {
			return fmt.Errorf("expected their minHtlc to be %v, "+
				"was %v", expTheirMinHtlc, theirMinHtlc)
		}
		return nil
	}

	// Helper method for checking the MaxValueInFlight stored for a
	// reservation.
	assertMaxHtlc := func(resCtx *reservationWithCtx,
		expOurMaxValue, expTheirMaxValue lnwire.MilliSatoshi) error {

		ourMaxValue :=
			resCtx.reservation.OurContribution().MaxPendingAmount
		if ourMaxValue != expOurMaxValue {
			return fmt.Errorf("expected our maxValue to be %v, "+
				"was %v", expOurMaxValue, ourMaxValue)
		}

		theirMaxValue :=
			resCtx.reservation.TheirContribution().MaxPendingAmount
		if theirMaxValue != expTheirMaxValue {
			return fmt.Errorf("expected their MaxPendingAmount to "+
				"be %v, was %v", expTheirMaxValue,
				theirMaxValue)
		}
		return nil
	}

	// Helper method for checking baseFee and feeRate stored for a
	// reservation.
	assertFees := func(forwardingPolicy *models.ForwardingPolicy,
		baseFee, feeRate lnwire.MilliSatoshi) error {

		if forwardingPolicy.BaseFee != baseFee {
			return fmt.Errorf("expected baseFee to be %v, "+
				"was %v", baseFee, forwardingPolicy.BaseFee)
		}

		if forwardingPolicy.FeeRate != feeRate {
			return fmt.Errorf("expected feeRate to be %v, "+
				"was %v", feeRate, forwardingPolicy.FeeRate)
		}
		return nil
	}

	// Check that the custom channel parameters were properly set in the
	// channel reservation.
	resCtx, err := alice.fundingMgr.getReservationCtx(bobPubKey, chanID)
	require.NoError(t, err, "unable to find ctx")

	// Alice's CSV delay should be 4 since Bob sent the default value, and
	// Bob's should be 67 since Alice sent the custom value.
	if err := assertDelay(resCtx, 4, csvDelay); err != nil {
		t.Fatal(err)
	}

	// The minimum HTLC value Alice can offer should be 5, and the minimum
	// Bob can offer should be 1234.
	if err := assertMinHtlc(resCtx, 5, minHtlcIn); err != nil {
		t.Fatal(err)
	}

	// The max value in flight Alice can have should be
	// maxValueAcceptChannel, which is the default value and the maximum Bob
	// can offer should be maxValueInFlight.
	if err := assertMaxHtlc(resCtx,
		maxValueAcceptChannel, maxValueInFlight); err != nil {
		t.Fatal(err)
	}

	// The optional channel fees that will be applied in the channel
	// announcement phase. Both base fee and fee rate were provided
	// in the channel open request.
	if err := assertFees(&resCtx.forwardingPolicy, 42, 1337); err != nil {
		t.Fatal(err)
	}

	// Also make sure the parameters are properly set on Bob's end.
	resCtx, err = bob.fundingMgr.getReservationCtx(alicePubKey, chanID)
	require.NoError(t, err, "unable to find ctx")

	if err := assertDelay(resCtx, csvDelay, 4); err != nil {
		t.Fatal(err)
	}

	if err := assertMinHtlc(resCtx, minHtlcIn, 5); err != nil {
		t.Fatal(err)
	}

	if err := assertMaxHtlc(resCtx,
		maxValueInFlight, maxValueAcceptChannel); err != nil {
		t.Fatal(err)
	}

	if err := assertFees(&resCtx.forwardingPolicy, 100, 1000); err != nil {
		t.Fatal(err)
	}

	// Give the message to Bob.
	bob.fundingMgr.ProcessFundingMsg(fundingCreated, alice)

	// Finally, Bob should send the FundingSigned message.
	fundingSigned := assertFundingMsgSent(
		t, bob.msgChan, "FundingSigned",
	).(*lnwire.FundingSigned)

	// Forward the signature to Alice.
	alice.fundingMgr.ProcessFundingMsg(fundingSigned, bob)

	// After Alice processes the singleFundingSignComplete message, she will
	// broadcast the funding transaction to the network. We expect to get a
	// channel update saying the channel is pending.
	var pendingUpdate *lnrpc.OpenStatusUpdate
	select {
	case pendingUpdate = <-updateChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenStatusUpdate_ChanPending")
	}

	_, ok = pendingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	if !ok {
		t.Fatal("OpenStatusUpdate was not OpenStatusUpdate_ChanPending")
	}

	// After the funding is signed and before the channel announcement
	// we expect Alice and Bob to store their respective fees in the
	// database.
	forwardingPolicy, err := alice.fundingMgr.getInitialForwardingPolicy(
		fundingSigned.ChanID,
	)
	require.NoError(t, err)
	require.NoError(t, assertFees(forwardingPolicy, 42, 1337))

	forwardingPolicy, err = bob.fundingMgr.getInitialForwardingPolicy(
		fundingSigned.ChanID,
	)
	require.NoError(t, err)
	require.NoError(t, assertFees(forwardingPolicy, 100, 1000))

	// Wait for Alice to publish the funding tx to the network.
	var fundingTx *wire.MsgTx
	select {
	case fundingTx = <-alice.publTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not publish funding tx")
	}

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, fundingSigned.ChanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, fundingSigned.ChanID, fundingTx)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Make sure both fundingManagers send the expected channel
	// announcements.
	// Alice should advertise the default MinHTLC value of
	// 5, while bob should advertise the value minHtlc, since Alice
	// required him to use it.
	minHtlcArr := []lnwire.MilliSatoshi{5, minHtlcIn}

	// For maxHltc Alice should advertise the default MaxHtlc value of
	// maxValueAcceptChannel, while bob should advertise the value
	// maxValueInFlight since Alice required him to use it.
	maxHtlcArr := []lnwire.MilliSatoshi{
		maxValueAcceptChannel, maxValueInFlight,
	}

	// Alice should have custom fees set whereas Bob should see his
	// configured default fees announced.
	defaultDelta := bob.fundingMgr.cfg.DefaultRoutingPolicy.TimeLockDelta
	defaultBaseFee := bob.fundingMgr.cfg.DefaultRoutingPolicy.BaseFee
	defaultFeeRate := bob.fundingMgr.cfg.DefaultRoutingPolicy.FeeRate
	baseFees := []lnwire.MilliSatoshi{
		lnwire.MilliSatoshi(baseFee), defaultBaseFee,
	}
	feeRates := []lnwire.MilliSatoshi{
		lnwire.MilliSatoshi(feeRate), defaultFeeRate,
	}

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(
		t, alice, bob, func(node *testNode, msg *newChannelMsg) bool {
			aliceDB := alice.fundingMgr.cfg.ChannelDB
			chanID := lnwire.NewChanIDFromOutPoint(
				msg.channel.FundingOutpoint,
			)

			if node == alice {
				p, err := aliceDB.GetInitialForwardingPolicy(
					chanID,
				)
				require.NoError(t, err)

				return reflect.DeepEqual(
					p, &models.ForwardingPolicy{
						MaxHTLC:       maxHtlcArr[0],
						MinHTLCOut:    minHtlcArr[0],
						BaseFee:       baseFees[0],
						FeeRate:       feeRates[0],
						TimeLockDelta: defaultDelta,
					},
				)
			}

			return true
		},
	)
	assertChannelAnnouncements(
		t, alice, bob, capacity, minHtlcArr, maxHtlcArr, baseFees,
		feeRates,
	)

	// The funding transaction is now confirmed, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// Send along the 6-confirmation channel so that announcement sigs can
	// be exchanged.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	assertAnnouncementSignatures(t, alice, bob)

	// After the announcement we expect Alice and Bob to have cleared
	// the fees for the channel from the database.
	assertNoFwdingPolicy(t, alice, bob, channelReadyAlice.ChanID)
}

// TestFundingManagerInvalidChanReserve ensures proper validation is done on
// remoteChanReserve parameter sent to open channel.
func TestFundingManagerInvalidChanReserve(t *testing.T) {
	t.Parallel()

	var (
		fundingAmt  = btcutil.Amount(500000)
		pushAmt     = lnwire.NewMSatFromSatoshis(10)
		genesisHash = *fundingNetParams.GenesisHash
	)

	tests := []struct {
		name          string
		chanReserve   btcutil.Amount
		expectErr     bool
		errorContains string
	}{
		{
			name:        "Use default chan reserve",
			chanReserve: 0,
		},
		{
			name:        "Above dust but below 1% of the capacity",
			chanReserve: 400,
		},
		{
			name:        "Channel reserve below dust",
			chanReserve: 300,
			expectErr:   true,
			errorContains: "channel reserve of 300 sat is too " +
				"small",
		},
		{
			name: "Channel reserve more than 20% of the " +
				"channel capacity",
			chanReserve:   fundingAmt,
			expectErr:     true,
			errorContains: "channel reserve is too large",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			defer tearDownFundingManagers(t, alice, bob)

			// Create a funding request and start the workflow.
			updateChan := make(chan *lnrpc.OpenStatusUpdate)
			errChan := make(chan error, 1)
			initReq := &InitFundingMsg{
				Peer:              bob,
				TargetPubkey:      bob.privKey.PubKey(),
				ChainHash:         genesisHash,
				LocalFundingAmt:   fundingAmt,
				PushAmt:           pushAmt,
				Updates:           updateChan,
				RemoteChanReserve: test.chanReserve,
				Err:               errChan,
			}

			alice.fundingMgr.InitFundingWorkflow(initReq)

			var err error
			select {
			case <-alice.msgChan:
			case err = <-initReq.Err:
			case <-time.After(time.Second * 5):
				t.Fatalf("no message or error received")
			}

			if !test.expectErr {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.errorContains)
		})
	}
}

// TestFundingManagerMaxPendingChannels checks that trying to open another
// channel with the same peer when MaxPending channels are pending fails.
func TestFundingManagerMaxPendingChannels(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(
		t, func(cfg *Config) {
			cfg.MaxPendingChannels = maxPending
		},
	)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Create InitFundingMsg structs for maxPending+1 channels.
	var initReqs []*InitFundingMsg
	for i := 0; i < maxPending+1; i++ {
		updateChan := make(chan *lnrpc.OpenStatusUpdate)
		errChan := make(chan error, 1)
		initReq := &InitFundingMsg{
			Peer:            bob,
			TargetPubkey:    bob.privKey.PubKey(),
			ChainHash:       *fundingNetParams.GenesisHash,
			LocalFundingAmt: 5000000,
			PushAmt:         lnwire.NewMSatFromSatoshis(0),
			Private:         false,
			Updates:         updateChan,
			Err:             errChan,
		}
		initReqs = append(initReqs, initReq)
	}

	// Kick of maxPending+1 funding workflows.
	var accepts []*lnwire.AcceptChannel
	var lastOpen *lnwire.OpenChannel
	for i, initReq := range initReqs {
		alice.fundingMgr.InitFundingWorkflow(initReq)

		// Alice should have sent the OpenChannel message to Bob.
		var aliceMsg lnwire.Message
		select {
		case aliceMsg = <-alice.msgChan:
		case err := <-initReq.Err:
			t.Fatalf("error init funding workflow: %v", err)
		case <-time.After(time.Second * 5):
			t.Fatalf("alice did not send OpenChannel message")
		}

		openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
		if !ok {
			errorMsg, gotError := aliceMsg.(*lnwire.Error)
			if gotError {
				t.Fatalf("expected OpenChannel to be sent "+
					"from bob, instead got error: %v",
					errorMsg.Error())
			}
			t.Fatalf("expected OpenChannel to be sent from "+
				"alice, instead got %T", aliceMsg)
		}

		// Let Bob handle the init message.
		bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

		// Bob should answer with an AcceptChannel message for the
		// first maxPending channels.
		if i < maxPending {
			acceptChannelResponse := assertFundingMsgSent(
				t, bob.msgChan, "AcceptChannel",
			).(*lnwire.AcceptChannel)
			accepts = append(accepts, acceptChannelResponse)
			continue
		}

		// For the last channel, Bob should answer with an error.
		lastOpen = openChannelReq
		_ = assertFundingMsgSent(
			t, bob.msgChan, "Error",
		).(*lnwire.Error)

	}

	// Forward the responses to Alice.
	var signs []*lnwire.FundingSigned
	for _, accept := range accepts {
		alice.fundingMgr.ProcessFundingMsg(accept, bob)

		// Alice responds with a FundingCreated message.
		fundingCreated := assertFundingMsgSent(
			t, alice.msgChan, "FundingCreated",
		).(*lnwire.FundingCreated)

		// Give the message to Bob.
		bob.fundingMgr.ProcessFundingMsg(fundingCreated, alice)

		// Finally, Bob should send the FundingSigned message.
		fundingSigned := assertFundingMsgSent(
			t, bob.msgChan, "FundingSigned",
		).(*lnwire.FundingSigned)

		signs = append(signs, fundingSigned)
	}

	// Sending another init request from Alice should still make Bob
	// respond with an error.
	bob.fundingMgr.ProcessFundingMsg(lastOpen, alice)
	_ = assertFundingMsgSent(
		t, bob.msgChan, "Error",
	).(*lnwire.Error)

	// Give the FundingSigned messages to Alice.
	var txs []*wire.MsgTx
	for i, sign := range signs {
		alice.fundingMgr.ProcessFundingMsg(sign, bob)

		// Alice should send a status update for each channel, and
		// publish a funding tx to the network.
		var pendingUpdate *lnrpc.OpenStatusUpdate
		select {
		case pendingUpdate = <-initReqs[i].Updates:
		case <-time.After(time.Second * 5):
			t.Fatalf("alice did not send " +
				"OpenStatusUpdate_ChanPending")
		}

		_, ok := pendingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		if !ok {
			t.Fatal("OpenStatusUpdate was not " +
				"OpenStatusUpdate_ChanPending")
		}

		select {
		case tx := <-alice.publTxChan:
			txs = append(txs, tx)
		case <-time.After(time.Second * 5):
			t.Fatalf("alice did not publish funding tx")
		}

	}

	// Sending another init request from Alice should still make Bob
	// respond with an error, since the funding transactions are not
	// confirmed yet,
	bob.fundingMgr.ProcessFundingMsg(lastOpen, alice)
	_ = assertFundingMsgSent(
		t, bob.msgChan, "Error",
	).(*lnwire.Error)

	// Notify that the transactions were mined.
	for i := 0; i < maxPending; i++ {
		alice.mockNotifier.oneConfChannel <- &chainntnfs.TxConfirmation{
			Tx: txs[i],
		}
		bob.mockNotifier.oneConfChannel <- &chainntnfs.TxConfirmation{
			Tx: txs[i],
		}

		// Expect both to be sending ChannelReady.
		_ = assertFundingMsgSent(
			t, alice.msgChan, "ChannelReady",
		).(*lnwire.ChannelReady)

		_ = assertFundingMsgSent(
			t, bob.msgChan, "ChannelReady",
		).(*lnwire.ChannelReady)

	}

	// Now opening another channel should work.
	bob.fundingMgr.ProcessFundingMsg(lastOpen, alice)

	// Bob should answer with an AcceptChannel message.
	_ = assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)
}

// TestFundingManagerRejectPush checks behaviour of 'rejectpush'
// option, namely that non-zero incoming push amounts are disabled.
func TestFundingManagerRejectPush(t *testing.T) {
	t.Parallel()

	// Enable 'rejectpush' option and initialize funding managers.
	alice, bob := setupFundingManagers(
		t, func(cfg *Config) {
			cfg.RejectPush = true
		},
	)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Create a funding request and start the workflow.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		PushAmt:         lnwire.NewMSatFromSatoshis(10),
		Private:         true,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Assert Bob responded with an ErrNonZeroPushAmount error.
	err := assertFundingMsgSent(t, bob.msgChan, "Error").(*lnwire.Error)
	require.ErrorContains(
		t, err, "non-zero push amounts are disabled",
		"expected ErrNonZeroPushAmount error, got \"%v\"", err.Error(),
	)
}

// TestFundingManagerMaxConfs ensures that we don't accept a funding proposal
// that proposes a MinAcceptDepth greater than the maximum number of
// confirmations we're willing to accept.
func TestFundingManagerMaxConfs(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Create a funding request and start the workflow.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		PushAmt:         lnwire.NewMSatFromSatoshis(10),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Let Bob handle the init message.
	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel message.
	acceptChannelResponse := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// Modify the AcceptChannel message Bob is proposing to including a
	// MinAcceptDepth Alice won't be willing to accept.
	acceptChannelResponse.MinAcceptDepth = chainntnfs.MaxNumConfs + 1

	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// Alice should respond back with an error indicating MinAcceptDepth is
	// too large.
	err := assertFundingMsgSent(t, alice.msgChan, "Error").(*lnwire.Error)
	if !strings.Contains(err.Error(), "minimum depth") {
		t.Fatalf("expected ErrNumConfsTooLarge, got \"%v\"",
			err.Error())
	}
}

// TestFundingManagerFundAll tests that we can initiate a funding request to
// use the funds remaining in the wallet. This should produce a funding tx with
// no change output.
func TestFundingManagerFundAll(t *testing.T) {
	t.Parallel()

	// We set up our mock wallet to control a list of UTXOs that sum to
	// less than the max channel size.
	allCoins := []*lnwallet.Utxo{
		{
			AddressType: lnwallet.WitnessPubKey,
			Value: btcutil.Amount(
				0.05 * btcutil.SatoshiPerBitcoin,
			),
			PkScript: mock.CoinPkScript,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 0,
			},
		},
		{
			AddressType: lnwallet.WitnessPubKey,
			Value: btcutil.Amount(
				0.06 * btcutil.SatoshiPerBitcoin,
			),
			PkScript: mock.CoinPkScript,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 1,
			},
		},
	}

	tests := []struct {
		spendAmt btcutil.Amount
		change   bool
	}{
		{
			// We will spend all the funds in the wallet, and
			// expects no change output.
			spendAmt: btcutil.Amount(
				0.11 * btcutil.SatoshiPerBitcoin,
			),
			change: false,
		},
		{
			// We spend a little less than the funds in the wallet,
			// so a change output should be created.
			spendAmt: btcutil.Amount(
				0.10 * btcutil.SatoshiPerBitcoin,
			),
			change: true,
		},
	}

	for _, test := range tests {
		alice, bob := setupFundingManagers(t)
		t.Cleanup(func() {
			tearDownFundingManagers(t, alice, bob)
		})

		alice.fundingMgr.cfg.Wallet.WalletController.(*mock.WalletController).Utxos = allCoins

		// We will consume the channel updates as we go, so no
		// buffering is needed.
		updateChan := make(chan *lnrpc.OpenStatusUpdate)

		// Initiate a fund channel, and inspect the funding tx.
		pushAmt := btcutil.Amount(0)
		fundingTx := fundChannel(
			t, alice, bob, test.spendAmt, pushAmt, true, 0, 0, 1,
			updateChan, true, nil,
		)

		// Check whether the expected change output is present.
		if test.change && len(fundingTx.TxOut) != 2 {
			t.Fatalf("expected 2 outputs, had %v",
				len(fundingTx.TxOut))
		}

		if !test.change && len(fundingTx.TxOut) != 1 {
			t.Fatalf("expected 1 output, had %v",
				len(fundingTx.TxOut))
		}

		// Inputs should be all funds in the wallet.
		if len(fundingTx.TxIn) != len(allCoins) {
			t.Fatalf("Had %d inputs, expected %d",
				len(fundingTx.TxIn), len(allCoins))
		}

		for i, txIn := range fundingTx.TxIn {
			if txIn.PreviousOutPoint != allCoins[i].OutPoint {
				t.Fatalf("expected outpoint to be %v, was %v",
					allCoins[i].OutPoint,
					txIn.PreviousOutPoint)
			}
		}
	}
}

// TestFundingManagerFundMax tests that we can initiate a funding request to use
// the maximum allowed funds remaining in the wallet.
func TestFundingManagerFundMax(t *testing.T) {
	t.Parallel()

	// Helper function to create a test utxos
	constructTestUtxos := func(values ...btcutil.Amount) []*lnwallet.Utxo {
		var utxos []*lnwallet.Utxo
		for _, value := range values {
			utxos = append(utxos, &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       value,
				PkScript:    mock.CoinPkScript,
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0,
				},
			})
		}

		return utxos
	}

	tests := []struct {
		name           string
		coins          []*lnwallet.Utxo
		fundUpToMaxAmt btcutil.Amount
		minFundAmt     btcutil.Amount
		pushAmt        btcutil.Amount
		change         bool
	}{
		{
			// We will spend all the funds in the wallet, and expect
			// no change output due to the dust limit.
			coins: constructTestUtxos(
				MaxBtcFundingAmount + 1,
			),
			fundUpToMaxAmt: MaxBtcFundingAmount,
			minFundAmt:     MinChanFundingSize,
			pushAmt:        0,
			change:         false,
		},
		{
			// We spend less than the funds in the wallet, so a
			// change output should be created.
			coins: constructTestUtxos(
				2 * MaxBtcFundingAmount,
			),
			fundUpToMaxAmt: MaxBtcFundingAmount,
			minFundAmt:     MinChanFundingSize,
			pushAmt:        0,
			change:         true,
		},
		{
			// We spend less than the funds in the wallet when
			// setting a smaller channel size, so a change output
			// should be created.
			coins: constructTestUtxos(
				MaxBtcFundingAmount,
			),
			fundUpToMaxAmt: MaxBtcFundingAmount / 2,
			minFundAmt:     MinChanFundingSize,
			pushAmt:        0,
			change:         true,
		},
		{
			// We are using the entirety of two inputs for the
			// funding of a channel, hence expect no change output.
			coins: constructTestUtxos(
				MaxBtcFundingAmount/2, MaxBtcFundingAmount/2,
			),
			fundUpToMaxAmt: MaxBtcFundingAmount,
			minFundAmt:     MinChanFundingSize,
			pushAmt:        0,
			change:         false,
		},
		{
			// We are using a fraction of two inputs for the funding
			// of our channel, hence expect a change output.
			coins: constructTestUtxos(
				MaxBtcFundingAmount/2, MaxBtcFundingAmount/2,
			),
			fundUpToMaxAmt: MaxBtcFundingAmount / 2,
			minFundAmt:     MinChanFundingSize,
			pushAmt:        0,
			change:         true,
		},
		{
			// We are funding a channel with half of the balance in
			// our wallet hence expect a change output. Furthermore
			// we push half of the funding amount to the remote end
			// which we expect to succeed.
			coins:          constructTestUtxos(MaxBtcFundingAmount),
			fundUpToMaxAmt: MaxBtcFundingAmount / 2,
			minFundAmt:     MinChanFundingSize / 4,
			pushAmt:        MaxBtcFundingAmount / 4,
			change:         true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// We set up our mock wallet to control a list of UTXOs
			// that sum to more than the max channel size.
			addFunds := func(fundingCfg *Config) {
				wc := fundingCfg.Wallet.WalletController
				mockWc, ok := wc.(*mock.WalletController)
				if ok {
					mockWc.Utxos = test.coins
				}
			}
			alice, bob := setupFundingManagers(t, addFunds)
			defer tearDownFundingManagers(t, alice, bob)

			// We will consume the channel updates as we go, so no
			// buffering is needed.
			updateChan := make(chan *lnrpc.OpenStatusUpdate)

			// Initiate a fund channel, and inspect the funding tx.
			pushAmt := test.pushAmt
			fundingTx := fundChannel(
				t, alice, bob, 0, pushAmt, false,
				test.fundUpToMaxAmt, test.minFundAmt, 1,
				updateChan, true, nil,
			)

			// Check whether the expected change output is present.
			if test.change {
				require.EqualValues(t, 2, len(fundingTx.TxOut))
			}

			if !test.change {
				require.EqualValues(t, 1, len(fundingTx.TxOut))
			}

			// Inputs should be all funds in the wallet.
			require.Equal(t, len(test.coins), len(fundingTx.TxIn))

			for i, txIn := range fundingTx.TxIn {
				require.Equal(
					t, test.coins[i].OutPoint,
					txIn.PreviousOutPoint,
				)
			}
		})
	}
}

// TestGetUpfrontShutdown tests different combinations of inputs for getting a
// shutdown script. It varies whether the peer has the feature set, whether
// the user has provided a script and our local configuration to test that
// GetUpfrontShutdownScript returns the expected outcome.
func TestGetUpfrontShutdownScript(t *testing.T) {
	t.Parallel()

	upfrontScript := []byte("upfront script")
	generatedScript := []byte("generated script")

	getScript := func(_ bool) (lnwire.DeliveryAddress, error) {
		return generatedScript, nil
	}

	tests := []struct {
		name           string
		getScript      func(bool) (lnwire.DeliveryAddress, error)
		upfrontScript  lnwire.DeliveryAddress
		peerEnabled    bool
		localEnabled   bool
		expectedScript lnwire.DeliveryAddress
		expectedErr    error
	}{
		{
			name:      "peer disabled, no shutdown",
			getScript: getScript,
		},
		{
			name:          "peer disabled, upfront provided",
			upfrontScript: upfrontScript,
			expectedErr:   errUpfrontShutdownScriptNotSupported,
		},
		{
			name:           "peer enabled, upfront provided",
			upfrontScript:  upfrontScript,
			peerEnabled:    true,
			expectedScript: upfrontScript,
		},
		{
			name:        "peer enabled, local disabled",
			peerEnabled: true,
		},
		{
			name:           "local enabled, no upfront script",
			getScript:      getScript,
			peerEnabled:    true,
			localEnabled:   true,
			expectedScript: generatedScript,
		},
		{
			name:           "local enabled, upfront script",
			peerEnabled:    true,
			upfrontScript:  upfrontScript,
			localEnabled:   true,
			expectedScript: upfrontScript,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			var mockPeer testNode

			// If the remote peer in the test should support
			// upfront shutdown, add the feature bit.
			if test.peerEnabled {
				mockPeer.remoteFeatures = []lnwire.FeatureBit{
					lnwire.UpfrontShutdownScriptOptional,
				}
			}

			addr, err := getUpfrontShutdownScript(
				test.localEnabled, &mockPeer, test.upfrontScript,
				test.getScript,
			)
			if err != test.expectedErr {
				t.Fatalf("got: %v, expected error: %v", err,
					test.expectedErr)
			}

			if !bytes.Equal(addr, test.expectedScript) {
				t.Fatalf("expected address: %x, got: %x",
					test.expectedScript, addr)
			}
		})
	}
}

func expectOpenChannelMsg(t *testing.T,
	msgChan chan lnwire.Message) *lnwire.OpenChannel {

	var msg lnwire.Message
	select {
	case msg = <-msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("node did not send OpenChannel message")
	}

	openChannelReq, ok := msg.(*lnwire.OpenChannel)
	if !ok {
		errorMsg, gotError := msg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected OpenChannel to be sent "+
				"from bob, instead got error: %v",
				errorMsg.Error())
		}
		t.Fatalf("expected OpenChannel to be sent, instead got %T",
			msg)
	}

	return openChannelReq
}

func TestMaxChannelSizeConfig(t *testing.T) {
	t.Parallel()

	// Create a set of funding managers that will reject wumbo
	// channels but set --maxchansize explicitly lower than soft-limit.
	// Verify that wumbo rejecting funding managers will respect
	// --maxchansize below 16777215 satoshi (MaxBtcFundingAmount) limit.
	alice, bob := setupFundingManagers(t, func(cfg *Config) {
		cfg.NoWumboChans = true
		cfg.MaxChanSize = MaxBtcFundingAmount - 1
	})

	// Attempt to create a channel above the limit
	// imposed by --maxchansize, which should be rejected.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: MaxBtcFundingAmount,
		PushAmt:         lnwire.NewMSatFromSatoshis(0),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	// After processing the funding open message, bob should respond with
	// an error rejecting the channel that exceeds size limit.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg := expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertErrorSent(t, bob.msgChan)

	// Create a set of funding managers that will reject wumbo
	// channels but set --maxchansize explicitly higher than soft-limit
	// A --maxchansize greater than this limit should have no effect.
	tearDownFundingManagers(t, alice, bob)
	alice, bob = setupFundingManagers(t, func(cfg *Config) {
		cfg.NoWumboChans = true
		cfg.MaxChanSize = MaxBtcFundingAmount + 1
	})

	// Reset the Peer to the newly created one.
	initReq.Peer = bob

	// We expect Bob to respond with an Accept channel message.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg = expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertFundingMsgSent(t, bob.msgChan, "AcceptChannel")

	// Verify that wumbo accepting funding managers will respect
	// --maxchansize. Create the funding managers, this time allowing wumbo
	// channels but setting --maxchansize explicitly.
	tearDownFundingManagers(t, alice, bob)
	alice, bob = setupFundingManagers(t, func(cfg *Config) {
		cfg.NoWumboChans = false
		cfg.MaxChanSize = btcutil.Amount(100000000)
	})

	// Reset the Peer to the newly created one.
	initReq.Peer = bob

	// Attempt to create a channel above the limit
	// imposed by --maxchansize, which should be rejected.
	initReq.LocalFundingAmt = btcutil.SatoshiPerBitcoin + 1

	// After processing the funding open message, bob should respond with
	// an error rejecting the channel that exceeds size limit.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg = expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertErrorSent(t, bob.msgChan)
}

// TestWumboChannelConfig tests that the funding manager will respect the wumbo
// channel config param when creating or accepting new channels.
func TestWumboChannelConfig(t *testing.T) {
	t.Parallel()

	// First we'll create a set of funding managers that will reject wumbo
	// channels.
	alice, bob := setupFundingManagers(t, func(cfg *Config) {
		cfg.NoWumboChans = true
	})

	// If we attempt to initiate a new funding open request to Alice,
	// that's below the wumbo channel mark, we should be able to start the
	// funding process w/o issue.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: MaxBtcFundingAmount,
		PushAmt:         lnwire.NewMSatFromSatoshis(0),
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}

	// We expect Bob to respond with an Accept channel message.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg := expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertFundingMsgSent(t, bob.msgChan, "AcceptChannel")

	// We'll now attempt to create a channel above the wumbo mark, which
	// should be rejected.
	initReq.LocalFundingAmt = btcutil.SatoshiPerBitcoin

	// After processing the funding open message, bob should respond with an
	// error rejecting the channel.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg = expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertErrorSent(t, bob.msgChan)

	// Next, we'll re-create the funding managers, but this time allowing
	// wumbo channels explicitly.
	tearDownFundingManagers(t, alice, bob)
	alice, bob = setupFundingManagers(t, func(cfg *Config) {
		cfg.NoWumboChans = false
		cfg.MaxChanSize = MaxBtcFundingAmountWumbo
	})

	// Reset the Peer to the newly created one.
	initReq.Peer = bob

	// We should now be able to initiate a wumbo channel funding w/o any
	// issues.
	alice.fundingMgr.InitFundingWorkflow(initReq)
	openChanMsg = expectOpenChannelMsg(t, alice.msgChan)
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)
	assertFundingMsgSent(t, bob.msgChan, "AcceptChannel")
}

// TestFundingManagerUpfrontShutdown asserts that we'll properly fail out if
// an invalid upfront shutdown script is sent in the open_channel message.
// Since both the open_channel and accept_message logic validate the script
// using the same validation function, it suffices to just check the
// open_channel case.
func TestFundingManagerUpfrontShutdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pkscript  []byte
		expectErr bool
	}{
		{
			name: "p2pk script",
			pkscript: []byte("\x21\x02\xd3\x00\x50\x2f\x61\x15" +
				"\x0d\x58\x0a\x42\xa0\x99\x63\xe3\x47\xa2" +
				"\xad\x3c\xe5\x1f\x11\x96\x0d\x35\x8d\xf8" +
				"\xf3\x94\xf9\x67\x2a\x67\xac"),
			expectErr: true,
		},
		{
			name: "op return script",
			pkscript: []byte("\x6a\x24\xaa\x21\xa9\xed\x18\xa9" +
				"\x93\x58\x94\xbd\x48\x9b\xeb\x87\x66\x13" +
				"\x60\xbc\x80\x92\xab\xf6\xdd\xe9\x1e\x82" +
				"\x0c\x7d\x91\x89\x9d\x0a\x02\x34\x14\x3f"),
			expectErr: true,
		},
		{
			name: "standard (non-p2sh) 2-of-3 multisig",
			pkscript: []byte("\x51\x41\x04\xcc\x71\xeb\x30\xd6" +
				"\x53\xc0\xc3\x16\x39\x90\xc4\x7b\x97\x6f" +
				"\x3f\xb3\xf3\x7c\xcc\xdc\xbe\xdb\x16\x9a" +
				"\x1d\xfe\xf5\x8b\xbf\xbf\xaf\xf7\xd8\xa4" +
				"\x73\xe7\xe2\xe6\xd3\x17\xb8\x7b\xaf\xe8" +
				"\xbd\xe9\x7e\x3c\xf8\xf0\x65\xde\xc0\x22" +
				"\xb5\x1d\x11\xfc\xdd\x0d\x34\x8a\xc4\x41" +
				"\x04\x61\xcb\xdc\xc5\x40\x9f\xb4\xb4\xd4" +
				"\x2b\x51\xd3\x33\x81\x35\x4d\x80\xe5\x50" +
				"\x07\x8c\xb5\x32\xa3\x4b\xfa\x2f\xcf\xde" +
				"\xb7\xd7\x65\x19\xae\xcc\x62\x77\x0f\x5b" +
				"\x0e\x4e\xf8\x55\x19\x46\xd8\xa5\x40\x91" +
				"\x1a\xbe\x3e\x78\x54\xa2\x6f\x39\xf5\x8b" +
				"\x25\xc1\x53\x42\xaf\x52\xae"),
			expectErr: true,
		},
		{
			name:      "nonstandard script",
			pkscript:  []byte("\x99"),
			expectErr: true,
		},
		{
			name: "p2sh script",
			pkscript: []byte("\xa9\x14\xfe\x44\x10\x65\xb6\x53" +
				"\x22\x31\xde\x2f\xac\x56\x31\x52\x20\x5e" +
				"\xc4\xf5\x9c\x74\x87"),
			expectErr: true,
		},
		{
			name: "p2pkh script",
			pkscript: []byte("\x76\xa9\x14\x64\x1a\xd5\x05\x1e" +
				"\xdd\x97\x02\x9a\x00\x3f\xe9\xef\xb2\x93" +
				"\x59\xfc\xee\x40\x9d\x88\xac"),
			expectErr: true,
		},
		{
			name: "p2wpkh script",
			pkscript: []byte("\x00\x14\x4b\xfe\x98\x3f\x16\xaa" +
				"\xde\x1b\x1c\xb1\x54\x5a\xa5\xa4\x88\xd5" +
				"\xe3\x68\xb5\xdc"),
			expectErr: false,
		},
		{
			name: "p2wsh script",
			pkscript: []byte("\x00\x20\x1d\xd6\x3c\x20\x13\x89" +
				"\x3a\x8b\x41\x5e\xb2\xe7\x41\x8f\x07\x5d" +
				"\x4f\x3b\xf1\x81\x34\x99\xef\x31\xfb\xd7" +
				"\x8c\xa8\xc4\x5d\x8f\xf0"),
			expectErr: false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testUpfrontFailure(t, test.pkscript, test.expectErr)
		})
	}
}

func testUpfrontFailure(t *testing.T, pkscript []byte, expectErr bool) {
	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	errChan := make(chan error, 1)
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	fundingAmt := btcutil.Amount(500000)
	pushAmt := lnwire.NewMSatFromSatoshis(btcutil.Amount(0))

	initReq := &InitFundingMsg{
		Peer:            alice,
		TargetPubkey:    alice.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		SubtractFees:    false,
		LocalFundingAmt: fundingAmt,
		PushAmt:         pushAmt,
		FundingFeePerKw: 1000,
		Private:         false,
		Updates:         updateChan,
		Err:             errChan,
	}
	bob.fundingMgr.InitFundingWorkflow(initReq)

	// Bob should send an open_channel message to Alice.
	var bobMsg lnwire.Message
	select {
	case bobMsg = <-bob.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("received unexpected error: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("timed out waiting for bob's message")
	}

	bobOpenChan, ok := bobMsg.(*lnwire.OpenChannel)
	require.True(t, ok, "did not receive OpenChannel")

	// Set the UpfrontShutdownScript in OpenChannel.
	bobOpenChan.UpfrontShutdownScript = lnwire.DeliveryAddress(pkscript)

	// Send the OpenChannel message to Alice now. If we expected an error,
	// check that we received it.
	alice.fundingMgr.ProcessFundingMsg(bobOpenChan, bob)

	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("timed out waiting for alice's message")
	}

	if expectErr {
		// Assert that Error was received.
		_, ok = aliceMsg.(*lnwire.Error)
		require.True(t, ok, "did not receive Error")
	} else {
		// Assert that AcceptChannel was received.
		_, ok = aliceMsg.(*lnwire.AcceptChannel)
		require.True(t, ok, "did not receive AcceptChannel")
	}
}

func testZeroConf(t *testing.T, chanType *lnwire.ChannelType) {
	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Alice and Bob will have the same set of feature bits in our test.
	featureBits := []lnwire.FeatureBit{
		lnwire.ZeroConfOptional,
		lnwire.ScidAliasOptional,
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.SimpleTaprootChannelsOptionalStaging,
	}
	alice.localFeatures = featureBits
	alice.remoteFeatures = featureBits
	bob.localFeatures = featureBits
	bob.remoteFeatures = featureBits

	fundingAmt := btcutil.Amount(500000)
	pushAmt := btcutil.Amount(0)
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Construct the zero-conf ChannelType for use in open_channel.
	var channelTypeBits []lnwire.FeatureBit
	if isTaprootChanType(chanType) {
		channelTypeBits = []lnwire.FeatureBit{
			lnwire.ZeroConfRequired,
			lnwire.SimpleTaprootChannelsRequiredStaging,
		}
	} else {
		channelTypeBits = []lnwire.FeatureBit{
			lnwire.ZeroConfRequired,
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		}
	}

	channelType := lnwire.ChannelType(
		*lnwire.NewRawFeatureVector(channelTypeBits...),
	)

	// Create a default-accept channelacceptor so that the test passes and
	// we don't have to use any goroutines.
	mockAcceptor := &mockZeroConfAcceptor{}
	bob.fundingMgr.cfg.OpenChannelPredicate = mockAcceptor

	// Call fundChannel with the zero-conf ChannelType.
	fundingTx := fundChannel(
		t, alice, bob, fundingAmt, pushAmt, false, 0, 0, 1, updateChan,
		true, &channelType,
	)
	fundingOp := &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 0,
	}
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOp)

	// Assert that Bob's channel_ready message has an AliasScid.
	bobChannelReady, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)
	require.NotNil(t, bobChannelReady.AliasScid)
	require.Equal(t, *bobChannelReady.AliasScid, alias)

	// Do the same for Alice as well.
	aliceChannelReady, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)
	require.NotNil(t, aliceChannelReady.AliasScid)
	require.Equal(t, *aliceChannelReady.AliasScid, alias)

	// Exchange the channel_ready messages.
	alice.fundingMgr.ProcessFundingMsg(bobChannelReady, bob)
	bob.fundingMgr.ProcessFundingMsg(aliceChannelReady, alice)

	// We'll assert that they both create new links.
	assertHandleChannelReady(t, alice, bob)

	// We'll now assert that both sides send ChannelAnnouncement and
	// ChannelUpdate messages.
	assertChannelAnnouncements(
		t, alice, bob, fundingAmt, nil, nil, nil, nil,
	)

	// We'll now wait for the OpenStatusUpdate_ChanOpen update.
	waitForOpenUpdate(t, updateChan)

	// Assert that both Alice & Bob are in the addedToGraph state.
	assertAddedToGraph(t, alice, bob, fundingOp)

	// We'll now restart Alice's funding manager and assert that the tx
	// is rebroadcast.
	recreateAliceFundingManager(t, alice)

	select {
	case <-alice.publTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("timed out waiting for alice to rebroadcast tx")
	}

	// We'll now confirm the funding transaction.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: 1,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: 1,
	}

	// Check that the confirmation height is set to 1 for both alice and
	// bob.
	assertConfirmationHeight(t, alice, chanID, 1)
	assertConfirmationHeight(t, bob, chanID, 1)

	// Both Alice and Bob should call ReportShortChanID first (before
	// sending announcements) to avoid a race where other nodes learn about
	// the confirmed SCID before the switch is ready.
	select {
	case <-alice.reportScidChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("did not call ReportShortChanID in time")
	}

	select {
	case <-bob.reportScidChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("did not call ReportShortChanID in time")
	}

	// For taproot channels, we don't expect them to be announced atm.
	if !isTaprootChanType(chanType) {
		assertChannelAnnouncements(
			t, alice, bob, fundingAmt, nil, nil, nil, nil,
		)
	}

	// Send along the 6-confirmation channel so that announcement sigs can
	// be exchanged.
	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	switch {
	// For taproot channels, we expect them to only send a node
	// announcement message at this point. These channels aren't advertised
	// so we don't expect the other messages.
	case isTaprootChanType(chanType):
		assertNodeAnnSent(t, alice, bob)

	// For regular channels, we'll make sure the fundingManagers exchange
	// announcement signatures.
	case chanType == nil:
		fallthrough
	default:
		assertAnnouncementSignatures(t, alice, bob)
	}

	// Assert that the channel state is deleted from the fundingmanager's
	// datastore.
	assertNoChannelState(t, alice, bob, fundingOp)

	// The forwarding policy for the channel announcement should
	// have been deleted from the database, as the channel is announced.
	assertNoFwdingPolicy(t, alice, bob, aliceChannelReady.ChanID)
}

// TestCommitmentTypeFundmaxSanityCheck was introduced as a way of reminding
// developers of new channel commitment types to also consider the channel
// opening behavior with a specified fundmax flag. To give a hypothetical
// example, if ANCHOR types had been introduced after the fundmax flag had been
// activated, the developer would have had to code for the anchor reserve in the
// funding manager in the context of public and private channels. Otherwise,
// inconsistent behavior would have resulted when specifying fundmax for
// different types of channel openings.
// To ensure consistency this test compares a map of locally defined channel
// commitment types to the list of channel types that are defined in the proto
// files. It fails if the proto files contain additional commitment types. Once
// the developer considered the new channel type behavior it can be added in
// this test to the map `allCommitmentTypes`.
func TestCommitmentTypeFundmaxSanityCheck(t *testing.T) {
	t.Parallel()
	allCommitmentTypes := map[string]int{
		"UNKNOWN_COMMITMENT_TYPE": 0,
		"LEGACY":                  1,
		"STATIC_REMOTE_KEY":       2,
		"ANCHORS":                 3,
		"SCRIPT_ENFORCED_LEASE":   4,
		"SIMPLE_TAPROOT":          5,
		"SIMPLE_TAPROOT_OVERLAY":  6,
	}

	for commitmentType := range lnrpc.CommitmentType_value {
		if _, ok := allCommitmentTypes[commitmentType]; !ok {
			t.Fatalf("Commitment type %s hasn't been considered "+
				"in the context of the --fundmax flag for "+
				"channel openings.", commitmentType)
		}
	}
}

// TestFundingManagerNoEchoChanType tests that the funding flow is aborted if
// the peer fails to echo back the channel type in AcceptChannel.
func TestFundingManagerNoEchoChanType(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Alice and Bob will have the same set of feature bits in our test.
	featureBits := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
	}
	alice.localFeatures = featureBits
	alice.remoteFeatures = featureBits
	bob.localFeatures = featureBits
	bob.remoteFeatures = featureBits

	expectedChanType := (*lnwire.ChannelType)(lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	))

	// Create a funding request and start the workflow.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should have sent the OpenChannel message to Bob.
	openChanMsg := expectOpenChannelMsg(t, alice.msgChan)

	require.Equal(t, expectedChanType, openChanMsg.ChannelType)

	// Let Bob handle the OpenChannel message.
	bob.fundingMgr.ProcessFundingMsg(openChanMsg, alice)

	acceptChanMsg, _ := assertFundingMsgSent(t, bob.msgChan,
		"AcceptChannel").(*lnwire.AcceptChannel)

	require.Equal(t, expectedChanType, acceptChanMsg.ChannelType)

	// Drop the channel type and ensure Alice responds with an error.
	acceptChanMsg.ChannelType = nil
	alice.fundingMgr.ProcessFundingMsg(acceptChanMsg, bob)
	assertFundingMsgSent(t, alice.msgChan, "Error")
}

// TestFundingManagerZeroConf tests that the fundingmanager properly handles
// the whole flow for zero-conf channels.
func TestFundingManagerZeroConf(t *testing.T) {
	t.Parallel()

	taprootChanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	))

	testCases := []struct {
		typeName string
		chanType *lnwire.ChannelType
	}{
		{
			typeName: "normal",
			chanType: nil,
		},
		{
			typeName: "taproot",
			chanType: &taprootChanType,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.typeName, func(t *testing.T) {
			testZeroConf(t, testCase.chanType)
		})
	}
}

// TestFundingManagerCoinbase tests that a coinbase transaction that is also a
// funding transaction is not used until the coinbase maturity has passed.
func TestFundingManagerCoinbase(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	const chanSize = btcutil.Amount(1_000_000)

	aliceKeyRing := alice.fundingMgr.cfg.Wallet.SecretKeyRing
	bobKeyRing := bob.fundingMgr.cfg.Wallet.SecretKeyRing

	// Since we want to know the multi-sig keys upfront, derive the keys
	// at index 0.
	multiSigKeyLoc := keychain.KeyLocator{
		Family: keychain.KeyFamilyMultiSig,
		Index:  0,
	}

	aliceKeyDesc, err := aliceKeyRing.DeriveKey(multiSigKeyLoc)
	require.NoError(t, err)

	bobKeyDesc, err := bobKeyRing.DeriveKey(multiSigKeyLoc)
	require.NoError(t, err)

	// Craft the coinbase transaction that will spend to the 2-of-2 p2wsh.
	fundingTx := wire.NewMsgTx(2)

	// The all-zero hash and the index of math.MaxUint32 makes this a
	// coinbase transaction.
	txIn := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: math.MaxUint32,
		},
	}

	fundingTx.TxIn = append(fundingTx.TxIn, txIn)

	_, txOut, err := input.GenFundingPkScript(
		aliceKeyDesc.PubKey.SerializeCompressed(),
		bobKeyDesc.PubKey.SerializeCompressed(), int64(chanSize),
	)
	require.NoError(t, err)

	fundingTx.TxOut = append(fundingTx.TxOut, txOut)

	fundingOp := &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 0,
	}
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOp)

	chanFunder := &mockChanFunder{
		fundingAmt: chanSize,
		localKey:   &aliceKeyDesc,
		remoteKey:  bobKeyDesc.PubKey,
		chanPoint:  fundingOp,
	}

	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)

	initReq := &InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		MinConfs:        int32(1),
		LocalFundingAmt: chanSize,
		ChanFunder:      chanFunder,
		Updates:         updateChan,
		Err:             errChan,
	}

	alice.fundingMgr.InitFundingWorkflow(initReq)

	// Alice should send an OpenChannel to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.Err:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenChannel message")
	}

	openChannelReq, ok := aliceMsg.(*lnwire.OpenChannel)
	require.True(t, ok)

	bob.fundingMgr.ProcessFundingMsg(openChannelReq, alice)

	// Bob should answer with an AcceptChannel message.
	acceptChannelResponse, ok := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)
	require.True(t, ok)

	// Forward the response to Alice.
	alice.fundingMgr.ProcessFundingMsg(acceptChannelResponse, bob)

	// Alice responds with a FundingCreated message.
	fundingCreated, ok := assertFundingMsgSent(
		t, alice.msgChan, "FundingCreated",
	).(*lnwire.FundingCreated)
	require.True(t, ok)

	// Give the message to Bob.
	bob.fundingMgr.ProcessFundingMsg(fundingCreated, alice)

	// Finally, Bob should send the FundingSigned message.
	fundingSigned, ok := assertFundingMsgSent(
		t, bob.msgChan, "FundingSigned",
	).(*lnwire.FundingSigned)
	require.True(t, ok)

	// Forward the signature to Alice.
	alice.fundingMgr.ProcessFundingMsg(fundingSigned, bob)

	var pendingUpdate *lnrpc.OpenStatusUpdate
	select {
	case pendingUpdate = <-updateChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send OpenStatusUpdate_ChanPending")
	}

	_, ok = pendingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t, ok)

	// Notify that the transaction was mined, and check that the
	// confirmation height is set to 1 for both Alice and Bob.
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)

	// Make sure the notification about the pending channel was sent out.
	select {
	case <-alice.mockChanEvent.pendingOpenEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send pending channel event")
	}
	select {
	case <-bob.mockChanEvent.pendingOpenEvent:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send pending channel event")
	}

	// Assert that neither alice nor bob sees the channel as open yet. This
	// is because the coinbase check is hit and we must wait for more
	// confirmations.
	select {
	case <-alice.mockChanEvent.openEvent:
		t.Fatalf("alice sent an open channel event")
	case <-time.After(time.Second * 5):
	}

	select {
	case <-bob.mockChanEvent.openEvent:
		t.Fatalf("bob sent an open channel event")
	case <-time.After(time.Second * 5):
	}

	// Send along the oneConfChannel again and then assert that the open
	// event is sent. This serves as the 100 block + MinAcceptDepth
	// confirmation.
	alice.mockNotifier.oneConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	bob.mockNotifier.oneConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	assertMarkedOpen(t, alice, bob, fundingOp)

	// After the funding transaction is mined, Alice will send
	// channelReady to Bob.
	channelReadyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// And similarly Bob will send channel_ready to Alice.
	channelReadyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)

	// Check that the state machine is updated accordingly
	assertChannelReadySent(t, alice, bob, fundingOp)

	// Exchange the channelReady messages.
	alice.fundingMgr.ProcessFundingMsg(channelReadyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(channelReadyAlice, alice)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleChannelReady(t, alice, bob)
}
