package htlcswitch

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// maxInflightHtlcs specifies the max number of inflight HTLCs. This number is
// chosen to be smaller than the default 483 so the test can run faster.
const maxInflightHtlcs = 50

var (
	alicePrivKey = []byte("alice priv key")
	bobPrivKey   = []byte("bob priv key")
	carolPrivKey = []byte("carol priv key")

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar   = new(btcec.ModNScalar)
	testSScalar   = new(btcec.ModNScalar)
	_             = testRScalar.SetByteSlice(testRBytes)
	_             = testSScalar.SetByteSlice(testSBytes)
	testSig       = ecdsa.NewSignature(testRScalar, testSScalar)

	wireSig, _ = lnwire.NewSigFromSignature(testSig)

	testBatchTimeout = 50 * time.Millisecond
)

var idSeqNum uint64

// genID generates a unique tuple to identify a test channel.
func genID() (lnwire.ChannelID, lnwire.ShortChannelID) {
	id := atomic.AddUint64(&idSeqNum, 1)

	var scratch [8]byte

	binary.BigEndian.PutUint64(scratch[:], id)
	hash1, _ := chainhash.NewHash(bytes.Repeat(scratch[:], 4))

	chanPoint1 := wire.NewOutPoint(hash1, uint32(id))
	chanID1 := lnwire.NewChanIDFromOutPoint(*chanPoint1)
	aliceChanID := lnwire.NewShortChanIDFromInt(id)

	return chanID1, aliceChanID
}

// genIDs generates ids for two test channels.
func genIDs() (lnwire.ChannelID, lnwire.ChannelID, lnwire.ShortChannelID,
	lnwire.ShortChannelID) {

	chanID1, aliceChanID := genID()
	chanID2, bobChanID := genID()

	return chanID1, chanID2, aliceChanID, bobChanID
}

