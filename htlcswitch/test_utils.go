package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"testing"
	"time"

	"io/ioutil"
	"os"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
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
)

// generateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)

	_, err := rand.Read(b[:])
	// Note that Err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

// createTestChannel creates the channel and returns our and remote channels
// representations.
func createTestChannel(alicePrivKey, bobPrivKey []byte,
	aliceAmount, bobAmount btcutil.Amount, chanID lnwire.ShortChannelID) (
	*lnwallet.LightningChannel, *lnwallet.LightningChannel, func(), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), alicePrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), bobPrivKey)

	channelCapacity := aliceAmount + bobAmount
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(800)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	witnessScript, _, err := lnwallet.GenFundingPkScript(
		aliceKeyPub.SerializeCompressed(),
		bobKeyPub.SerializeCompressed(),
		int64(channelCapacity),
	)
	if err != nil {
		return nil, nil, nil, err
	}

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

	bobRoot := lnwallet.DeriveRevocationRoot(bobKeyPriv, bobKeyPub,
		aliceKeyPub)
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobRevokeKey := lnwallet.DeriveRevocationPubkey(aliceKeyPub,
		bobFirstRevoke[:])

	aliceRoot := lnwallet.DeriveRevocationRoot(aliceKeyPriv, aliceKeyPub,
		bobKeyPub)
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceRevokeKey := lnwallet.DeriveRevocationPubkey(bobKeyPub,
		aliceFirstRevoke[:])

	aliceCommitTx, err := lnwallet.CreateCommitTx(
		fundingTxIn,
		aliceKeyPub,
		bobKeyPub,
		aliceRevokeKey,
		csvTimeoutAlice,
		aliceAmount,
		bobAmount,
		lnwallet.DefaultDustLimit(),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitTx, err := lnwallet.CreateCommitTx(
		fundingTxIn,
		bobKeyPub,
		aliceKeyPub,
		bobRevokeKey,
		csvTimeoutBob,
		bobAmount,
		aliceAmount,
		lnwallet.DefaultDustLimit(),
	)
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

	aliceChannelState := &channeldb.OpenChannel{
		IdentityPub:            aliceKeyPub,
		ChanID:                 prevOut,
		ChanType:               channeldb.SingleFunder,
		IsInitiator:            true,
		StateHintObsfucator:    obsfucator,
		OurCommitKey:           aliceKeyPub,
		TheirCommitKey:         bobKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             aliceAmount,
		TheirBalance:           bobAmount,
		OurCommitTx:            aliceCommitTx,
		OurCommitSig:           bytes.Repeat([]byte{1}, 71),
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         aliceKeyPub,
		TheirMultiSigKey:       bobKeyPub,
		FundingWitnessScript:   witnessScript,
		LocalCsvDelay:          csvTimeoutAlice,
		RemoteCsvDelay:         csvTimeoutBob,
		TheirCurrentRevocation: bobRevokeKey,
		RevocationProducer:     alicePreimageProducer,
		RevocationStore:        shachain.NewRevocationStore(),
		TheirDustLimit:         bobDustLimit,
		OurDustLimit:           aliceDustLimit,
		ShortChanID:            chanID,
		Db:                     dbAlice,
	}
	bobChannelState := &channeldb.OpenChannel{
		IdentityPub:            bobKeyPub,
		ChanID:                 prevOut,
		ChanType:               channeldb.SingleFunder,
		IsInitiator:            false,
		StateHintObsfucator:    obsfucator,
		OurCommitKey:           bobKeyPub,
		TheirCommitKey:         aliceKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             bobAmount,
		TheirBalance:           aliceAmount,
		OurCommitTx:            bobCommitTx,
		OurCommitSig:           bytes.Repeat([]byte{1}, 71),
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         bobKeyPub,
		TheirMultiSigKey:       aliceKeyPub,
		FundingWitnessScript:   witnessScript,
		LocalCsvDelay:          csvTimeoutBob,
		RemoteCsvDelay:         csvTimeoutAlice,
		TheirCurrentRevocation: aliceRevokeKey,
		RevocationProducer:     bobPreimageProducer,
		RevocationStore:        shachain.NewRevocationStore(),
		TheirDustLimit:         aliceDustLimit,
		OurDustLimit:           bobDustLimit,
		ShortChanID:            chanID,
		Db:                     dbBob,
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}
	estimator := &lnwallet.StaticFeeEstimator{
		FeeRate:      24,
		Confirmation: 6,
	}

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
func generatePayment(invoiceAmt, htlcAmt btcutil.Amount, timelock uint32,
	blob [lnwire.OnionPacketSize]byte) (*channeldb.Invoice, *lnwire.UpdateAddHTLC, error) {

	// Initialize random seed with unix time in order to generate random
	// preimage every time.
	rand.Seed(time.Now().UTC().UnixNano())

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
func generateHops(payAmt btcutil.Amount,
	path ...*channelLink) (btcutil.Amount, uint32, []ForwardingInfo) {

	lastHop := path[len(path)-1]

	var (
		runningAmt    btcutil.Amount = payAmt
		totalTimelock uint32
	)

	hops := make([]ForwardingInfo, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		// If this is the last hop, then the next hop is the special
		// "exit node". Otherwise, we look to the "prior" hop.
		nextHop := exitHop
		if i != len(path)-1 {
			nextHop = path[i+1].channel.ShortChanID()
		}

		// If this is the last, hop, then the time lock will be their
		// specified delta policy.
		timeLock := lastHop.cfg.FwrdingPolicy.TimeLockDelta
		totalTimelock += timeLock

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
	invoiceAmt, htlcAmt btcutil.Amount,
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
		_, err := sender.htlcSwitch.SendHTLC(firstHopPub, htlc)
		errChan <- err
	}()

	select {
	case err := <-errChan:
		return invoice, err
	case <-time.After(12 * time.Second):
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
	bobToCarol btcutil.Amount) *threeHopNetwork {
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

	globalPolicy := ForwardingPolicy{
		MinHTLC:       5,
		BaseFee:       btcutil.Amount(1),
		TimeLockDelta: 1,
	}

	aliceChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy: globalPolicy,
			Peer:          bobServer,
			Switch:        aliceServer.htlcSwitch,
			DecodeOnion:   decoder.Decode,
			Registry:      aliceServer.registry,
		},
		aliceChannel,
	)
	if err := aliceServer.htlcSwitch.addLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice channel link: %v", err)
	}

	firstBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy: globalPolicy,
			Peer:          aliceServer,
			Switch:        bobServer.htlcSwitch,
			DecodeOnion:   decoder.Decode,
			Registry:      bobServer.registry,
		},
		firstBobChannel,
	)
	if err := bobServer.htlcSwitch.addLink(firstBobChannelLink); err != nil {
		t.Fatalf("unable to add first bob channel link: %v", err)
	}

	secondBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy: globalPolicy,
			Peer:          carolServer,
			Switch:        bobServer.htlcSwitch,
			DecodeOnion:   decoder.Decode,
			Registry:      bobServer.registry,
		},
		secondBobChannel,
	)
	if err := bobServer.htlcSwitch.addLink(secondBobChannelLink); err != nil {
		t.Fatalf("unable to add second bob channel link: %v", err)
	}

	carolChannelLink := NewChannelLink(
		ChannelLinkConfig{
			FwrdingPolicy: globalPolicy,
			Peer:          bobServer,
			Switch:        carolServer.htlcSwitch,
			DecodeOnion:   decoder.Decode,
			Registry:      carolServer.registry,
		},
		carolChannel,
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
