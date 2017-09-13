package htlcswitch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"io/ioutil"
	"os"

	"io"

	"math/big"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
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

	_, _ = testSig.R.SetString("6372440660162918006277497454296753625158993"+
		"5445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("1880105606924982582529128710493133386286603"+
		"3135609736119018462340006816851118", 10)
)

// mockGetChanUpdateMessage helper function which returns topology update
// of the channel
func mockGetChanUpdateMessage() (*lnwire.ChannelUpdate, error) {
	return &lnwire.ChannelUpdate{
		Signature: testSig,
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
	chanID lnwire.ShortChannelID) (*lnwallet.LightningChannel, *lnwallet.LightningChannel, func(), error) {

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
		return nil, nil, nil, err
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
	}

	bobRoot := lnwallet.DeriveRevocationRoot(bobKeyPriv, hash, aliceKeyPub)
	bobPreimageProducer := shachain.NewRevocationProducer(bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitPoint := lnwallet.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot := lnwallet.DeriveRevocationRoot(aliceKeyPriv, hash, bobKeyPub)
	alicePreimageProducer := shachain.NewRevocationProducer(aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceCommitPoint := lnwallet.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(aliceAmount,
		bobAmount, &aliceCfg, &bobCfg, aliceCommitPoint, bobCommitPoint,
		fundingTxIn)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, err
	}

	var obsfucator [lnwallet.StateHintSize]byte
	copy(obsfucator[:], aliceFirstRevoke[:])

	estimator := &lnwallet.StaticFeeEstimator{
		FeeRate:      24,
		Confirmation: 6,
	}
	feePerKw := btcutil.Amount(estimator.EstimateFeePerWeight(1) * 1000)
	commitFee := (feePerKw * btcutil.Amount(724)) / 1000

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunder,
		FeePerKw:                feePerKw,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		LocalBalance:            lnwire.NewMSatFromSatoshis(aliceAmount - commitFee),
		RemoteBalance:           lnwire.NewMSatFromSatoshis(bobAmount),
		CommitTx:                *aliceCommitTx,
		CommitSig:               bytes.Repeat([]byte{1}, 71),
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		ShortChanID:             chanID,
		Db:                      dbAlice,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FeePerKw:                feePerKw,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunder,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		LocalBalance:            lnwire.NewMSatFromSatoshis(bobAmount),
		RemoteBalance:           lnwire.NewMSatFromSatoshis(aliceAmount - commitFee),
		CommitTx:                *bobCommitTx,
		CommitSig:               bytes.Repeat([]byte{1}, 71),
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		ShortChanID:             chanID,
		Db:                      dbBob,
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	channelAlice, err := lnwallet.NewLightningChannel(aliceSigner,
		nil, estimator, aliceChannelState)
	if err != nil {
		return nil, nil, nil, err
	}
	channelBob, err := lnwallet.NewLightningChannel(bobSigner, nil,
		estimator, bobChannelState)
	if err != nil {
		return nil, nil, nil, err
	}

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	aliceNextRevoke, err := channelAlice.NextRevocationKey()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := channelBob.InitNextRevocation(aliceNextRevoke); err != nil {
		return nil, nil, nil, err
	}

	bobNextRevoke, err := channelBob.NextRevocationKey()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := channelAlice.InitNextRevocation(bobNextRevoke); err != nil {
		return nil, nil, nil, err
	}

	return channelAlice, channelBob, cleanUpFunc, nil
}

// getChanID retrieves the channel point from nwire message.
func getChanID(msg lnwire.Message) lnwire.ChannelID {
	var point lnwire.ChannelID
	switch msg := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		point = msg.ChanID
	case *lnwire.UpdateFufillHTLC:
		point = msg.ChanID
	case *lnwire.UpdateFailHTLC:
		point = msg.ChanID
	case *lnwire.RevokeAndAck:
		point = msg.ChanID
	case *lnwire.CommitSig:
		point = msg.ChanID
	}

	return point
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

	firstBobChannelLink  *channelLink
	bobServer            *mockServer
	secondBobChannelLink *channelLink

	carolChannelLink *channelLink
	carolServer      *mockServer

	firstChannelCleanup  func()
	secondChannelCleanup func()

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

			// If the this the first hop, then we don't need to
			// apply any fee, otherwise, the amount to forward
			// needs to take into account the fees.
			if i == 0 {
				amount = prevAmount
			} else {
				amount = prevAmount + fee
			}
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
	timelock uint32) (*channeldb.Invoice, error) {

	sender := sendingPeer.(*mockServer)
	receiver := receivingPeer.(*mockServer)

	// Generate route convert it to blob, and return next destination for
	// htlc add request.
	blob, err := generateRoute(hops...)
	if err != nil {
		return nil, err
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, err := generatePayment(invoiceAmt, htlcAmt, timelock,
		blob)
	if err != nil {
		return nil, err
	}

	// Check who is last in the route and add invoice to server registry.
	if err := receiver.registry.AddInvoice(invoice); err != nil {
		return nil, err
	}

	// Send payment and expose err channel.
	errChan := make(chan error)
	go func() {
		_, err := sender.htlcSwitch.SendHTLC(firstHopPub, htlc,
			newMockDeobfuscator())
		errChan <- err
	}()

	select {
	case err := <-errChan:
		return invoice, err
	case <-time.After(5 * time.Minute):
		return invoice, errors.New("htlc was not settled in time")
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

	for i := 0; i < 3; i++ {
		<-done
	}

	n.firstChannelCleanup()
	n.secondChannelCleanup()
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
func newThreeHopNetwork(t *testing.T, aliceToBob,
	bobToCarol btcutil.Amount, startingHeight uint32) *threeHopNetwork {
	var err error

	// Create three peers/servers.
	aliceServer := newMockServer(t, "alice")
	bobServer := newMockServer(t, "bob")
	carolServer := newMockServer(t, "carol")

	// Create mock decoder instead of sphinx one in order to mock the
	// route which htlc should follow.
	decoder := &mockIteratorDecoder{}

	firstChanID := lnwire.NewShortChanIDFromInt(4)
	secondChanID := lnwire.NewShortChanIDFromInt(5)

	// Create lightning channels between Alice<->Bob and Bob<->Carol
	aliceChannel, firstBobChannel, fCleanUp, err := createTestChannel(
		alicePrivKey, bobPrivKey, aliceToBob, aliceToBob, firstChanID)
	if err != nil {
		t.Fatalf("unable to create alice<->bob channel: %v", err)
	}

	secondBobChannel, carolChannel, sCleanUp, err := createTestChannel(
		bobPrivKey, carolPrivKey, bobToCarol, bobToCarol, secondChanID)
	if err != nil {
		t.Fatalf("unable to create bob<->carol channel: %v", err)
	}

	globalEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {
		},
	}
	globalPolicy := ForwardingPolicy{
		MinHTLC:       lnwire.NewMSatFromSatoshis(5),
		BaseFee:       lnwire.NewMSatFromSatoshis(1),
		TimeLockDelta: 6,
	}
	obfuscator := newMockObfuscator()
	aliceChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              bobServer,
			Switch:            aliceServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (Obfuscator,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             aliceServer.registry,
			BlockEpochs:          globalEpoch,
		},
		aliceChannel,
		startingHeight,
	)
	if err := aliceServer.htlcSwitch.addLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice channel link: %v", err)
	}

	firstBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              aliceServer,
			Switch:            bobServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (Obfuscator,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             bobServer.registry,
			BlockEpochs:          globalEpoch,
		},
		firstBobChannel,
		startingHeight,
	)
	if err := bobServer.htlcSwitch.addLink(firstBobChannelLink); err != nil {
		t.Fatalf("unable to add first bob channel link: %v", err)
	}

	secondBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              carolServer,
			Switch:            bobServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (Obfuscator,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             bobServer.registry,
			BlockEpochs:          globalEpoch,
		},
		secondBobChannel,
		startingHeight,
	)
	if err := bobServer.htlcSwitch.addLink(secondBobChannelLink); err != nil {
		t.Fatalf("unable to add second bob channel link: %v", err)
	}

	carolChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy:     globalPolicy,
			Peer:              bobServer,
			Switch:            carolServer.htlcSwitch,
			DecodeHopIterator: decoder.DecodeHopIterator,
			DecodeOnionObfuscator: func(io.Reader) (Obfuscator,
				lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			GetLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:             carolServer.registry,
			BlockEpochs:          globalEpoch,
		},
		carolChannel,
		startingHeight,
	)
	if err := carolServer.htlcSwitch.addLink(carolChannelLink); err != nil {
		t.Fatalf("unable to add carol channel link: %v", err)
	}

	return &threeHopNetwork{
		aliceServer:          aliceServer,
		aliceChannelLink:     aliceChannelLink.(*channelLink),
		firstBobChannelLink:  firstBobChannelLink.(*channelLink),
		bobServer:            bobServer,
		secondBobChannelLink: secondBobChannelLink.(*channelLink),
		carolChannelLink:     carolChannelLink.(*channelLink),
		carolServer:          carolServer,

		firstChannelCleanup:  fCleanUp,
		secondChannelCleanup: sCleanUp,

		globalPolicy: globalPolicy,
	}
}