// mockGetChanUpdateMessage helper function which returns topology update of
// the channel
func mockGetChanUpdateMessage(_ lnwire.ShortChannelID) (*lnwire.ChannelUpdate1,
	error) {

	return &lnwire.ChannelUpdate1{
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

	_, err := crand.Read(b)
	// Note that Err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

type testLightningChannel struct {
	channel *lnwallet.LightningChannel
	restore func() (*lnwallet.LightningChannel, error)
}

// createTestChannel creates the channel and returns our and remote channels
// representations.
//
// TODO(roasbeef): need to factor out, similar func re-used in many parts of codebase
func createTestChannel(t *testing.T, alicePrivKey, bobPrivKey []byte,
	aliceAmount, bobAmount, aliceReserve, bobReserve btcutil.Amount,
	chanID lnwire.ShortChannelID) (*testLightningChannel,
	*testLightningChannel, error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(alicePrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(bobPrivKey)

	channelCapacity := aliceAmount + bobAmount
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)
	isAliceInitiator := true

	aliceBounds := channeldb.ChannelStateBounds{
		MaxPendingAmount: lnwire.NewMSatFromSatoshis(
			channelCapacity),
		ChanReserve:      aliceReserve,
		MinHTLC:          0,
		MaxAcceptedHtlcs: maxInflightHtlcs,
	}
	aliceCommitParams := channeldb.CommitmentParams{
		DustLimit: btcutil.Amount(200),
		CsvDelay:  uint16(csvTimeoutAlice),
	}

	bobBounds := channeldb.ChannelStateBounds{
		MaxPendingAmount: lnwire.NewMSatFromSatoshis(
			channelCapacity),
		ChanReserve:      bobReserve,
		MinHTLC:          0,
		MaxAcceptedHtlcs: maxInflightHtlcs,
	}
	bobCommitParams := channeldb.CommitmentParams{
		DustLimit: btcutil.Amount(800),
		CsvDelay:  uint16(csvTimeoutBob),
	}

	var hash [sha256.Size]byte
	randomSeed, err := generateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, err
	}
	copy(hash[:], randomSeed)

	prevOut := &wire.OutPoint{
		Hash:  chainhash.Hash(hash),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	aliceCfg := channeldb.ChannelConfig{
		ChannelStateBounds: aliceBounds,
		CommitmentParams:   aliceCommitParams,
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
		ChannelStateBounds: bobBounds,
		CommitmentParams:   bobCommitParams,
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
		return nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(
		aliceAmount, bobAmount, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, channeldb.SingleFunderTweaklessBit,
		isAliceInitiator, 0,
	)
	if err != nil {
		return nil, nil, err
	}

	dbAlice := channeldb.OpenForTesting(t, t.TempDir())
	dbBob := channeldb.OpenForTesting(t, t.TempDir())

	estimator := chainfee.NewStaticEstimator(6000, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, nil, err
	}
	commitFee := feePerKw.FeeForWeight(724)

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
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(bobAmount),
		RemoteBalance: lnwire.NewMSatFromSatoshis(aliceAmount - commitFee),
		CommitFee:     commitFee,
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitTx:      bobCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             isAliceInitiator,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		ShortChannelID:          chanID,
		Db:                      dbAlice.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(chanID),
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
		ShortChannelID:          chanID,
		Db:                      dbBob.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(chanID),
	}

	if err := aliceChannelState.SyncPending(bobAddr, broadcastHeight); err != nil {
		return nil, nil, err
	}

	if err := bobChannelState.SyncPending(aliceAddr, broadcastHeight); err != nil {
		return nil, nil, err
	}

	aliceSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{aliceKeyPriv}, nil,
	)
	bobSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{bobKeyPriv}, nil,
	)

	alicePool := lnwallet.NewSigPool(runtime.NumCPU(), aliceSigner)
	signerMock := lnwallet.NewDefaultAuxSignerMock(t)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(signerMock),
	)
	if err != nil {
		return nil, nil, err
	}
	alicePool.Start()

	bobPool := lnwallet.NewSigPool(runtime.NumCPU(), bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, bobChannelState, bobPool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(signerMock),
	)
	if err != nil {
		return nil, nil, err
	}
	bobPool.Start()

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	aliceNextRevoke, err := channelAlice.NextRevocationKey()
	if err != nil {
		return nil, nil, err
	}
	if err := channelBob.InitNextRevocation(aliceNextRevoke); err != nil {
		return nil, nil, err
	}

	bobNextRevoke, err := channelBob.NextRevocationKey()
	if err != nil {
		return nil, nil, err
	}
	if err := channelAlice.InitNextRevocation(bobNextRevoke); err != nil {
		return nil, nil, err
	}

	restoreAlice := func() (*lnwallet.LightningChannel, error) {
		aliceStoredChannels, err := dbAlice.ChannelStateDB().
			FetchOpenChannels(aliceKeyPub)
		switch err {
		case nil:
		case kvdb.ErrDatabaseNotOpen:
			dbAlice = channeldb.OpenForTesting(t, dbAlice.Path())

			aliceStoredChannels, err = dbAlice.ChannelStateDB().
				FetchOpenChannels(aliceKeyPub)
			if err != nil {
				return nil, fmt.Errorf("unable to fetch alice "+
					"channel: %w", err)
			}
		default:
			return nil, fmt.Errorf("unable to fetch alice "+
				"channel: %w", err)
		}

		var aliceStoredChannel *channeldb.OpenChannel
		for _, channel := range aliceStoredChannels {
			if channel.FundingOutpoint.String() == prevOut.String() {
				aliceStoredChannel = channel
				break
			}
		}

		if aliceStoredChannel == nil {
			return nil, errors.New("unable to find stored alice channel")
		}

		newAliceChannel, err := lnwallet.NewLightningChannel(
			aliceSigner, aliceStoredChannel, alicePool,
			lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
			lnwallet.WithAuxSigner(signerMock),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create new "+
				"channel: %w", err)
		}

		return newAliceChannel, nil
	}

	restoreBob := func() (*lnwallet.LightningChannel, error) {
		bobStoredChannels, err := dbBob.ChannelStateDB().
			FetchOpenChannels(bobKeyPub)
		switch err {
		case nil:
		case kvdb.ErrDatabaseNotOpen:
			dbBob = channeldb.OpenForTesting(t, dbBob.Path())
			if err != nil {
				return nil, fmt.Errorf("unable to reopen bob "+
					"db: %w", err)
			}

			bobStoredChannels, err = dbBob.ChannelStateDB().
				FetchOpenChannels(bobKeyPub)
			if err != nil {
				return nil, fmt.Errorf("unable to fetch bob "+
					"channel: %w", err)
			}
		default:
			return nil, fmt.Errorf("unable to fetch bob channel: "+
				"%w", err)
		}

		var bobStoredChannel *channeldb.OpenChannel
		for _, channel := range bobStoredChannels {
			if channel.FundingOutpoint.String() == prevOut.String() {
				bobStoredChannel = channel
				break
			}
		}

		if bobStoredChannel == nil {
			return nil, errors.New("unable to find stored bob channel")
		}

		newBobChannel, err := lnwallet.NewLightningChannel(
			bobSigner, bobStoredChannel, bobPool,
			lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
			lnwallet.WithAuxSigner(signerMock),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create new "+
				"channel: %w", err)
		}
		return newBobChannel, nil
	}

	testLightningChannelAlice := &testLightningChannel{
		channel: channelAlice,
		restore: restoreAlice,
	}

	testLightningChannelBob := &testLightningChannel{
		channel: channelBob,
		restore: restoreBob,
	}

	return testLightningChannelAlice, testLightningChannelBob, nil
}

