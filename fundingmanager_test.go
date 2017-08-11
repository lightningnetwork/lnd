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
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// The block height returned by the mock BlockChainIO's GetBestBlock.
const fundingBroadcastHeight = 123

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

// mockWalletController is used by the LightningWallet, and let us mock the
// interaction with the bitcoin network.
type mockWalletController struct {
	rootKey               *btcec.PrivateKey
	prevAddres            btcutil.Address
	publishedTransactions chan *wire.MsgTx
}

// FetchInputInfo will be called to get info about the inputs to the funding
// transaction.
func (*mockWalletController) FetchInputInfo(
	prevOut *wire.OutPoint) (*wire.TxOut, error) {
	txOut := &wire.TxOut{
		Value:    int64(10 * btcutil.SatoshiPerBitcoin),
		PkScript: []byte("dummy"),
	}
	return txOut, nil
}
func (*mockWalletController) ConfirmedBalance(confs int32,
	witness bool) (btcutil.Amount, error) {
	return 0, nil
}

// NewAddress is called to get new addresses for delivery, change etc.
func (m *mockWalletController) NewAddress(addrType lnwallet.AddressType,
	change bool) (btcutil.Address, error) {
	addr, _ := btcutil.NewAddressPubKey(
		m.rootKey.PubKey().SerializeCompressed(), &chaincfg.MainNetParams)
	return addr, nil
}
func (*mockWalletController) GetPrivKey(a btcutil.Address) (*btcec.PrivateKey, error) {
	return nil, nil
}

// NewRawKey will be called to get keys to be used for the funding tx and the
// commitment tx.
func (m *mockWalletController) NewRawKey() (*btcec.PublicKey, error) {
	return m.rootKey.PubKey(), nil
}

// FetchRootKey will be called to provide the wallet with a root key.
func (m *mockWalletController) FetchRootKey() (*btcec.PrivateKey, error) {
	return m.rootKey, nil
}
func (*mockWalletController) SendOutputs(outputs []*wire.TxOut) (*chainhash.Hash, error) {
	return nil, nil
}

// ListUnspentWitness is called by the wallet when doing coin selection. We just
// need one unspent for the funding transaction.
func (*mockWalletController) ListUnspentWitness(confirms int32) ([]*lnwallet.Utxo, error) {
	utxo := &lnwallet.Utxo{
		Value: btcutil.Amount(10 * btcutil.SatoshiPerBitcoin),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	}
	var ret []*lnwallet.Utxo
	ret = append(ret, utxo)
	return ret, nil
}
func (*mockWalletController) ListTransactionDetails() ([]*lnwallet.TransactionDetail, error) {
	return nil, nil
}
func (*mockWalletController) LockOutpoint(o wire.OutPoint)   {}
func (*mockWalletController) UnlockOutpoint(o wire.OutPoint) {}
func (m *mockWalletController) PublishTransaction(tx *wire.MsgTx) error {
	m.publishedTransactions <- tx
	return nil
}
func (*mockWalletController) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	return nil, nil
}
func (*mockWalletController) IsSynced() (bool, error) {
	return true, nil
}
func (*mockWalletController) Start() error {
	return nil
}
func (*mockWalletController) Stop() error {
	return nil
}

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, txscript.SigHashAll,
		privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {
	witnessScript, err := txscript.WitnessScript(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value,
		signDesc.Output.PkScript, txscript.SigHashAll, m.key, true)
	if err != nil {
		return nil, err
	}

	return &lnwallet.InputScript{
		Witness: witnessScript,
	}, nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return activeNetParams.GenesisHash, fundingBroadcastHeight, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint,
	heightHint uint32) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

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
	privKey      *btcec.PrivateKey
	msgChan      chan lnwire.Message
	announceChan chan lnwire.Message
	publTxChan   chan *wire.MsgTx
	fundingMgr   *fundingManager
	mockNotifier *mockNotifier
	testDir      string
}

func disableLogger(t *testing.T) {
	channeldb.UseLogger(btclog.Disabled)
	lnwallet.UseLogger(btclog.Disabled)
	fndgLog = btclog.Disabled
}

