// +build !rpctest

package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	privPass = []byte("dummy-pass")

	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		alicePrivKeyBytes[:])

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

	bobPrivKey, bobPubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		bobPrivKeyBytes[:])

	bobTCPAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.2:9000")

	bobAddr = &lnwire.NetAddress{
		IdentityKey: bobPubKey,
		Address:     bobTCPAddr,
	}
)

type mockNotifier struct {
	confChannel chan *chainntnfs.TxConfirmation
	epochChan   chan *chainntnfs.BlockEpoch
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs,
	heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChannel,
	}, nil
}
func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: m.epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}
func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

type testNode struct {
	privKey         *btcec.PrivateKey
	msgChan         chan lnwire.Message
	announceChan    chan lnwire.Message
	arbiterChan     chan *lnwallet.LightningChannel
	publTxChan      chan *wire.MsgTx
	fundingMgr      *fundingManager
	peer            *peer
	mockNotifier    *mockNotifier
	testDir         string
	shutdownChannel chan struct{}
}

func disableFndgLogger(t *testing.T) {
	channeldb.UseLogger(btclog.Disabled)
	lnwallet.UseLogger(btclog.Disabled)
	fndgLog = btclog.Disabled
}

func createTestWallet(cdb *channeldb.DB, netParams *chaincfg.Params,
	notifier chainntnfs.ChainNotifier, wc lnwallet.WalletController,
	signer lnwallet.Signer, bio lnwallet.BlockChainIO,
	estimator lnwallet.FeeEstimator) (*lnwallet.LightningWallet, error) {

	wallet, err := lnwallet.NewLightningWallet(lnwallet.Config{
		Database:         cdb,
		Notifier:         notifier,
		WalletController: wc,
		Signer:           signer,
		ChainIO:          bio,
		FeeEstimator:     estimator,
		NetParams:        *netParams,
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
	tempTestDir string) (*testNode, error) {

	netParams := activeNetParams.Params
	estimator := lnwallet.StaticFeeEstimator{FeeRate: 250}

	chainNotifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation, 1),
		epochChan:   make(chan *chainntnfs.BlockEpoch, 1),
	}

	newChannelsChan := make(chan *newChannelMsg)
	p := &peer{
		newChannels: newChannelsChan,
	}

	sentMessages := make(chan lnwire.Message)
	sentAnnouncements := make(chan lnwire.Message)
	publTxChan := make(chan *wire.MsgTx, 1)
	arbiterChan := make(chan *lnwallet.LightningChannel)
	shutdownChan := make(chan struct{})

	wc := &mockWalletController{
		rootKey:               alicePrivKey,
		publishedTransactions: publTxChan,
	}
	signer := &mockSigner{
		key: alicePrivKey,
	}
	bio := &mockChainIO{}

	dbDir := filepath.Join(tempTestDir, "cdb")
	cdb, err := channeldb.Open(dbDir)
	if err != nil {
		return nil, err
	}

	lnw, err := createTestWallet(cdb, netParams,
		chainNotifier, wc, signer, bio, estimator)
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}

	var chanIDSeed [32]byte

	f, err := newFundingManager(fundingConfig{
		IDKey:        privKey.PubKey(),
		Wallet:       lnw,
		Notifier:     chainNotifier,
		FeeEstimator: estimator,
		SignMessage: func(pubKey *btcec.PublicKey, msg []byte) (*btcec.Signature, error) {
			return nil, nil
		},
		SendAnnouncement: func(msg lnwire.Message) error {
			select {
			case sentAnnouncements <- msg:
			case <-shutdownChan:
				return fmt.Errorf("shutting down")
			}
			return nil
		},
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement, error) {
			return lnwire.NodeAnnouncement{}, nil
		},
		ArbiterChan: arbiterChan,
		SendToPeer: func(target *btcec.PublicKey, msgs ...lnwire.Message) error {
			select {
			case sentMessages <- msgs[0]:
			case <-shutdownChan:
				return fmt.Errorf("shutting down")
			}
			return nil
		},
		NotifyWhenOnline: func(peer *btcec.PublicKey, connectedChan chan<- struct{}) {
			t.Fatalf("did not expect fundingManager to call NotifyWhenOnline")
		},
		FindPeer: func(peerKey *btcec.PublicKey) (*peer, error) {
			return p, nil
		},
		TempChanIDSeed: chanIDSeed,
		FindChannel: func(chanID lnwire.ChannelID) (*lnwallet.LightningChannel, error) {
			dbChannels, err := cdb.FetchAllChannels()
			if err != nil {
				return nil, err
			}

			for _, channel := range dbChannels {
				if chanID.IsChanPoint(&channel.FundingOutpoint) {
					return lnwallet.NewLightningChannel(
						signer,
						nil,
						estimator,
						channel)
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		},
		NumRequiredConfs: func(chanAmt btcutil.Amount,
			pushAmt lnwire.MilliSatoshi) uint16 {

			return uint16(cfg.DefaultNumChanConfs)
		},
		RequiredRemoteDelay: func(amt btcutil.Amount) uint16 {
			return 4
		},
	})
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	if err = f.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}

	return &testNode{
		privKey:         privKey,
		msgChan:         sentMessages,
		announceChan:    sentAnnouncements,
		arbiterChan:     arbiterChan,
		publTxChan:      publTxChan,
		fundingMgr:      f,
		peer:            p,
		mockNotifier:    chainNotifier,
		testDir:         tempTestDir,
		shutdownChannel: shutdownChan,
	}, nil
}

