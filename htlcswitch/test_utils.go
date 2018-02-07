package htlcswitch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"io/ioutil"
	"os"

	"io"

	"math/big"

	"net"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	alicePrivKey = []byte("alice priv key")
	bobPrivKey   = []byte("bob priv key")
	carolPrivKey = []byte("carol priv key")

	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	_, testPubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)
	testSig       = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	wireSig, _ = lnwire.NewSigFromSignature(testSig)

	_, _ = testSig.R.SetString("6372440660162918006277497454296753625158993"+
		"5445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("1880105606924982582529128710493133386286603"+
		"3135609736119018462340006816851118", 10)
)

// mockGetChanUpdateMessage helper function which returns topology update of
// the channel
func mockGetChanUpdateMessage() (*lnwire.ChannelUpdate, error) {
	return &lnwire.ChannelUpdate{
		Signature: wireSig,
	}, nil
}

// generateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)

	// TODO(roasbeef): should use counter in tests (atomic) rather than
	// this

	_, err := rand.Read(b[:])
	// Note that Err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

// createTestChannel creates the channel and returns our and remote channels
// representations.
//
// TODO(roasbeef): need to factor out, similar func re-used in many parts of codebase
func createTestChannel(alicePrivKey, bobPrivKey []byte,
	aliceAmount, bobAmount btcutil.Amount,
	chanID lnwire.ShortChannelID) (*lnwallet.LightningChannel, *lnwallet.LightningChannel, func(),
	func() (*lnwallet.LightningChannel, *lnwallet.LightningChannel,
		error), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), alicePrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), bobPrivKey)

	channelCapacity := aliceAmount + bobAmount
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(800)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	var hash [sha256.Size]byte
	randomSeed, err := generateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	copy(hash[:], randomSeed)

	prevOut := &wire.OutPoint{
		Hash:  chainhash.Hash(hash),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	aliceCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit: aliceDustLimit,
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
			DustLimit: bobDustLimit,
		},
		CsvDelay:            uint16(csvTimeoutBob),
		MultiSigKey:         bobKeyPub,
		RevocationBasePoint: bobKeyPub,
		PaymentBasePoint:    bobKeyPub,
		DelayBasePoint:      bobKeyPub,
		HtlcBasePoint:       bobKeyPub,
	}

	bobRoot := lnwallet.DeriveRevocationRoot(bobKeyPriv, hash, aliceKeyPub)
	bobPreimageProducer := shachain.NewRevocationProducer(bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bobCommitPoint := lnwallet.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot := lnwallet.DeriveRevocationRoot(aliceKeyPriv, hash, bobKeyPub)
	alicePreimageProducer := shachain.NewRevocationProducer(aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	aliceCommitPoint := lnwallet.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(aliceAmount,
		bobAmount, &aliceCfg, &bobCfg, aliceCommitPoint, bobCommitPoint,
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

	estimator := &lnwallet.StaticFeeEstimator{
		FeeRate: 24,
	}
	feePerWeight, err := estimator.EstimateFeePerWeight(1)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	feePerKw := btcutil.Amount(feePerWeight * 1000)
	commitFee := (feePerKw * btcutil.Amount(724)) / 1000

	const broadcastHeight = 1
	bobAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	aliceAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}

	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(aliceAmount - commitFee),
		RemoteBalance: lnwire.NewMSatFromSatoshis(bobAmount),
		CommitFee:     commitFee,
		FeePerKw:      feePerKw,
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(bobAmount),
		RemoteBalance: lnwire.NewMSatFromSatoshis(aliceAmount - commitFee),
		CommitFee:     commitFee,
		FeePerKw:      feePerKw,
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
		ShortChanID:             chanID,
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
		ShortChanID:             chanID,
		Db:                      dbBob,
	}

	if err := aliceChannelState.SyncPending(bobAddr, broadcastHeight); err != nil {
		return nil, nil, nil, nil, err
	}

	if err := bobChannelState.SyncPending(aliceAddr, broadcastHeight); err != nil {
		return nil, nil, nil, nil, err
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	pCache := &mockPreimageCache{
		// hash -> preimage
		preimageMap: make(map[[32]byte][]byte),
	}

	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, pCache, aliceChannelState,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, pCache, bobChannelState,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	aliceNextRevoke, err := channelAlice.NextRevocationKey()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := channelBob.InitNextRevocation(aliceNextRevoke); err != nil {
		return nil, nil, nil, nil, err
	}

	bobNextRevoke, err := channelBob.NextRevocationKey()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := channelAlice.InitNextRevocation(bobNextRevoke); err != nil {
		return nil, nil, nil, nil, err
	}

	restore := func() (*lnwallet.LightningChannel, *lnwallet.LightningChannel,
		error) {
		aliceStoredChannels, err := dbAlice.FetchOpenChannels(aliceKeyPub)
		if err != nil {
			return nil, nil, errors.Errorf("unable to fetch alice channel: "+
				"%v", err)
		}

		var aliceStoredChannel *channeldb.OpenChannel
		for _, channel := range aliceStoredChannels {
			if channel.FundingOutpoint.String() == prevOut.String() {
				aliceStoredChannel = channel
				break
			}
		}

		if aliceStoredChannel == nil {
			return nil, nil, errors.New("unable to find stored alice channel")
		}

		newAliceChannel, err := lnwallet.NewLightningChannel(aliceSigner,
			nil, aliceStoredChannel)
		if err != nil {
			return nil, nil, errors.Errorf("unable to create new channel: %v",
				err)
		}

		bobStoredChannels, err := dbBob.FetchOpenChannels(bobKeyPub)
		if err != nil {
			return nil, nil, errors.Errorf("unable to fetch bob channel: "+
				"%v", err)
		}

		var bobStoredChannel *channeldb.OpenChannel
		for _, channel := range bobStoredChannels {
			if channel.FundingOutpoint.String() == prevOut.String() {
				bobStoredChannel = channel
				break
			}
		}

		if bobStoredChannel == nil {
			return nil, nil, errors.New("unable to find stored bob channel")
		}

		newBobChannel, err := lnwallet.NewLightningChannel(bobSigner,
			nil, bobStoredChannel)
		if err != nil {
			return nil, nil, errors.Errorf("unable to create new channel: %v",
				err)
		}
		return newAliceChannel, newBobChannel, nil
	}

	return channelAlice, channelBob, cleanUpFunc, restore, nil
}

// getChanID retrieves the channel point from nwire message.
func getChanID(msg lnwire.Message) (lnwire.ChannelID, error) {
	var chanID lnwire.ChannelID
	switch msg := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		chanID = msg.ChanID
	case *lnwire.UpdateFulfillHTLC:
		chanID = msg.ChanID
	case *lnwire.UpdateFailHTLC:
		chanID = msg.ChanID
	case *lnwire.RevokeAndAck:
		chanID = msg.ChanID
	case *lnwire.CommitSig:
		chanID = msg.ChanID
	case *lnwire.ChannelReestablish:
		chanID = msg.ChanID
	case *lnwire.FundingLocked:
		chanID = msg.ChanID
	case *lnwire.UpdateFee:
		chanID = msg.ChanID
	default:
		return chanID, fmt.Errorf("unknown type: %T", msg)
	}

	return chanID, nil
}

