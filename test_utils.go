package main

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"net"
	"os"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	alicesPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	// Just use some arbitrary bytes as delivery script.
	dummyDeliveryScript = alicesPrivKey[:]
)

// createTestPeer creates a channel between two nodes, and returns a peer for
// one of the nodes, together with the channel seen from both nodes.
func createTestPeer(notifier chainntnfs.ChainNotifier,
	publTx chan *wire.MsgTx) (*peer, *lnwallet.LightningChannel,
	*lnwallet.LightningChannel, func(), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		alicesPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(1300)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	prevOut := &wire.OutPoint{
		Hash:  chainhash.Hash(testHdSeed),
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
		},
		CsvDelay:            uint16(csvTimeoutAlice),
		MultiSigKey:         aliceKeyPub,
		RevocationBasePoint: aliceKeyPub,
		PaymentBasePoint:    aliceKeyPub,
		DelayBasePoint:      aliceKeyPub,
		HtlcBasePoint:       aliceKeyPub,
	}
	bobCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        bobDustLimit,
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
		},
		CsvDelay:            uint16(csvTimeoutBob),
		MultiSigKey:         bobKeyPub,
		RevocationBasePoint: bobKeyPub,
		PaymentBasePoint:    bobKeyPub,
		DelayBasePoint:      bobKeyPub,
		HtlcBasePoint:       bobKeyPub,
	}

	bobRoot := lnwallet.DeriveRevocationRoot(bobKeyPriv, testHdSeed, aliceKeyPub)
	bobPreimageProducer := shachain.NewRevocationProducer(bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bobCommitPoint := lnwallet.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot := lnwallet.DeriveRevocationRoot(aliceKeyPriv, testHdSeed, bobKeyPub)
	alicePreimageProducer := shachain.NewRevocationProducer(aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	aliceCommitPoint := lnwallet.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(channelBal,
		channelBal, &aliceCfg, &bobCfg, aliceCommitPoint, bobCommitPoint,
		*fundingTxIn)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	estimator := &lnwallet.StaticFeeEstimator{FeeRate: 50}
	feePerWeight, err := estimator.EstimateFeePerWeight(1)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	feePerKw := feePerWeight * 1000

	// TODO(roasbeef): need to factor in commit fee?
	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      feePerKw,
		CommitFee:     8688,
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      feePerKw,
		CommitFee:     8688,
		CommitTx:      bobCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunder,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunder,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob,
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	if err := aliceChannelState.SyncPending(addr, 0); err != nil {
		return nil, nil, nil, nil, err
	}

	addr = &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}

	if err := bobChannelState.SyncPending(addr, 0); err != nil {
		return nil, nil, nil, nil, err
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	channelAlice, err := lnwallet.NewLightningChannel(aliceSigner, notifier,
		estimator, aliceChannelState)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	channelBob, err := lnwallet.NewLightningChannel(bobSigner, notifier,
		estimator, bobChannelState)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	chainIO := &mockChainIO{}
	wallet := &lnwallet.LightningWallet{
		WalletController: &mockWalletController{
			rootKey:               aliceKeyPriv,
			publishedTransactions: publTx,
		},
	}
	cc := &chainControl{
		feeEstimator:  estimator,
		chainIO:       chainIO,
		chainNotifier: notifier,
		wallet:        wallet,
	}

	breachArbiter := &breachArbiter{
		settledContracts: make(chan *wire.OutPoint, 10),
	}

	s := &server{
		chanDB:        dbAlice,
		cc:            cc,
		breachArbiter: breachArbiter,
	}
	s.htlcSwitch = htlcswitch.New(htlcswitch.Config{})
	s.htlcSwitch.Start()

	alicePeer := &peer{
		server:        s,
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		activeChannels: make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		newChannels:    make(chan *newChannelMsg, 1),

		activeChanCloses:   make(map[lnwire.ChannelID]*channelCloser),
		localCloseChanReqs: make(chan *htlcswitch.ChanClose),
		chanCloseMsgs:      make(chan *closeMsg),

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
	}

	chanID := lnwire.NewChanIDFromOutPoint(channelAlice.ChannelPoint())
	alicePeer.activeChannels[chanID] = channelAlice

	go alicePeer.channelManager()

	return alicePeer, channelAlice, channelBob, cleanUpFunc, nil
}