func recreateAliceFundingManager(t *testing.T, alice *testNode) {
	// Stop the old fundingManager before creating a new one.
	close(alice.shutdownChannel)
	if err := alice.fundingMgr.Stop(); err != nil {
		t.Fatalf("unable to stop old fundingManager: %v", err)
	}

	aliceMsgChan := make(chan lnwire.Message)
	aliceAnnounceChan := make(chan lnwire.Message)
	shutdownChan := make(chan struct{})

	oldCfg := alice.fundingMgr.cfg

	f, err := newFundingManager(fundingConfig{
		IDKey:        oldCfg.IDKey,
		Wallet:       oldCfg.Wallet,
		Notifier:     oldCfg.Notifier,
		FeeEstimator: oldCfg.FeeEstimator,
		SignMessage: func(pubKey *btcec.PublicKey,
			msg []byte) (*btcec.Signature, error) {
			return nil, nil
		},
		SendAnnouncement: func(msg lnwire.Message) error {
			select {
			case aliceAnnounceChan <- msg:
			case <-shutdownChan:
				return fmt.Errorf("shutting down")
			}
			return nil
		},
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement, error) {
			return lnwire.NodeAnnouncement{}, nil
		},
		ArbiterChan: oldCfg.ArbiterChan,
		SendToPeer: func(target *btcec.PublicKey,
			msgs ...lnwire.Message) error {
			select {
			case aliceMsgChan <- msgs[0]:
			case <-shutdownChan:
				return fmt.Errorf("shutting down")
			}
			return nil
		},
		NotifyWhenOnline: func(peer *btcec.PublicKey, connectedChan chan<- struct{}) {
			t.Fatalf("did not expect fundingManager to call NotifyWhenOnline")
		},
		FindPeer:       oldCfg.FindPeer,
		TempChanIDSeed: oldCfg.TempChanIDSeed,
		FindChannel:    oldCfg.FindChannel,
	})
	if err != nil {
		t.Fatalf("failed recreating aliceFundingManager: %v", err)
	}

	alice.fundingMgr = f
	alice.msgChan = aliceMsgChan
	alice.announceChan = aliceAnnounceChan
	alice.shutdownChannel = shutdownChan

	if err = f.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}
}