// generatePayment generates the htlc add request by given path blob and
// invoice which should be added by destination peer.
func generatePayment(invoiceAmt, htlcAmt lnwire.MilliSatoshi, timelock uint32,
	blob [lnwire.OnionPacketSize]byte) (*channeldb.Invoice, *lnwire.UpdateAddHTLC, error) {

	var preimage [sha256.Size]byte
	r, err := generateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, err
	}
	copy(preimage[:], r)

	rhash := fastsha256.Sum256(preimage[:])

	invoice := &channeldb.Invoice{
		CreationDate: time.Now(),
		Terms: channeldb.ContractTerm{
			Value:           invoiceAmt,
			PaymentPreimage: preimage,
		},
	}

	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      htlcAmt,
		Expiry:      timelock,
		OnionBlob:   blob,
	}

	return invoice, htlc, nil
}

// generateRoute generates the path blob by given array of peers.
func generateRoute(hops ...ForwardingInfo) ([lnwire.OnionPacketSize]byte, error) {
	var blob [lnwire.OnionPacketSize]byte
	if len(hops) == 0 {
		return blob, errors.New("empty path")
	}

	iterator := newMockHopIterator(hops...)

	w := bytes.NewBuffer(blob[0:0])
	if err := iterator.EncodeNextHop(w); err != nil {
		return blob, err
	}

	return blob, nil

}