func createTestWallet(tempTestDir string, netParams *chaincfg.Params,
	notifier chainntnfs.ChainNotifier, wc lnwallet.WalletController,
	signer lnwallet.Signer, bio lnwallet.BlockChainIO,
	estimator lnwallet.FeeEstimator) (*lnwallet.LightningWallet, error) {

	dbDir := filepath.Join(tempTestDir, "cdb")
	cdb, err := channeldb.Open(dbDir)
	if err != nil {
		return nil, err
	}

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

func createTestFundingManager(t *testing.T, pubKey *btcec.PublicKey,
	tempTestDir string, hdSeed []byte, netParams *chaincfg.Params,
	chainNotifier chainntnfs.ChainNotifier, estimator lnwallet.FeeEstimator,
	sentMessages chan lnwire.Message, sentAnnouncements chan lnwire.Message,
	publTxChan chan *wire.MsgTx, shutdownChan chan struct{}) (*fundingManager, error) {

	wc := &mockWalletController{
		rootKey:               alicePrivKey,
		publishedTransactions: publTxChan,
	}
	signer := &mockSigner{
		key: alicePrivKey,
	}
	bio := &mockChainIO{}

	lnw, err := createTestWallet(tempTestDir, netParams,
		chainNotifier, wc, signer, bio, estimator)
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}

	arbiterChan := make(chan *lnwallet.LightningChannel)
	var chanIDSeed [32]byte

	f, err := newFundingManager(fundingConfig{
		IDKey:        pubKey,
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
		FindPeer: func(peerKey *btcec.PublicKey) (*peer, error) {
			return nil, nil
		},
		TempChanIDSeed: chanIDSeed,
		FindChannel: func(chanID lnwire.ChannelID) (*lnwallet.LightningChannel, error) {
			// This is not expected to be used in the current tests.
			// Add an implementation if that changes.
			t.Fatal("did not expect FindChannel to be called")
			return nil, nil
		},
		NumRequiredConfs: func(chanAmt btcutil.Amount, pushAmt btcutil.Amount) uint16 {
			return uint16(cfg.DefaultNumChanConfs)
		},
		RequiredRemoteDelay: func(amt btcutil.Amount) uint16 {
			return 4
		},
	})
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	return f, nil
}

func recreateAliceFundingManager(t *testing.T, alice *testNode) {
	// Stop the old fundingManager before creating a new one.
	if err := alice.fundingMgr.Stop(); err != nil {
		t.Fatalf("unable to stop old fundingManager: %v", err)
	}

	aliceMsgChan := make(chan lnwire.Message)
	aliceAnnounceChan := make(chan lnwire.Message)

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
			aliceAnnounceChan <- msg
			return nil
		},
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement, error) {
			return lnwire.NodeAnnouncement{}, nil
		},
		ArbiterChan: oldCfg.ArbiterChan,
		SendToPeer: func(target *btcec.PublicKey,
			msgs ...lnwire.Message) error {
			aliceMsgChan <- msgs[0]
			return nil
		},
		FindPeer: func(peerKey *btcec.PublicKey) (*peer, error) {
			return nil, nil
		},
		TempChanIDSeed: oldCfg.TempChanIDSeed,
		FindChannel:    oldCfg.FindChannel,
	})
	if err != nil {
		t.Fatalf("failed recreating aliceFundingManager: %v", err)
	}

	alice.fundingMgr = f
	alice.msgChan = aliceMsgChan
	alice.announceChan = aliceAnnounceChan

	if err = f.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}
}