func setupFundingManagers(t *testing.T) (*testNode, *testNode) {
	// We need to set the global config, as fundingManager uses
	// MaxPendingChannels, and it is usually set in lndMain().
	cfg = &config{
		MaxPendingChannels: defaultMaxPendingChannels,
	}

	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	alice, err := createTestFundingManager(t, alicePrivKey, aliceTestDir)
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	bobTestDir, err := ioutil.TempDir("", "boblnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	bob, err := createTestFundingManager(t, bobPrivKey, bobTestDir)
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	return alice, bob
}

func tearDownFundingManagers(t *testing.T, a, b *testNode) {
	close(a.shutdownChannel)
	close(b.shutdownChannel)

	if err := a.fundingMgr.Stop(); err != nil {
		t.Fatalf("unable to stop fundingManager: %v", err)
	}
	if err := b.fundingMgr.Stop(); err != nil {
		t.Fatalf("unable to stop fundingManager: %v", err)
	}
	os.RemoveAll(a.testDir)
	os.RemoveAll(b.testDir)
}

// openChannel takes the funding process to the point where the funding
// transaction is confirmed on-chain. Returns the funding out point.
func openChannel(t *testing.T, alice, bob *testNode, localFundingAmt,
	pushAmt btcutil.Amount, numConfs uint32,
	updateChan chan *lnrpc.OpenStatusUpdate) *wire.OutPoint {
	// Create a funding request and start the workflow.
	errChan := make(chan error, 1)
	initReq := &openChanReq{
		targetPeerID:    int32(1),
		targetPubkey:    bob.privKey.PubKey(),
		chainHash:       *activeNetParams.GenesisHash,
		localFundingAmt: localFundingAmt,
		pushAmt:         lnwire.NewMSatFromSatoshis(pushAmt),
		updates:         updateChan,
		err:             errChan,
	}

	alice.fundingMgr.initFundingWorkflow(bobAddr, initReq)

	// Alice should have sent the OpenChannel message to Bob.
	var aliceMsg lnwire.Message
	select {
	case aliceMsg = <-alice.msgChan:
	case err := <-initReq.err:
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
				lnwire.ErrorCode(errorMsg.Data[0]))
		}
		t.Fatalf("expected OpenChannel to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Let Bob handle the init message.
	bob.fundingMgr.processFundingOpen(openChannelReq, aliceAddr)

	// Bob should answer with an AcceptChannel.
	var bobMsg lnwire.Message
	select {
	case bobMsg = <-bob.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send AcceptChannel message")
	}

	acceptChannelResponse, ok := bobMsg.(*lnwire.AcceptChannel)
	if !ok {
		errorMsg, gotError := bobMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected AcceptChannel to be sent "+
				"from bob, instead got error: %v",
				lnwire.ErrorCode(errorMsg.Data[0]))
		}
		t.Fatalf("expected AcceptChannel to be sent from bob, "+
			"instead got %T", bobMsg)
	}

	// Forward the response to Alice.
	alice.fundingMgr.processFundingAccept(acceptChannelResponse, bobAddr)

	// Alice responds with a FundingCreated messages.
	select {
	case aliceMsg = <-alice.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send FundingCreated message")
	}
	fundingCreated, ok := aliceMsg.(*lnwire.FundingCreated)
	if !ok {
		errorMsg, gotError := aliceMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected FundingCreated to be sent "+
				"from bob, instead got error: %v",
				lnwire.ErrorCode(errorMsg.Data[0]))
		}
		t.Fatalf("expected FundingCreated to be sent from "+
			"alice, instead got %T", aliceMsg)
	}

	// Give the message to Bob.
	bob.fundingMgr.processFundingCreated(fundingCreated, aliceAddr)

	// Finally, Bob should send the FundingSigned message.
	select {
	case bobMsg = <-bob.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send FundingSigned message")
	}

	fundingSigned, ok := bobMsg.(*lnwire.FundingSigned)
	if !ok {
		errorMsg, gotError := bobMsg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected FundingSigned to be "+
				"sent from bob, instead got error: %v",
				lnwire.ErrorCode(errorMsg.Data[0]))
		}
		t.Fatalf("expected FundingSigned to be sent from "+
			"bob, instead got %T", bobMsg)
	}

	// Forward the signature to Alice.
	alice.fundingMgr.processFundingSigned(fundingSigned, bobAddr)

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

	fundingOutPoint := &wire.OutPoint{
		Hash:  publ.TxHash(),
		Index: 0,
	}
	return fundingOutPoint
}