// threeHopNetwork is used for managing the created cluster of 3 hops.
type threeHopNetwork struct {
	aliceServer      *mockServer
	aliceChannelLink *channelLink
	aliceBlockEpoch  chan *chainntnfs.BlockEpoch
	aliceTicker      *time.Ticker

	firstBobChannelLink *channelLink
	bobFirstBlockEpoch  chan *chainntnfs.BlockEpoch
	firstBobTicker      *time.Ticker

	bobServer            *mockServer
	secondBobChannelLink *channelLink
	bobSecondBlockEpoch  chan *chainntnfs.BlockEpoch
	secondBobTicker      *time.Ticker

	carolChannelLink *channelLink
	carolServer      *mockServer
	carolBlockEpoch  chan *chainntnfs.BlockEpoch
	carolTicker      *time.Ticker

	feeEstimator *mockFeeEstimator

	globalPolicy ForwardingPolicy
}

// generateHops creates the per hop payload, the total amount to be sent, and
// also the time lock value needed to route a HTLC with the target amount over
// the specified path.
func generateHops(payAmt lnwire.MilliSatoshi, startingHeight uint32,
	path ...*channelLink) (lnwire.MilliSatoshi, uint32, []ForwardingInfo) {

	lastHop := path[len(path)-1]

	totalTimelock := startingHeight
	runningAmt := payAmt

	hops := make([]ForwardingInfo, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		// If this is the last hop, then the next hop is the special
		// "exit node". Otherwise, we look to the "prior" hop.
		nextHop := exitHop
		if i != len(path)-1 {
			nextHop = path[i+1].channel.ShortChanID()
		}

		// If this is the last, hop, then the time lock will be their
		// specified delta policy plus our starting height.
		totalTimelock += lastHop.cfg.FwrdingPolicy.TimeLockDelta
		timeLock := totalTimelock

		// Otherwise, the outgoing time lock should be the incoming
		// timelock minus their specified delta.
		if i != len(path)-1 {
			delta := path[i].cfg.FwrdingPolicy.TimeLockDelta
			timeLock = totalTimelock - delta
		}

		// Finally, we'll need to calculate the amount to forward. For
		// the last hop, it's just the payment amount.
		amount := payAmt
		if i != len(path)-1 {
			prevHop := hops[i+1]
			prevAmount := prevHop.AmountToForward

			fee := ExpectedFee(path[i].cfg.FwrdingPolicy, prevAmount)
			runningAmt += fee

			// Otherwise, for a node to forward an HTLC, then
			// following inequality most hold true:
			//     * amt_in - fee >= amt_to_forward
			amount = runningAmt - fee
		}

		hops[i] = ForwardingInfo{
			Network:         BitcoinHop,
			NextHop:         nextHop,
			AmountToForward: amount,
			OutgoingCTLV:    timeLock,
		}
	}

	return runningAmt, totalTimelock, hops
}

type paymentResponse struct {
	rhash chainhash.Hash
	err   chan error
}