// getChanID retrieves the channel point from an lnnwire message.
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
	case *lnwire.ChannelReady:
		chanID = msg.ChanID
	case *lnwire.UpdateFee:
		chanID = msg.ChanID
	default:
		return chanID, fmt.Errorf("unknown type: %T", msg)
	}

	return chanID, nil
}

// generateHoldPayment generates the htlc add request by given path blob and
// invoice which should be added by destination peer.
func generatePaymentWithPreimage(invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32, blob [lnwire.OnionPacketSize]byte,
	preimage *lntypes.Preimage, rhash, payAddr [32]byte) (
	*invoices.Invoice, *lnwire.UpdateAddHTLC, uint64, error) {

	// Create the db invoice. Normally the payment requests needs to be set,
	// because it is decoded in InvoiceRegistry to obtain the cltv expiry.
	// But because the mock registry used in tests is mocking the decode
	// step and always returning the value of testInvoiceCltvExpiry, we
	// don't need to bother here with creating and signing a payment
	// request.

	invoice := &invoices.Invoice{
		CreationDate: time.Now(),
		Terms: invoices.ContractTerm{
			FinalCltvDelta:  testInvoiceCltvExpiry,
			Value:           invoiceAmt,
			PaymentPreimage: preimage,
			PaymentAddr:     payAddr,
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
		},
		HodlInvoice: preimage == nil,
	}

	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      htlcAmt,
		Expiry:      timelock,
		OnionBlob:   blob,
	}

	pid, err := generateRandomBytes(8)
	if err != nil {
		return nil, nil, 0, err
	}
	paymentID := binary.BigEndian.Uint64(pid)

	return invoice, htlc, paymentID, nil
}

// generatePayment generates the htlc add request by given path blob and
// invoice which should be added by destination peer.
func generatePayment(invoiceAmt, htlcAmt lnwire.MilliSatoshi, timelock uint32,
	blob [lnwire.OnionPacketSize]byte) (*invoices.Invoice,
	*lnwire.UpdateAddHTLC, uint64, error) {

	var preimage lntypes.Preimage
	r, err := generateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, 0, err
	}
	copy(preimage[:], r)

	rhash := sha256.Sum256(preimage[:])

	var payAddr [sha256.Size]byte
	r, err = generateRandomBytes(sha256.Size)
	if err != nil {
		return nil, nil, 0, err
	}
	copy(payAddr[:], r)

	return generatePaymentWithPreimage(
		invoiceAmt, htlcAmt, timelock, blob, &preimage, rhash, payAddr,
	)
}

// generateRoute generates the path blob by given array of peers.
func generateRoute(hops ...*hop.Payload) (
	[lnwire.OnionPacketSize]byte, error) {

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
	aliceServer       *mockServer
	aliceChannelLink  *channelLink
	aliceOnionDecoder *mockIteratorDecoder

	bobServer            *mockServer
	firstBobChannelLink  *channelLink
	secondBobChannelLink *channelLink
	bobOnionDecoder      *mockIteratorDecoder

	carolServer       *mockServer
	carolChannelLink  *channelLink
	carolOnionDecoder *mockIteratorDecoder

	hopNetwork
}