func setupFundingManagers(t *testing.T, shutdownChannel chan struct{}) (*testNode, *testNode) {
	// We need to set the global config, as fundingManager uses
	// MaxPendingChannels, and it is usually set in lndMain().
	cfg = &config{
		MaxPendingChannels: defaultMaxPendingChannels,
	}

	netParams := activeNetParams.Params
	estimator := lnwallet.StaticFeeEstimator{FeeRate: 250}

	aliceMockNotifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation, 1),
		epochChan:   make(chan *chainntnfs.BlockEpoch, 1),
	}

	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	aliceMsgChan := make(chan lnwire.Message)
	aliceAnnounceChan := make(chan lnwire.Message)
	alicePublTxChan := make(chan *wire.MsgTx, 1)

	aliceFundingMgr, err := createTestFundingManager(t, alicePubKey,
		aliceTestDir, alicePrivKeyBytes[:], netParams, aliceMockNotifier,
		estimator, aliceMsgChan, aliceAnnounceChan, alicePublTxChan,
		shutdownChannel)
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	if err = aliceFundingMgr.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}

	alice := &testNode{
		privKey:      alicePrivKey,
		msgChan:      aliceMsgChan,
		announceChan: aliceAnnounceChan,
		publTxChan:   alicePublTxChan,
		fundingMgr:   aliceFundingMgr,
		mockNotifier: aliceMockNotifier,
		testDir:      aliceTestDir,
	}

	bobMockNotifier := &mockNotifier{
		confChannel: make(chan *chainntnfs.TxConfirmation, 1),
		epochChan:   make(chan *chainntnfs.BlockEpoch, 1),
	}

	bobTestDir, err := ioutil.TempDir("", "boblnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	bobMsgChan := make(chan lnwire.Message)
	bobAnnounceChan := make(chan lnwire.Message)
	bobPublTxChan := make(chan *wire.MsgTx, 1)
	bobFundingMgr, err := createTestFundingManager(t, bobPubKey, bobTestDir,
		bobPrivKeyBytes[:], netParams, bobMockNotifier, estimator,
		bobMsgChan, bobAnnounceChan, bobPublTxChan, shutdownChannel)
	if err != nil {
		t.Fatalf("failed creating fundingManager: %v", err)
	}

	if err = bobFundingMgr.Start(); err != nil {
		t.Fatalf("failed starting fundingManager: %v", err)
	}

	bob := &testNode{
		privKey:      bobPrivKey,
		msgChan:      bobMsgChan,
		announceChan: bobAnnounceChan,
		publTxChan:   bobPublTxChan,
		fundingMgr:   bobFundingMgr,
		mockNotifier: bobMockNotifier,
		testDir:      bobTestDir,
	}

	return alice, bob
}

func tearDownFundingManagers(t *testing.T, a, b *testNode, shutdownChannel chan struct{}) {
	close(shutdownChannel)

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
		pushAmt:         pushAmt,
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
				"from bob, instead got error: (%v) %v",
				errorMsg.Code, string(errorMsg.Data))
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
				"from bob, instead got error: (%v) %v",
				errorMsg.Code, string(errorMsg.Data))
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
				"from bob, instead got error: (%v) %v",
				errorMsg.Code, string(errorMsg.Data))
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
				"sent from bob, instead got error: (%v) %v",
				errorMsg.Code, string(errorMsg.Data))
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

func TestFundingManagerNormalWorkflow(t *testing.T) {
	disableLogger(t)

	shutdownChannel := make(chan struct{})

	alice, bob := setupFundingManagers(t, shutdownChannel)
	defer tearDownFundingManagers(t, alice, bob, shutdownChannel)

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

	// After the funding transaction is mined, Alice will send
	// fundingLocked to Bob.
	var fundingLockedAlice lnwire.Message
	select {
	case fundingLockedAlice = <-alice.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice did not send fundingLocked")
	}
	if fundingLockedAlice.MsgType() != lnwire.MsgFundingLocked {
		t.Fatalf("expected fundingLocked sent from Alice, "+
			"instead got %T", fundingLockedAlice)
	}

	// And similarly Bob will send funding locked to Alice.
	var fundingLockedBob lnwire.Message
	select {
	case fundingLockedBob = <-bob.msgChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("bob did not send fundingLocked")
	}

	if fundingLockedBob.MsgType() != lnwire.MsgFundingLocked {
		t.Fatalf("expected fundingLocked sent from Bob, "+
			"instead got %T", fundingLockedBob)
	}

	// Sleep to make sure database write is finished.
	time.Sleep(300 * time.Millisecond)

	// Check that the state machine is updated accordingly
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
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

	// The funding process is now finished, wait for the
	// OpenStatusUpdate_ChanOpen update
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

	// The internal state-machine should now have deleted the channelStates
	// from the database, as the channel is announced.
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

	// Need to give bob time to update database.
	time.Sleep(300 * time.Millisecond)

	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

}