func (r *paymentResponse) Wait(d time.Duration) (chainhash.Hash, error) {
	select {
	case err := <-r.err:
		close(r.err)
		return r.rhash, err
	case <-time.After(d):
		return r.rhash, errors.New("htlc was no settled in time")
	}
}

// makePayment takes the destination node and amount as input, sends the
// payment and returns the error channel to wait for error to be received and
// invoice in order to check its status after the payment finished.
//
// With this function you can send payments:
// * from Alice to Bob
// * from Alice to Carol through the Bob
// * from Alice to some another peer through the Bob
func (n *threeHopNetwork) makePayment(sendingPeer, receivingPeer Peer,
	firstHopPub [33]byte, hops []ForwardingInfo,
	invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32) *paymentResponse {

	paymentErr := make(chan error, 1)

	var rhash chainhash.Hash

	sender := sendingPeer.(*mockServer)
	receiver := receivingPeer.(*mockServer)

	// Generate route convert it to blob, and return next destination for
	// htlc add request.
	blob, err := generateRoute(hops...)
	if err != nil {
		paymentErr <- err
		return &paymentResponse{
			rhash: rhash,
			err:   paymentErr,
		}
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, err := generatePayment(invoiceAmt, htlcAmt, timelock, blob)
	if err != nil {
		paymentErr <- err
		return &paymentResponse{
			rhash: rhash,
			err:   paymentErr,
		}
	}
	rhash = fastsha256.Sum256(invoice.Terms.PaymentPreimage[:])

	// Check who is last in the route and add invoice to server registry.
	if err := receiver.registry.AddInvoice(*invoice); err != nil {
		paymentErr <- err
		return &paymentResponse{
			rhash: rhash,
			err:   paymentErr,
		}
	}

	// Send payment and expose err channel.
	go func() {
		_, err := sender.htlcSwitch.SendHTLC(firstHopPub, htlc,
			newMockDeobfuscator())
		paymentErr <- err
	}()

	return &paymentResponse{
		rhash: rhash,
		err:   paymentErr,
	}
}

// start starts the three hop network alice,bob,carol servers.
func (n *threeHopNetwork) start() error {
	if err := n.aliceServer.Start(); err != nil {
		return err
	}
	if err := n.bobServer.Start(); err != nil {
		return err
	}
	if err := n.carolServer.Start(); err != nil {
		return err
	}

	return nil
}

// stop stops nodes and cleanup its databases.
func (n *threeHopNetwork) stop() {
	done := make(chan struct{})
	go func() {
		n.aliceServer.Stop()
		done <- struct{}{}
	}()

	go func() {
		n.bobServer.Stop()
		done <- struct{}{}
	}()

	go func() {
		n.carolServer.Stop()
		done <- struct{}{}
	}()

	n.aliceTicker.Stop()
	n.firstBobTicker.Stop()
	n.secondBobTicker.Stop()
	n.carolTicker.Stop()

	for i := 0; i < 3; i++ {
		<-done
	}
}

type clusterChannels struct {
	aliceToBob *lnwallet.LightningChannel
	bobToAlice *lnwallet.LightningChannel
	bobToCarol *lnwallet.LightningChannel
	carolToBob *lnwallet.LightningChannel
}

// createClusterChannels creates lightning channels which are needed for
// network cluster to be initialized.
func createClusterChannels(aliceToBob, bobToCarol btcutil.Amount) (
	*clusterChannels, func(), func() (*clusterChannels, error), error) {

	firstChanID := lnwire.NewShortChanIDFromInt(4)
	secondChanID := lnwire.NewShortChanIDFromInt(5)

	// Create lightning channels between Alice<->Bob and Bob<->Carol
	aliceChannel, firstBobChannel, cleanAliceBob, restoreAliceBob, err := createTestChannel(
		alicePrivKey, bobPrivKey, aliceToBob, aliceToBob, firstChanID)
	if err != nil {
		return nil, nil, nil, errors.Errorf("unable to create "+
			"alice<->bob channel: %v", err)
	}

	secondBobChannel, carolChannel, cleanBobCarol, restoreBobCarol, err := createTestChannel(
		bobPrivKey, carolPrivKey, bobToCarol, bobToCarol, secondChanID)
	if err != nil {
		cleanAliceBob()
		return nil, nil, nil, errors.Errorf("unable to create "+
			"bob<->carol channel: %v", err)
	}

	cleanUp := func() {
		cleanAliceBob()
		cleanBobCarol()
	}

	restoreFromDb := func() (*clusterChannels, error) {
		a2b, b2a, err := restoreAliceBob()
		if err != nil {
			return nil, err
		}

		b2c, c2b, err := restoreBobCarol()
		if err != nil {
			return nil, err
		}

		return &clusterChannels{
			aliceToBob: a2b,
			bobToAlice: b2a,
			bobToCarol: b2c,
			carolToBob: c2b,
		}, nil
	}

	return &clusterChannels{
		aliceToBob: aliceChannel,
		bobToAlice: firstBobChannel,
		bobToCarol: secondBobChannel,
		carolToBob: carolChannel,
	}, cleanUp, restoreFromDb, nil
}

// newThreeHopNetwork function creates the following topology and returns the
// control object to manage this cluster:
//
//	alice			   bob				   carol
//	server - <-connection-> - server - - <-connection-> - - - server
//	 |		   	  |				   |
//   alice htlc			bob htlc		    carol htlc
//     switch			switch	\		    switch
//	|			 |       \			|
//	|			 |        \			|
// alice                   first bob    second bob              carol
// channel link	    	  channel link   channel link		channel link
//
func newThreeHopNetwork(t testing.TB, aliceChannel, firstBobChannel,
	secondBobChannel, carolChannel *lnwallet.LightningChannel,
	startingHeight uint32) *threeHopNetwork {

	// Create three peers/servers.
	aliceServer := newMockServer(t, "alice")
	bobServer := newMockServer(t, "bob")
	carolServer := newMockServer(t, "carol")

	// Create mock decoder instead of sphinx one in order to mock the route
	// which htlc should follow.
	decoder := &mockIteratorDecoder{}

	feeEstimator := &mockFeeEstimator{
		byteFeeIn:   make(chan btcutil.Amount),
		weightFeeIn: make(chan btcutil.Amount),
		quit:        make(chan struct{}),
	}

	pCache := &mockPreimageCache{
		// hash -> preimage
		preimageMap: make(map[[32]byte][]byte),
	}

	globalPolicy := ForwardingPolicy{
		MinHTLC:       lnwire.NewMSatFromSatoshis(5),
		BaseFee:       lnwire.NewMSatFromSatoshis(1),
		TimeLockDelta: 6,
	}
	obfuscator := newMockObfuscator()

	aliceEpochChan := make(chan *chainntnfs.BlockEpoch)
	aliceEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: aliceEpochChan,
		Cancel: func() {
		},
	}
	aliceTicker := time.NewTicker(50 * time.Millisecond)
	aliceChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              bobServer,
			Switch:            aliceServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             aliceServer.registry,
			BlockEpochs:          aliceEpoch,
			FeeEstimator:         feeEstimator,
			PreimageCache:        pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents: &contractcourt.ChainEventSubscription{},
			SyncStates:  true,
			BatchTicker: &mockTicker{aliceTicker.C},
			BatchSize:   10,
		},
		aliceChannel,
		startingHeight,
	)
	if err := aliceServer.htlcSwitch.addLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice channel link: %v", err)
	}
	go func() {
		for {
			select {
			case <-aliceChannelLink.(*channelLink).htlcUpdates:
			case <-aliceChannelLink.(*channelLink).quit:
				return
			}
		}
	}()

	bobFirstEpochChan := make(chan *chainntnfs.BlockEpoch)
	bobFirstEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: bobFirstEpochChan,
		Cancel: func() {
		},
	}
	firstBobTicker := time.NewTicker(50 * time.Millisecond)
	firstBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              aliceServer,
			Switch:            bobServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             bobServer.registry,
			BlockEpochs:          bobFirstEpoch,
			FeeEstimator:         feeEstimator,
			PreimageCache:        pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents: &contractcourt.ChainEventSubscription{},
			SyncStates:  true,
			BatchTicker: &mockTicker{firstBobTicker.C},
			BatchSize:   10,
		},
		firstBobChannel,
		startingHeight,
	)
	if err := bobServer.htlcSwitch.addLink(firstBobChannelLink); err != nil {
		t.Fatalf("unable to add first bob channel link: %v", err)
	}
	go func() {
		for {
			select {
			case <-firstBobChannelLink.(*channelLink).htlcUpdates:
			case <-firstBobChannelLink.(*channelLink).quit:
				return
			}
		}
	}()

	bobSecondEpochChan := make(chan *chainntnfs.BlockEpoch)
	bobSecondEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: bobSecondEpochChan,
		Cancel: func() {
		},
	}
	secondBobTicker := time.NewTicker(50 * time.Millisecond)
	secondBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              carolServer,
			Switch:            bobServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             bobServer.registry,
			BlockEpochs:          bobSecondEpoch,
			FeeEstimator:         feeEstimator,
			PreimageCache:        pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents: &contractcourt.ChainEventSubscription{},
			SyncStates:  true,
			BatchTicker: &mockTicker{secondBobTicker.C},
			BatchSize:   10,
		},
		secondBobChannel,
		startingHeight,
	)
	if err := bobServer.htlcSwitch.addLink(secondBobChannelLink); err != nil {
		t.Fatalf("unable to add second bob channel link: %v", err)
	}
	go func() {
		for {
			select {
			case <-secondBobChannelLink.(*channelLink).htlcUpdates:
			case <-secondBobChannelLink.(*channelLink).quit:
				return
			}
		}
	}()

	carolBlockEpoch := make(chan *chainntnfs.BlockEpoch)
	carolEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: bobSecondEpochChan,
		Cancel: func() {
		},
	}
	carolTicker := time.NewTicker(50 * time.Millisecond)
	carolChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              bobServer,
			Switch:            carolServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             carolServer.registry,
			BlockEpochs:          carolEpoch,
			FeeEstimator:         feeEstimator,
			PreimageCache:        pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents: &contractcourt.ChainEventSubscription{},
			SyncStates:  true,
			BatchTicker: &mockTicker{carolTicker.C},
			BatchSize:   10,
		},
		carolChannel,
		startingHeight,
	)
	if err := carolServer.htlcSwitch.addLink(carolChannelLink); err != nil {
		t.Fatalf("unable to add carol channel link: %v", err)
	}
	go func() {
		for {
			select {
			case <-carolChannelLink.(*channelLink).htlcUpdates:
			case <-carolChannelLink.(*channelLink).quit:
				return
			}
		}
	}()

	return &threeHopNetwork{
		aliceServer:      aliceServer,
		aliceChannelLink: aliceChannelLink.(*channelLink),
		aliceBlockEpoch:  aliceEpochChan,
		aliceTicker:      aliceTicker,

		firstBobChannelLink: firstBobChannelLink.(*channelLink),
		bobFirstBlockEpoch:  bobFirstEpochChan,
		firstBobTicker:      firstBobTicker,

		bobServer:            bobServer,
		secondBobChannelLink: secondBobChannelLink.(*channelLink),
		bobSecondBlockEpoch:  bobSecondEpochChan,
		secondBobTicker:      secondBobTicker,

		carolChannelLink: carolChannelLink.(*channelLink),
		carolServer:      carolServer,
		carolBlockEpoch:  carolBlockEpoch,
		carolTicker:      carolTicker,

		feeEstimator: feeEstimator,
		globalPolicy: globalPolicy,
	}
}