func assertMarkedOpen(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {
	state, _, err := alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != markedOpen {
		t.Fatalf("expected state to be markedOpen, was %v", state)
	}
	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != markedOpen {
		t.Fatalf("expected state to be markedOpen, was %v", state)
	}
}

func checkNodeSendingFundingLocked(t *testing.T, node *testNode) *lnwire.FundingLocked {
	var msg lnwire.Message
	select {
	case msg = <-node.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("node did not send fundingLocked")
	}

	fundingLocked, ok := msg.(*lnwire.FundingLocked)
	if !ok {
		errorMsg, gotError := msg.(*lnwire.Error)
		if gotError {
			t.Fatalf("expected FundingLocked to be sent "+
				"from node, instead got error: %v",
				lnwire.ErrorCode(errorMsg.Data[0]))
		}
		t.Fatalf("expected FundingLocked to be sent from node, "+
			"instead got %T", msg)
	}
	return fundingLocked
}

func assertFundingLockedSent(t *testing.T, alice, bob *testNode,
	fundingOutPoint *wire.OutPoint) {
	state, _, err := alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}
	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}
}

func assertChannelAnnouncements(t *testing.T, alice, bob *testNode) {
	// After the FundingLocked message is sent, the channel will be announced.
	// A chanAnnouncement consists of three distinct messages:
	//	1) ChannelAnnouncement
	//	2) ChannelUpdate
	//	3) AnnounceSignatures
	// that will be announced in no particular order.
	// A node announcement will also be sent.
	announcements := make([]lnwire.Message, 4)
	for i := 0; i < len(announcements); i++ {
		select {
		case announcements[i] = <-alice.announceChan:
		case <-time.After(time.Second * 5):
			t.Fatalf("alice did not send announcement %v", i)
		}
	}

	gotChannelAnnouncement := false
	gotChannelUpdate := false
	gotAnnounceSignatures := false
	gotNodeAnnouncement := false

	for _, msg := range announcements {
		switch msg.(type) {
		case *lnwire.ChannelAnnouncement:
			gotChannelAnnouncement = true
		case *lnwire.ChannelUpdate:
			gotChannelUpdate = true
		case *lnwire.AnnounceSignatures:
			gotAnnounceSignatures = true
		case *lnwire.NodeAnnouncement:
			gotNodeAnnouncement = true
		}
	}

	if !gotChannelAnnouncement {
		t.Fatalf("did not get ChannelAnnouncement from Alice")
	}
	if !gotChannelUpdate {
		t.Fatalf("did not get ChannelUpdate from Alice")
	}
	if !gotAnnounceSignatures {
		t.Fatalf("did not get AnnounceSignatures from Alice")
	}
	if !gotNodeAnnouncement {
		t.Fatalf("did not get NodeAnnouncement from Alice")
	}

	// Do the check for Bob as well.
	for i := 0; i < len(announcements); i++ {
		select {
		case announcements[i] = <-bob.announceChan:
		case <-time.After(time.Second * 5):
			t.Fatalf("bob did not send announcement %v", i)
		}
	}

	gotChannelAnnouncement = false
	gotChannelUpdate = false
	gotAnnounceSignatures = false
	gotNodeAnnouncement = false

	for _, msg := range announcements {
		switch msg.(type) {
		case *lnwire.ChannelAnnouncement:
			gotChannelAnnouncement = true
		case *lnwire.ChannelUpdate:
			gotChannelUpdate = true
		case *lnwire.AnnounceSignatures:
			gotAnnounceSignatures = true
		case *lnwire.NodeAnnouncement:
			gotNodeAnnouncement = true
		}
	}

	if !gotChannelAnnouncement {
		t.Fatalf("did not get ChannelAnnouncement from Bob")
	}
	if !gotChannelUpdate {
		t.Fatalf("did not get ChannelUpdate from Bob")
	}
	if !gotAnnounceSignatures {
		t.Fatalf("did not get AnnounceSignatures from Bob")
	}
	if !gotNodeAnnouncement {
		t.Fatalf("did not get NodeAnnouncement from Bob")
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
	state, _, err := alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

}

func assertHandleFundingLocked(t *testing.T, alice, bob *testNode) {
	// They should both send the new channel to the breach arbiter.
	select {
	case <-alice.arbiterChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send channel to breach arbiter")
	}

	select {
	case <-bob.arbiterChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send channel to breach arbiter")
	}

	// And send the new channel state to their peer.
	select {
	case c := <-alice.peer.newChannels:
		close(c.done)
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send new channel to peer")
	}

	select {
	case c := <-bob.peer.newChannels:
		close(c.done)
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send new channel to peer")
	}
}

func TestFundingManagerNormalWorkflow(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// We will consume the channel updates as we go, so no buffering is needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write
	//state to database.
	time.Sleep(300 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// fundingLocked to Bob.
	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// And similarly Bob will send funding locked to Alice.
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(300 * time.Millisecond)

	// Check that the state machine is updated accordingly
	assertFundingLockedSent(t, alice, bob, fundingOutPoint)

	// Make sure both fundingManagers send the expected channel announcements.
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	time.Sleep(300 * time.Millisecond)
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Exchange the fundingLocked messages.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)
}

func TestFundingManagerRestartBehavior(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// After the funding transaction gets mined, both nodes will send the
	// fundingLocked message to the other peer. If the funding node fails
	// before this message has been successfully sent, it should retry
	// sending it on restart. We mimic this behavior by letting the
	// SendToPeer method return an error, as if the message was not
	// successfully sent. We then recreate the fundingManager and make sure
	// it continues the process as expected.
	alice.fundingMgr.cfg.SendToPeer = func(target *btcec.PublicKey,
		msgs ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	alice.fundingMgr.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey, con chan<- struct{}) {
		// Intetionally empty.
	}

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write to
	// the database.
	time.Sleep(500 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction was mined, Bob should have successfully
	// sent the fundingLocked message, while Alice failed sending it. In
	// Alice's case this means that there should be no messages for Bob, and
	// the channel should still be in state 'markedOpen'
	select {
	case msg := <-alice.msgChan:
		t.Fatalf("did not expect any message from Alice: %v", msg)
	default:
		// Expected.
	}

	// Bob will send funding locked to Alice.
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(1 * time.Second)

	// Alice should still be markedOpen
	state, _, err := alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != markedOpen {
		t.Fatalf("expected state to be markedOpen, was %v", state)
	}

	// While Bob successfully sent fundingLocked.
	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}

	// We now recreate Alice's fundingManager, and expect it to retry
	// sending the fundingLocked message.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)

	// Intetionally make the next channel announcement fail
	alice.fundingMgr.cfg.SendAnnouncement = func(msg lnwire.Message) error {
		return fmt.Errorf("intentional error in SendAnnouncement")
	}

	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// Sleep to make sure database write is finished.
	time.Sleep(500 * time.Millisecond)

	// The state should now be fundingLockedSent
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}

	// Check that the channel announcements were never sent
	select {
	case ann := <-alice.announceChan:
		t.Fatalf("unexpectedly got channel announcement message: %v", ann)
	default:
		// Expected
	}

	// Next up, we check that the Alice rebroadcasts the announcement
	// messages on restart. Bob should as expected send announcements.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished. Since we recreated the
	// fundingManager, we don't have an update channel to synchronize on,
	// so a small sleep makes sure the database writing is finished.
	time.Sleep(300 * time.Millisecond)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Exchange the fundingLocked messages.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)

}