func TestFundingManagerRestartBehavior(t *testing.T) {
	disableLogger(t)

	shutdownChannel := make(chan struct{})

	alice, bob := setupFundingManagers(t, shutdownChannel)
	defer tearDownFundingManagers(t, alice, bob, shutdownChannel)

	// Run through the process of opening the channel, up until the funding
	// transaction is broadcasted.
	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint := openChannel(t, alice, bob, 500000, 0, 1, updateChan)

	// After the funding transaction gets mined, both nodes will send the
	// fundingLocked message to the other peer. If the funding node fails
	// before this message has been successfully sent, it should retry
	// sending it on restart. We mimic this behavior by letting the
	// SendToPeer method return an error, as if the message was not
	// successfully sent. We then   the fundingManager and make sure
	// it continues the process as expected.
	alice.fundingMgr.cfg.SendToPeer = func(target *btcec.PublicKey,
		msgs ...lnwire.Message) error {
		return fmt.Errorf("intentional error in SendToPeer")
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

	// After the funding transaction was mined, Bob should have successfully
	// sent the fundingLocked message, while Alice failed sending it. In
	// Alice's case this means that there should be no messages for Bob, and
	//  the channel should still be in state 'markedOpen'

	select {
	case msg := <-alice.msgChan:
		t.Fatalf("did not expect any message from Alice: %v", msg)
	default:
		// Expected.
	}

	// Bob will send funding locked to Alice
	fundingLockedBob := <-bob.msgChan
	if fundingLockedBob.MsgType() != lnwire.MsgFundingLocked {
		t.Fatalf("expected fundingLocked sent from Bob, "+
			"instead got %T", fundingLockedBob)
	}

	// Sleep to make sure database write is finished.
	time.Sleep(1 * time.Second)

	// Alice should still be markedOpen
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
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

	fundingLockedAlice := <-alice.msgChan
	if fundingLockedAlice.MsgType() != lnwire.MsgFundingLocked {
		t.Fatalf("expected fundingLocked sent from Alice, "+
			"instead got %T", fundingLockedAlice)
	}

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

	// Bob, however, should send the announcements
	announcements := make([]lnwire.Message, 4)
	for i := 0; i < len(announcements); i++ {
		select {
		case announcements[i] = <-bob.announceChan:
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

	// Next up, we check that the Alice rebroadcasts the announcement
	// messages on restart.
	recreateAliceFundingManager(t, alice)
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < len(announcements); i++ {
		select {
		case announcements[i] = <-alice.announceChan:
		case <-time.After(time.Second * 5):
			t.Fatalf("alice did not send announcement %v", i)
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
		t.Fatalf("did not get ChannelAnnouncement from Alice after restart")
	}
	if !gotChannelUpdate {
		t.Fatalf("did not get ChannelUpdate from Alice after restart")
	}
	if !gotAnnounceSignatures {
		t.Fatalf("did not get AnnounceSignatures from Alice after restart")
	}
	if !gotNodeAnnouncement {
		t.Fatalf("did not get NodeAnnouncement from Alice after restart")
	}

	// The funding process is now finished. Since we recreated the
	// fundingManager, we don't have an update channel to synchronize on,
	// so a small sleep makes sure the database writing is finished.
	time.Sleep(300 * time.Millisecond)

	// The internal state-machine should now have deleted them from the
	// internal database, as the channel is announced.
	state, _, err = alice.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

	state, _, err = bob.fundingMgr.getChannelOpeningState(fundingOutPoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected to not find channel state, but got: %v", state)
	}

}

func TestFundingManagerFundingTimeout(t *testing.T) {
	disableLogger(t)

	shutdownChannel := make(chan struct{})

	alice, bob := setupFundingManagers(t, shutdownChannel)
	defer tearDownFundingManagers(t, alice, bob, shutdownChannel)

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