// generateHops creates the per hop payload, the total amount to be sent, and
// also the time lock value needed to route an HTLC with the target amount over
// the specified path.
func generateHops(payAmt lnwire.MilliSatoshi, startingHeight uint32,
	path ...*channelLink) (lnwire.MilliSatoshi, uint32, []*hop.Payload) {

	totalTimelock := startingHeight
	runningAmt := payAmt

	hops := make([]*hop.Payload, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		// If this is the last hop, then the next hop is the special
		// "exit node". Otherwise, we look to the "prior" hop.
		nextHop := hop.Exit
		if i != len(path)-1 {
			nextHop = path[i+1].channel.ShortChanID()
		}

		var timeLock uint32
		// If this is the last, hop, then the time lock will be their
		// specified delta policy plus our starting height.
		if i == len(path)-1 {
			totalTimelock += testInvoiceCltvExpiry
			timeLock = totalTimelock
		} else {
			// Otherwise, the outgoing time lock should be the
			// incoming timelock minus their specified delta.
			delta := path[i+1].cfg.FwrdingPolicy.TimeLockDelta
			totalTimelock += delta
			timeLock = totalTimelock - delta
		}

		// Finally, we'll need to calculate the amount to forward. For
		// the last hop, it's just the payment amount.
		amount := payAmt
		if i != len(path)-1 {
			prevHop := hops[i+1]
			prevAmount := prevHop.ForwardingInfo().AmountToForward

			fee := ExpectedFee(path[i].cfg.FwrdingPolicy, prevAmount)
			runningAmt += fee

			// Otherwise, for a node to forward an HTLC, then
			// following inequality most hold true:
			//     * amt_in - fee >= amt_to_forward
			amount = runningAmt - fee
		}

		var nextHopBytes [8]byte
		binary.BigEndian.PutUint64(nextHopBytes[:], nextHop.ToUint64())

		hops[i] = hop.NewLegacyPayload(&sphinx.HopData{
			Realm:         [1]byte{}, // hop.BitcoinNetwork
			NextAddress:   nextHopBytes,
			ForwardAmount: uint64(amount),
			OutgoingCltv:  timeLock,
		})
	}

	return runningAmt, totalTimelock, hops
}

type paymentResponse struct {
	rhash lntypes.Hash
	err   chan error
}

func (r *paymentResponse) Wait(d time.Duration) (lntypes.Hash, error) {
	return r.rhash, waitForPaymentResult(r.err, d)
}

// waitForPaymentResult waits for either an error to be received on c or a
// timeout.
func waitForPaymentResult(c chan error, d time.Duration) error {
	select {
	case err := <-c:
		close(c)
		return err
	case <-time.After(d):
		return errors.New("htlc was not settled in time")
	}
}

// waitForPayFuncResult executes the given function and waits for a result with
// a timeout.
func waitForPayFuncResult(payFunc func() error, d time.Duration) error {
	errChan := make(chan error)
	go func() {
		errChan <- payFunc()
	}()

	return waitForPaymentResult(errChan, d)
}

// makePayment takes the destination node and amount as input, sends the
// payment and returns the error channel to wait for error to be received and
// invoice in order to check its status after the payment finished.
//
// With this function you can send payments:
// * from Alice to Bob
// * from Alice to Carol through the Bob
// * from Alice to some another peer through the Bob
func makePayment(sendingPeer, receivingPeer lnpeer.Peer,
	firstHop lnwire.ShortChannelID, hops []*hop.Payload,
	invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32) *paymentResponse {

	paymentErr := make(chan error, 1)
	var rhash lntypes.Hash

	invoice, payFunc, err := preparePayment(sendingPeer, receivingPeer,
		firstHop, hops, invoiceAmt, htlcAmt, timelock,
	)
	if err != nil {
		paymentErr <- err
		return &paymentResponse{
			rhash: rhash,
			err:   paymentErr,
		}
	}

	rhash = invoice.Terms.PaymentPreimage.Hash()

	// Send payment and expose err channel.
	go func() {
		paymentErr <- payFunc()
	}()

	return &paymentResponse{
		rhash: rhash,
		err:   paymentErr,
	}
}