// TestFundingManagerOfflinePeer checks that the fundingManager waits for the
// server to notify when the peer comes online, in case sending the
// fundingLocked message fails the first time.
func TestFundingManagerOfflinePeer(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// After the funding transaction gets mined, both nodes will send the
	// fundingLocked message to the other peer. If the funding node fails
	// to send the fundingLocked message to the peer, it should wait for
	// the server to notify it that the peer is back online, and try again.
	alice.fundingMgr.cfg.SendToPeer = func(target *btcec.PublicKey,
		msgs ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
	}
	peerChan := make(chan *btcec.PublicKey, 1)
	conChan := make(chan chan<- struct{}, 1)
	alice.fundingMgr.cfg.NotifyWhenOnline = func(peer *btcec.PublicKey, connected chan<- struct{}) {
		peerChan <- peer
		conChan <- connected
	}

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write to
	// the database.
	time.Sleep(500 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction was mined, Bob should have successfully
	// sent the fundingLocked message, while Alice failed sending it. In
	// Alice's case this means that there should be no messages for Bob, and
	// the channel should still be in state 'markedOpen'
	select {
	case msg := <-alice.msgChan:
		t.Fatalf("did not expect any message from Alice: %v", msg)
	default:
		// Expected.
	}

	// Bob will send funding locked to Alice
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(1 * time.Second)

	// Alice should still be markedOpen
	state, _, err := alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != markedOpen {
		t.Fatalf("expected state to be markedOpen, was %v", state)
	}

	// While Bob successfully sent fundingLocked.
	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}

	// Alice should be waiting for the server to notify when Bob somes back online.
	var peer *btcec.PublicKey
	var con chan<- struct{}
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

	if !peer.IsEqual(bobPubKey) {
		t.Fatalf("expected to receive Bob's pubkey (%v), instead got %v",
			bobPubKey, peer)
	}

	// Fix Alice's SendToPeer, and notify that Bob is back online.
	alice.fundingMgr.cfg.SendToPeer = func(target *btcec.PublicKey,
		msgs ...lnwire.Message) error {
		select {
		case alice.msgChan <- msgs[0]:
		case <-alice.shutdownChannel:
			return fmt.Errorf("shutting down")
		}
		return nil
	}
	close(con)

	// This should make Alice send the fundingLocked.
	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// Sleep to make sure database write is finished.
	time.Sleep(500 * time.Millisecond)

	// The state should now be fundingLockedSent
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != nil {
		t.Fatalf("unable to get channel state: %v", err)
	}

	if state != fundingLockedSent {
		t.Fatalf("expected state to be fundingLockedSent, was %v", state)
	}

	// Make sure both fundingManagers send the expected channel announcements.
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	time.Sleep(300 * time.Millisecond)
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Exchange the fundingLocked messages.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)

}

