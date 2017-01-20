package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"io/ioutil"
	"math/rand"
	"os"
	"testing"
	"time"
)

var (
	alicePrivKey = []byte("alice priv key")
	bobPrivKey   = []byte("bob priv key")
	carolPrivKey = []byte("carol priv key")
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

// GenerateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)

	// Make output always be non-zero.
	b[0] = byte(1)

	_, err := rand.Read(b[1:])
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

// CreateTestChannel creates the channel and returns our and remote channels
// representations.
func CreateTestChannel(
	alicePrivKey, bobPrivKey []byte,
	aliceAmount, bobAmount btcutil.Amount) (
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
	randomSeed, err := GenerateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, nil, err
	}
	copy(hash[:], randomSeed)

	prevOut := &wire.OutPoint{
		Hash:  chainhash.Hash(hash),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	theirElkrem := elkrem.NewElkremSender(
		lnwallet.DeriveElkremRoot(
			bobKeyPriv,
			bobKeyPub,
			aliceKeyPub,
		),
	)
	bobFirstRevoke, err := theirElkrem.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobRevokeKey := lnwallet.DeriveRevocationPubkey(aliceKeyPub, bobFirstRevoke[:])

	ourElkrem := elkrem.NewElkremSender(
		lnwallet.DeriveElkremRoot(
			aliceKeyPriv,
			aliceKeyPub,
			bobKeyPub,
		),
	)
	aliceFirstRevoke, err := ourElkrem.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceRevokeKey := lnwallet.DeriveRevocationPubkey(bobKeyPub, aliceFirstRevoke[:])

	aliceCommitTx, err := lnwallet.CreateCommitTx(
		fundingTxIn,
		aliceKeyPub,
		bobKeyPub,
		aliceRevokeKey,
		csvTimeoutAlice,
		aliceAmount,
		bobAmount,
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

	firstChannelState := &channeldb.OpenChannel{
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
		LocalElkrem:            ourElkrem,
		RemoteElkrem:           &elkrem.ElkremReceiver{},
		TheirDustLimit:         bobDustLimit,
		OurDustLimit:           aliceDustLimit,
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
		LocalElkrem:            theirElkrem,
		RemoteElkrem:           &elkrem.ElkremReceiver{},
		TheirDustLimit:         aliceDustLimit,
		OurDustLimit:           bobDustLimit,
		Db:                     dbBob,
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &MockSigner{aliceKeyPriv}
	bobSigner := &MockSigner{bobKeyPriv}

	notifier := &MockNotifier{}

	channelAlice, err := lnwallet.NewLightningChannel(aliceSigner, nil,
		notifier, firstChannelState)
	if err != nil {
		return nil, nil, nil, err
	}
	channelBob, err := lnwallet.NewLightningChannel(bobSigner, nil,
		notifier, bobChannelState)
	if err != nil {
		return nil, nil, nil, err
	}

	return channelAlice, channelBob, cleanUpFunc, nil
}

// Cluster is used for managing the created cluster of 3 hops.
type Cluster struct {
	aliceServer      *MockServer
	aliceHtlcManager *htlcManager

	firstBobHtlcManager  *htlcManager
	bobServer            *MockServer
	secondBobHtlcManager *htlcManager

	carolHtlcManager *htlcManager
	carolServer      *MockServer

	aliceRegistry *MockInvoiceRegistry

	firstChannelCleanup  func()
	secondChannelCleanup func()
}

// MakeCarolToAlicePayment makes payment from Carol to Alice over Bob which
// includes:
// * creating the invoice and adding it on Alice side.
// * create Carol payment request.
// * handle user request by Carol HTLC switch.
func (c *Cluster) MakeCarolToAlicePayment(amount btcutil.Amount,
	wrongRhash bool, wrongHop bool) (chan error, *channeldb.Invoice, error) {

	rand.Seed(time.Now().UTC().UnixNano())

	var preimage [sha256.Size]byte
	r, err := GenerateRandomBytes(10)
	if err != nil {
		return nil, nil, err
	}
	copy(preimage[:], r)

	var rhash [sha256.Size]byte
	if wrongRhash {
		rhash = fastsha256.Sum256([]byte{byte(0)})
	} else {
		rhash = fastsha256.Sum256(preimage[:])
	}

	invoice := &channeldb.Invoice{
		CreationDate: time.Now(),
		Terms: channeldb.ContractTerm{
			Value:           amount,
			PaymentPreimage: preimage,
		},
	}
	c.aliceRegistry.AddInvoice(invoice)

	var hopIterator routing.HopIterator
	if wrongHop {
		var wrongHopID routing.HopID
		copy(wrongHopID[:], btcutil.Hash160([]byte("wrong id")))

		hopIterator = NewMockHopIterator(
			c.bobServer.HopID(),
			&wrongHopID,
		)
	} else {
		hopIterator = NewMockHopIterator(
			c.bobServer.HopID(),
			c.aliceServer.HopID(),
		)
	}

	hop := hopIterator.Next()
	data, err := hopIterator.ToBytes()
	if err != nil {
		return nil, nil, err
	}

	request := NewUserAddRequest(hop, &lnwire.HTLCAddRequest{
		RedemptionHashes: [][sha256.Size]byte{rhash},
		OnionBlob:        data,
		Amount:           amount,
	})

	if err := c.carolServer.htlcSwitch.Forward(request); err != nil {
		return nil, nil, err
	}

	return request.Error(), invoice, nil
}

// MakeBobToAlicePayment makes payment from Bob to Alice which includes:
// * creating the invoice and adding it on Alice side.
// * create Bob payment request.
// * handle user request by Bob HTLC switch.
func (c *Cluster) MakeBobToAlicePayment(amount btcutil.Amount) (
	chan error, *channeldb.Invoice, error) {

	preimage := [sha256.Size]byte{byte(rand.Int())}
	rhash := fastsha256.Sum256(preimage[:])
	invoice := &channeldb.Invoice{
		CreationDate: time.Now(),
		Terms: channeldb.ContractTerm{
			Value:           amount,
			PaymentPreimage: preimage,
		},
	}
	c.aliceRegistry.AddInvoice(invoice)

	hopIterator := NewMockHopIterator(c.aliceServer.HopID())

	hop := hopIterator.Next()
	data, err := hopIterator.ToBytes()
	if err != nil {
		return nil, nil, err
	}

	request := NewUserAddRequest(hop, &lnwire.HTLCAddRequest{
		RedemptionHashes: [][sha256.Size]byte{rhash},
		OnionBlob:        data,
		Amount:           amount,
	})

	if err := c.bobServer.htlcSwitch.Forward(request); err != nil {
		return nil, nil, err
	}

	return request.Error(), invoice, nil
}

func (c *Cluster) StartCluster() error {
	if err := c.aliceServer.Start(); err != nil {
		return err
	}
	if err := c.bobServer.Start(); err != nil {
		return err
	}
	if err := c.carolServer.Start(); err != nil {
		return err
	}

	return nil
}

// StopCluster stop nodes and cleanup its databases.
func (c *Cluster) StopCluster() {
	c.aliceServer.Stop()
	c.aliceServer.Wait()

	c.carolServer.Stop()
	c.carolServer.Wait()

	c.bobServer.Stop()
	c.bobServer.Wait()

	c.firstChannelCleanup()
	c.secondChannelCleanup()
}

// CreateCluster function create the following topology and returns the
// control object to manage this cluster:
//
//	alice			   bob				   carol
//	server - <-connection-> - server - - <-connection-> - - - server
//	 |		   	  |				   |
//   alice htlc			bob htlc		    carol htlc
//     switch			switch	\		    switch
//	|			 |       \			|
//	|			 |        \			|
// alice htlc  <-channel->  first bob    second bob <-channel-> carol htlc
// manager	    	  htlc manager   htlc manager		manager
//
func CreateCluster(t *testing.T) *Cluster {
	var err error

	// In order to see the lnwire messages which server is receiving set
	// 'debug' to the 'true'.
	aliceServer := NewMockServer(t, "alice", false)
	bobServer := NewMockServer(t, "bob", false)
	carolServer := NewMockServer(t, "carol", false)

	// Create mock decoder instead of sphinx one in order to mock the
	// route which htlc should follow.
	decoder := &MockIteratorDecoder{}

	// Create lightning channels between Alice<->Bob and Bob<->Carol
	aliceChannel, firstBobChannel, fCleanUp, err := CreateTestChannel(
		alicePrivKey,
		bobPrivKey,
		btcutil.Amount(btcutil.SatoshiPerBitcoin*3),
		btcutil.Amount(btcutil.SatoshiPerBitcoin*3),
	)
	if err != nil {
		t.Fatal(err)
	}

	secondBobChannel, carolChannel, sCleanUp, err := CreateTestChannel(
		bobPrivKey,
		carolPrivKey,
		btcutil.Amount(btcutil.SatoshiPerBitcoin*5),
		btcutil.Amount(btcutil.SatoshiPerBitcoin*5),
	)
	if err != nil {
		t.Fatal(err)
	}

	aliceRegistry := NewRegistry()

	aliceHtlcManager := NewHTLCManager(
		&HTLCManagerConfig{
			// htlc responses will be sent to this node
			Peer: bobServer,
			// htlc will be propagated to this switch
			Forward: aliceServer.htlcSwitch.Forward,
			// route will be generated by this decoder
			DecodeOnion: decoder.Decode,
			Registry:    aliceRegistry,
		}, aliceChannel)
	if err := aliceServer.htlcSwitch.Add(aliceHtlcManager); err != nil {
		t.Fatal(err)
	}

	firstBobHtlcManager := NewHTLCManager(
		&HTLCManagerConfig{
			Peer:        aliceServer,
			Forward:     bobServer.htlcSwitch.Forward,
			DecodeOnion: decoder.Decode,
		}, firstBobChannel)
	if err := bobServer.htlcSwitch.Add(firstBobHtlcManager); err != nil {
		t.Fatal(err)
	}

	secondBobHtlcManager := NewHTLCManager(
		&HTLCManagerConfig{
			Peer:        carolServer,
			Forward:     bobServer.htlcSwitch.Forward,
			DecodeOnion: decoder.Decode,
		}, secondBobChannel)

	if err := bobServer.htlcSwitch.Add(secondBobHtlcManager); err != nil {
		t.Fatal(err)
	}

	carolHtlcManager := NewHTLCManager(
		&HTLCManagerConfig{
			Peer:        bobServer,
			Forward:     carolServer.htlcSwitch.Forward,
			DecodeOnion: decoder.Decode,
		}, carolChannel)
	if err := carolServer.htlcSwitch.Add(carolHtlcManager); err != nil {
		t.Fatal(err)
	}

	return &Cluster{
		aliceServer:      aliceServer,
		aliceHtlcManager: aliceHtlcManager.(*htlcManager),

		firstBobHtlcManager:  firstBobHtlcManager.(*htlcManager),
		bobServer:            bobServer,
		secondBobHtlcManager: secondBobHtlcManager.(*htlcManager),

		carolHtlcManager: carolHtlcManager.(*htlcManager),
		carolServer:      carolServer,

		aliceRegistry: aliceRegistry,

		firstChannelCleanup:  fCleanUp,
		secondChannelCleanup: sCleanUp,
	}
}