// preparePayment creates an invoice at the receivingPeer and returns a function
// that, when called, launches the payment from the sendingPeer.
func preparePayment(sendingPeer, receivingPeer lnpeer.Peer,
	firstHop lnwire.ShortChannelID, hops []*hop.Payload,
	invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32) (*invoices.Invoice, func() error, error) {

	sender := sendingPeer.(*mockServer)
	receiver := receivingPeer.(*mockServer)

	// Generate route convert it to blob, and return next destination for
	// htlc add request.
	blob, err := generateRoute(hops...)
	if err != nil {
		return nil, nil, err
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, pid, err := generatePayment(
		invoiceAmt, htlcAmt, timelock, blob,
	)
	if err != nil {
		return nil, nil, err
	}

	// Check who is last in the route and add invoice to server registry.
	hash := invoice.Terms.PaymentPreimage.Hash()
	if err := receiver.registry.AddInvoice(
		context.Background(), *invoice, hash,
	); err != nil {
		return nil, nil, err
	}

	// Send payment and expose err channel.
	return invoice, func() error {
		err := sender.htlcSwitch.SendHTLC(
			firstHop, pid, htlc,
		)
		if err != nil {
			return err
		}
		resultChan, err := sender.htlcSwitch.GetAttemptResult(
			pid, hash, newMockDeobfuscator(),
		)
		if err != nil {
			return err
		}

		result, ok := <-resultChan
		if !ok {
			return fmt.Errorf("shutting down")
		}

		if result.Error != nil {
			return result.Error
		}

		return nil
	}, nil
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

	return waitLinksEligible(map[string]*channelLink{
		"alice":      n.aliceChannelLink,
		"bob first":  n.firstBobChannelLink,
		"bob second": n.secondBobChannelLink,
		"carol":      n.carolChannelLink,
	})
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
}

type clusterChannels struct {
	aliceToBob *lnwallet.LightningChannel
	bobToAlice *lnwallet.LightningChannel
	bobToCarol *lnwallet.LightningChannel
	carolToBob *lnwallet.LightningChannel
}

// createClusterChannels creates lightning channels which are needed for
// network cluster to be initialized.
func createClusterChannels(t *testing.T, aliceToBob, bobToCarol btcutil.Amount) (
	*clusterChannels, func() (*clusterChannels, error), error) {

	_, _, firstChanID, secondChanID := genIDs()

	// Create lightning channels between Alice<->Bob and Bob<->Carol
	aliceChannel, firstBobChannel, err := createTestChannel(t, alicePrivKey,
		bobPrivKey, aliceToBob, aliceToBob, 0, 0, firstChanID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create "+
			"alice<->bob channel: %w", err)
	}

	secondBobChannel, carolChannel, err := createTestChannel(t, bobPrivKey,
		carolPrivKey, bobToCarol, bobToCarol, 0, 0, secondChanID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create "+
			"bob<->carol channel: %w", err)
	}

	restoreFromDb := func() (*clusterChannels, error) {

		a2b, err := aliceChannel.restore()
		if err != nil {
			return nil, err
		}

		b2a, err := firstBobChannel.restore()
		if err != nil {
			return nil, err
		}

		b2c, err := secondBobChannel.restore()
		if err != nil {
			return nil, err
		}

		c2b, err := carolChannel.restore()
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
		aliceToBob: aliceChannel.channel,
		bobToAlice: firstBobChannel.channel,
		bobToCarol: secondBobChannel.channel,
		carolToBob: carolChannel.channel,
	}, restoreFromDb, nil
}