func TestFundingManagerFundingTimeout(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// We will consume the channel updates as we go, so no buffering is needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	_ = openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// Bob will at this point be waiting for the funding transaction to be
	// confirmed, so the channel should be considered pending.
	pendingChannels, err := bob.fundingMgr.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to fetch pending channels: %v", err)
	}
	if len(pendingChannels) != 1 {
		t.Fatalf("Expected Bob to have 1 pending channel, had  %v",
			len(pendingChannels))
	}

	// We expect Bob to forget the channel after 288 blocks (48 hours), so
	// mine 287, and check that it is still pending.
	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight + 287,
	}

	time.Sleep(300 * time.Millisecond)

	// Bob should still be waiting for the channel to open.
	pendingChannels, err = bob.fundingMgr.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to fetch pending channels: %v", err)
	}
	if len(pendingChannels) != 1 {
		t.Fatalf("Expected Bob to have 1 pending channel, had  %v",
			len(pendingChannels))
	}

	bob.mockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: fundingBroadcastHeight + 288,
	}

	// It takes some time for Bob to update the database, so sleep for
	// some time.
	time.Sleep(300 * time.Millisecond)

	pendingChannels, err = bob.fundingMgr.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to fetch pending channels: %v", err)
	}
	if len(pendingChannels) != 0 {
		t.Fatalf("Expected Bob to have 0 pending channel, had  %v",
			len(pendingChannels))
	}
}