// newThreeHopNetwork function creates the following topology and returns the
// control object to manage this cluster:
//
// alice		      bob			     carol
// server - <-connection-> - server - - <-connection-> - - - server
//
//	|		   	|			       |
//
// alice htlc		     bob htlc		          carol htlc
// switch		      switch	\		    switch
//
//	|			 |       \		       |
//	|			 |        \		       |
//
// alice                   first bob     second bob           carol
// channel link	    	  channel link   channel link      channel link
//
// This function takes server options which can be used to apply custom
// settings to alice, bob and carol.
func newThreeHopNetwork(t testing.TB, aliceChannel, firstBobChannel,
	secondBobChannel, carolChannel *lnwallet.LightningChannel,
	startingHeight uint32, opts ...serverOption) *threeHopNetwork {

	aliceDb := aliceChannel.State().Db.GetParentDB()
	bobDb := firstBobChannel.State().Db.GetParentDB()
	carolDb := carolChannel.State().Db.GetParentDB()

	hopNetwork := newHopNetwork()

	// Create three peers/servers.
	aliceServer, err := newMockServer(
		t, "alice", startingHeight, aliceDb, hopNetwork.defaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobServer, err := newMockServer(
		t, "bob", startingHeight, bobDb, hopNetwork.defaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")
	carolServer, err := newMockServer(
		t, "carol", startingHeight, carolDb, hopNetwork.defaultDelta,
	)
	require.NoError(t, err, "unable to create carol server")

	// Apply all additional functional options to the servers before
	// creating any links.
	for _, option := range opts {
		option(aliceServer, bobServer, carolServer)
	}

	// Create mock decoder instead of sphinx one in order to mock the route
	// which htlc should follow.
	aliceDecoder := newMockIteratorDecoder()
	bobDecoder := newMockIteratorDecoder()
	carolDecoder := newMockIteratorDecoder()

	aliceChannelLink, err := hopNetwork.createChannelLink(aliceServer,
		bobServer, aliceChannel, aliceDecoder,
	)
	if err != nil {
		t.Fatal(err)
	}

	firstBobChannelLink, err := hopNetwork.createChannelLink(bobServer,
		aliceServer, firstBobChannel, bobDecoder)
	if err != nil {
		t.Fatal(err)
	}

	secondBobChannelLink, err := hopNetwork.createChannelLink(bobServer,
		carolServer, secondBobChannel, bobDecoder)
	if err != nil {
		t.Fatal(err)
	}

	carolChannelLink, err := hopNetwork.createChannelLink(carolServer,
		bobServer, carolChannel, carolDecoder)
	if err != nil {
		t.Fatal(err)
	}

	return &threeHopNetwork{
		aliceServer:       aliceServer,
		aliceChannelLink:  aliceChannelLink.(*channelLink),
		aliceOnionDecoder: aliceDecoder,

		bobServer:            bobServer,
		firstBobChannelLink:  firstBobChannelLink.(*channelLink),
		secondBobChannelLink: secondBobChannelLink.(*channelLink),
		bobOnionDecoder:      bobDecoder,

		carolServer:       carolServer,
		carolChannelLink:  carolChannelLink.(*channelLink),
		carolOnionDecoder: carolDecoder,

		hopNetwork: *hopNetwork,
	}
}

// serverOption is a function which alters the three servers created for
// a three hop network to allow custom settings on each server.
type serverOption func(aliceServer, bobServer, carolServer *mockServer)

// serverOptionWithHtlcNotifier is a functional option for the creation of
// three hop network servers which allows setting of htlc notifiers.
// Note that these notifiers should be started and stopped by the calling
// function.
func serverOptionWithHtlcNotifier(alice, bob,
	carol *HtlcNotifier) serverOption {

	return func(aliceServer, bobServer, carolServer *mockServer) {
		aliceServer.htlcSwitch.cfg.HtlcNotifier = alice
		bobServer.htlcSwitch.cfg.HtlcNotifier = bob
		carolServer.htlcSwitch.cfg.HtlcNotifier = carol
	}
}

// serverOptionRejectHtlc is the functional option for setting the reject
// htlc config option in each server's switch.
func serverOptionRejectHtlc(alice, bob, carol bool) serverOption {
	return func(aliceServer, bobServer, carolServer *mockServer) {
		aliceServer.htlcSwitch.cfg.RejectHTLC = alice
		bobServer.htlcSwitch.cfg.RejectHTLC = bob
		carolServer.htlcSwitch.cfg.RejectHTLC = carol
	}
}

// createMirroredChannel creates two LightningChannel objects which represent
// the state machines on either side of a single channel between alice and bob.
func createMirroredChannel(t *testing.T, aliceToBob,
	bobToAlice btcutil.Amount) (*testLightningChannel,
	*testLightningChannel, error) {

	_, _, firstChanID, _ := genIDs()

	// Create lightning channels between Alice<->Bob for Alice and Bob
	alice, bob, err := createTestChannel(t, alicePrivKey, bobPrivKey,
		aliceToBob, bobToAlice, 0, 0, firstChanID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create "+
			"alice<->bob channel: %w", err)
	}

	return alice, bob, nil
}

// hopNetwork is the base struct for two and three hop networks
type hopNetwork struct {
	feeEstimator *mockFeeEstimator
	globalPolicy models.ForwardingPolicy
	obfuscator   hop.ErrorEncrypter

	defaultDelta uint32
}

func newHopNetwork() *hopNetwork {
	defaultDelta := uint32(6)

	globalPolicy := models.ForwardingPolicy{
		MinHTLCOut:    lnwire.NewMSatFromSatoshis(5),
		BaseFee:       lnwire.NewMSatFromSatoshis(1),
		TimeLockDelta: defaultDelta,
	}
	obfuscator := NewMockObfuscator()

	return &hopNetwork{
		feeEstimator: newMockFeeEstimator(),
		globalPolicy: globalPolicy,
		obfuscator:   obfuscator,
		defaultDelta: defaultDelta,
	}
}

func (h *hopNetwork) createChannelLink(server, peer *mockServer,
	channel *lnwallet.LightningChannel,
	decoder *mockIteratorDecoder) (ChannelLink, error) {

	const (
		fwdPkgTimeout       = 15 * time.Second
		minFeeUpdateTimeout = 30 * time.Minute
		maxFeeUpdateTimeout = 40 * time.Minute
	)

	notifyUpdateChan := make(chan *contractcourt.ContractUpdate)
	doneChan := make(chan struct{})
	notifyContractUpdate := func(u *contractcourt.ContractUpdate) error {
		select {
		case notifyUpdateChan <- u:
		case <-doneChan:
		}

		return nil
	}

	getAliases := func(
		base lnwire.ShortChannelID) []lnwire.ShortChannelID {

		return nil
	}

	forwardPackets := func(linkQuit <-chan struct{}, _ bool,
		packets ...*htlcPacket) error {

		return server.htlcSwitch.ForwardPackets(linkQuit, packets...)
	}

	//nolint:ll
	link := NewChannelLink(
		ChannelLinkConfig{
			BestHeight:         server.htlcSwitch.BestHeight,
			FwrdingPolicy:      h.globalPolicy,
			Peer:               peer,
			Circuits:           server.htlcSwitch.CircuitModifier(),
			ForwardPackets:     forwardPackets,
			DecodeHopIterators: decoder.DecodeHopIterators,
			ExtractErrorEncrypter: func(*btcec.PublicKey) (
				hop.ErrorEncrypter, lnwire.FailCode) {
				return h.obfuscator, lnwire.CodeNone
			},
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               server.registry,
			FeeEstimator:           h.feeEstimator,
			PreimageCache:          server.pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			NotifyContractUpdate:    notifyContractUpdate,
			ChainEvents:             &contractcourt.ChainEventSubscription{},
			SyncStates:              true,
			BatchSize:               10,
			BatchTicker:             ticker.NewForce(testBatchTimeout),
			FwdPkgGCTicker:          ticker.NewForce(fwdPkgTimeout),
			PendingCommitTicker:     ticker.New(2 * time.Minute),
			MinUpdateTimeout:        minFeeUpdateTimeout,
			MaxUpdateTimeout:        maxFeeUpdateTimeout,
			OnChannelFailure:        func(lnwire.ChannelID, lnwire.ShortChannelID, LinkFailureError) {},
			OutgoingCltvRejectDelta: 3,
			MaxOutgoingCltvExpiry:   DefaultMaxOutgoingCltvExpiry,
			MaxFeeAllocation:        DefaultMaxLinkFeeAllocation,
			MaxAnchorsCommitFeeRate: chainfee.SatPerKVByte(10 * 1000).FeePerKWeight(),
			NotifyActiveLink:        func(wire.OutPoint) {},
			NotifyActiveChannel:     func(wire.OutPoint) {},
			NotifyInactiveChannel:   func(wire.OutPoint) {},
			NotifyInactiveLinkEvent: func(wire.OutPoint) {},
			HtlcNotifier:            server.htlcSwitch.cfg.HtlcNotifier,
			GetAliases:              getAliases,
			ShouldFwdExpEndorsement: func() bool { return true },
		},
		channel,
	)
	if err := server.htlcSwitch.AddLink(link); err != nil {
		return nil, fmt.Errorf("unable to add channel link: %w", err)
	}

	go func() {
		if chanLink, ok := link.(*channelLink); ok {
			for {
				select {
				case <-notifyUpdateChan:
				case <-chanLink.cg.Done():
					close(doneChan)
					return
				}
			}
		}
	}()

	return link, nil
}

// twoHopNetwork is used for managing the created cluster of 2 hops.
type twoHopNetwork struct {
	hopNetwork

	aliceServer      *mockServer
	aliceChannelLink *channelLink

	bobServer      *mockServer
	bobChannelLink *channelLink
}

// newTwoHopNetwork function creates and starts the following topology and
// returns the control object to manage this cluster:
//
// alice                      bob
// server - <-connection-> - server
//
//	|                      |
//
// alice htlc               bob htlc
// switch                   switch
//
//	|                      |
//	|                      |
//
// alice                      bob
// channel link           channel link.
func newTwoHopNetwork(t testing.TB,
	aliceChannel, bobChannel *lnwallet.LightningChannel,
	startingHeight uint32) *twoHopNetwork {

	aliceDb := aliceChannel.State().Db.GetParentDB()
	bobDb := bobChannel.State().Db.GetParentDB()

	hopNetwork := newHopNetwork()

	// Create two peers/servers.
	aliceServer, err := newMockServer(
		t, "alice", startingHeight, aliceDb, hopNetwork.defaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobServer, err := newMockServer(
		t, "bob", startingHeight, bobDb, hopNetwork.defaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	// Create mock decoder instead of sphinx one in order to mock the route
	// which htlc should follow.
	aliceDecoder := newMockIteratorDecoder()
	bobDecoder := newMockIteratorDecoder()

	aliceChannelLink, err := hopNetwork.createChannelLink(
		aliceServer, bobServer, aliceChannel, aliceDecoder,
	)
	if err != nil {
		t.Fatal(err)
	}

	bobChannelLink, err := hopNetwork.createChannelLink(
		bobServer, aliceServer, bobChannel, bobDecoder,
	)
	if err != nil {
		t.Fatal(err)
	}

	n := &twoHopNetwork{
		aliceServer:      aliceServer,
		aliceChannelLink: aliceChannelLink.(*channelLink),

		bobServer:      bobServer,
		bobChannelLink: bobChannelLink.(*channelLink),

		hopNetwork: *hopNetwork,
	}

	require.NoError(t, n.start())
	t.Cleanup(n.stop)

	return n
}

// start starts the two hop network alice,bob servers.
func (n *twoHopNetwork) start() error {
	if err := n.aliceServer.Start(); err != nil {
		return err
	}
	if err := n.bobServer.Start(); err != nil {
		n.aliceServer.Stop()
		return err
	}

	return waitLinksEligible(map[string]*channelLink{
		"alice": n.aliceChannelLink,
		"bob":   n.bobChannelLink,
	})
}

// stop stops nodes and cleanup its databases.
func (n *twoHopNetwork) stop() {
	done := make(chan struct{})
	go func() {
		n.aliceServer.Stop()
		done <- struct{}{}
	}()

	go func() {
		n.bobServer.Stop()
		done <- struct{}{}
	}()

	for i := 0; i < 2; i++ {
		<-done
	}
}

func (n *twoHopNetwork) makeHoldPayment(sendingPeer, receivingPeer lnpeer.Peer,
	firstHop lnwire.ShortChannelID, hops []*hop.Payload,
	invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32, preimage lntypes.Preimage) chan error {

	paymentErr := make(chan error, 1)

	sender := sendingPeer.(*mockServer)
	receiver := receivingPeer.(*mockServer)

	// Generate route convert it to blob, and return next destination for
	// htlc add request.
	blob, err := generateRoute(hops...)
	if err != nil {
		paymentErr <- err
		return paymentErr
	}

	rhash := preimage.Hash()

	var payAddr [32]byte
	if _, err := crand.Read(payAddr[:]); err != nil {
		panic(err)
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, pid, err := generatePaymentWithPreimage(
		invoiceAmt, htlcAmt, timelock, blob,
		nil, rhash, payAddr,
	)
	if err != nil {
		paymentErr <- err
		return paymentErr
	}

	// Check who is last in the route and add invoice to server registry.
	if err := receiver.registry.AddInvoice(
		context.Background(), *invoice, rhash,
	); err != nil {
		paymentErr <- err
		return paymentErr
	}

	// Send payment and expose err channel.
	err = sender.htlcSwitch.SendHTLC(firstHop, pid, htlc)
	if err != nil {
		paymentErr <- err
		return paymentErr
	}

	go func() {
		resultChan, err := sender.htlcSwitch.GetAttemptResult(
			pid, rhash, newMockDeobfuscator(),
		)
		if err != nil {
			paymentErr <- err
			return
		}

		result, ok := <-resultChan
		if !ok {
			paymentErr <- fmt.Errorf("shutting down")
			return
		}

		if result.Error != nil {
			paymentErr <- result.Error
			return
		}
		paymentErr <- nil
	}()

	return paymentErr
}

// waitLinksEligible blocks until all links the provided name-to-link map are
// eligible to forward HTLCs.
func waitLinksEligible(links map[string]*channelLink) error {
	return wait.NoError(func() error {
		for name, link := range links {
			if link.EligibleToForward() {
				continue
			}
			return fmt.Errorf("%s channel link not eligible", name)
		}
		return nil
	}, 3*time.Second)
}

// timeout implements a test level timeout.
func timeout() func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(20 * time.Second):
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

			panic("test timeout")
		case <-done:
		}
	}()

	return func() {
		close(done)
	}
}