// TestFundingManagerReceiveFundingLockedTwice checks that the fundingManager
// continues to operate as expected in case we receive a duplicate fundingLocked
// message.
func TestFundingManagerReceiveFundingLockedTwice(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// We will consume the channel updates as we go, so no buffering is needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write
	//state to database.
	time.Sleep(300 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// fundingLocked to Bob.
	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// And similarly Bob will send funding locked to Alice.
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(300 * time.Millisecond)

	// Check that the state machine is updated accordingly
	assertFundingLockedSent(t, alice, bob, fundingOutPoint)

	// Make sure both fundingManagers send the expected channel announcements.
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	time.Sleep(300 * time.Millisecond)
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Send the fundingLocked message twice to Alice, and once to Bob.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)

	// Alice should not send the channel state the second time, as the
	// second funding locked should just be ignored.
	select {
	case <-alice.arbiterChan:
		t.Fatalf("alice sent channel to breach arbiter a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}
	select {
	case <-alice.peer.newChannels:
		t.Fatalf("alice sent new channel to peer a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}

	// Another fundingLocked should also be ignored, since Alice should
	// have updated her database at this point.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	select {
	case <-alice.arbiterChan:
		t.Fatalf("alice sent channel to breach arbiter a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}
	select {
	case <-alice.peer.newChannels:
		t.Fatalf("alice sent new channel to peer a second time")
	case <-time.After(time.Millisecond * 300):
		// Expected
	}

}

// TestFundingManagerRestartAfterChanAnn checks that the fundingManager properly
// handles receiving a fundingLocked after the its own fundingLocked and channel
// announcement is sent and gets restarted.
func TestFundingManagerRestartAfterChanAnn(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// We will consume the channel updates as we go, so no buffering is needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write
	//state to database.
	time.Sleep(300 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// fundingLocked to Bob.
	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// And similarly Bob will send funding locked to Alice.
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(300 * time.Millisecond)

	// Check that the state machine is updated accordingly
	assertFundingLockedSent(t, alice, bob, fundingOutPoint)

	// Make sure both fundingManagers send the expected channel announcements.
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished, wait for the
	// OpenStatusUpdate_ChanOpen update
	waitForOpenUpdate(t, updateChan)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	time.Sleep(300 * time.Millisecond)
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// At this point we restart Alice's fundingManager, before she receives
	// the fundingLocked message. After restart, she will receive it, and
	// we expect her to be able to handle it correctly.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)

	// Exchange the fundingLocked messages.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)
}

// TestFundingManagerRestartAfterReceivingFundingLocked checks that the
// fundingManager continues to operate as expected after it has received
// fundingLocked and then gets restarted.
func TestFundingManagerRestartAfterReceivingFundingLocked(t *testing.T) {
	disableFndgLogger(t)

	alice, bob := setupFundingManagers(t)
	defer tearDownFundingManagers(t, alice, bob)

	// We will consume the channel updates as we go, so no buffering is needed.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// Notify that transaction was mined
	alice.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}
	bob.mockNotifier.confChannel <- &chainntnfs.TxConfirmation{}

	// Give fundingManager time to process the newly mined tx and write
	//state to database.
	time.Sleep(300 * time.Millisecond)

	// The funding transaction was mined, so assert that both funding
	// managers now have the state of this channel 'markedOpen' in their
	// internal state machine.
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	// After the funding transaction is mined, Alice will send
	// fundingLocked to Bob.
	fundingLockedAlice := checkNodeSendingFundingLocked(t, alice)

	// And similarly Bob will send funding locked to Alice.
	fundingLockedBob := checkNodeSendingFundingLocked(t, bob)

	// Sleep to make sure database write is finished.
	time.Sleep(300 * time.Millisecond)

	// Check that the state machine is updated accordingly
	assertFundingLockedSent(t, alice, bob, fundingOutPoint)

	// Let Alice immediately get the fundingLocked message.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)
	time.Sleep(300 * time.Millisecond)

	// She will block waiting for local channel announcements to finish
	// before sending the new channel state to the peer.
	select {
	case <-alice.peer.newChannels:
		t.Fatalf("did not expect alice to handle the fundinglocked")
	case <-time.After(time.Millisecond * 300):
	}

	// At this point we restart Alice's fundingManager. Bob will resend
	// the fundingLocked after the connection is re-established.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)

	// Simulate Bob resending the message when Alice is back up.
	alice.fundingMgr.processFundingLocked(fundingLockedBob, bobAddr)

	// Make sure both fundingManagers send the expected channel announcements.
	assertChannelAnnouncements(t, alice, bob)

	// The funding process is now finished. Since we recreated the
	// fundingManager, we don't have an update channel to synchronize on,
	// so a small sleep makes sure the database writing is finished.
	time.Sleep(300 * time.Millisecond)

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	assertNoChannelState(t, alice, bob, fundingOutPoint)

	// Also let Bob get the fundingLocked message.
	bob.fundingMgr.processFundingLocked(fundingLockedAlice, aliceAddr)

	// Check that they notify the breach arbiter and peer about the new
	// channel.
	assertHandleFundingLocked(t, alice, bob)
}
