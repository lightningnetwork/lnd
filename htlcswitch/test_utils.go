package htlcswitch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/fastsha256"
	"github.com/coreos/bbolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/ticker"
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

	// testTx is used as the default funding txn for single-funder channels.
	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}
)

var idSeqNum uint64

func genIDs() (lnwire.ChannelID, lnwire.ChannelID, lnwire.ShortChannelID,
	lnwire.ShortChannelID) {

	id := atomic.AddUint64(&idSeqNum, 2)

	var scratch [8]byte

	binary.BigEndian.PutUint64(scratch[:], id)
	hash1, _ := chainhash.NewHash(bytes.Repeat(scratch[:], 4))

	binary.BigEndian.PutUint64(scratch[:], id+1)
	hash2, _ := chainhash.NewHash(bytes.Repeat(scratch[:], 4))

	chanPoint1 := wire.NewOutPoint(hash1, uint32(id))
	chanPoint2 := wire.NewOutPoint(hash2, uint32(id+1))

	chanID1 := lnwire.NewChanIDFromOutPoint(chanPoint1)
	chanID2 := lnwire.NewChanIDFromOutPoint(chanPoint2)

	aliceChanID := lnwire.NewShortChanIDFromInt(id)
	bobChanID := lnwire.NewShortChanIDFromInt(id + 1)

	return chanID1, chanID2, aliceChanID, bobChanID
}

// mockGetChanUpdateMessage helper function which returns topology update of
// the channel
func mockGetChanUpdateMessage(cid lnwire.ShortChannelID) (*lnwire.ChannelUpdate, error) {
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
	aliceAmount, bobAmount, aliceReserve, bobReserve btcutil.Amount,
	chanID lnwire.ShortChannelID) (*lnwallet.LightningChannel, *lnwallet.LightningChannel, func(),
	func() (*lnwallet.LightningChannel, *lnwallet.LightningChannel,
		error), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), alicePrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(), bobPrivKey)

	channelCapacity := aliceAmount + bobAmount
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	aliceConstraints := &channeldb.ChannelConstraints{
		DustLimit: btcutil.Amount(200),
		MaxPendingAmount: lnwire.NewMSatFromSatoshis(
			channelCapacity),
		ChanReserve:      aliceReserve,
		MinHTLC:          0,
		MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
		CsvDelay:         uint16(csvTimeoutAlice),
	}

	bobConstraints := &channeldb.ChannelConstraints{
		DustLimit: btcutil.Amount(800),
		MaxPendingAmount: lnwire.NewMSatFromSatoshis(
			channelCapacity),
		ChanReserve:      bobReserve,
		MinHTLC:          0,
		MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
		CsvDelay:         uint16(csvTimeoutBob),
	}

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
		ChannelConstraints: *aliceConstraints,
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
		ChannelConstraints: *bobConstraints,
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
		return nil, nil, nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

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

	estimator := lnwallet.NewStaticFeeEstimator(6000, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, nil, nil, nil, err
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
		ChanType:                channeldb.SingleFunder,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		ShortChannelID:          chanID,
		Db:                      dbAlice,
		Packager:                channeldb.NewChannelPackager(chanID),
		FundingTxn:              testTx,
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
		ShortChannelID:          chanID,
		Db:                      dbBob,
		Packager:                channeldb.NewChannelPackager(chanID),
	}

	if err := aliceChannelState.SyncPending(bobAddr, broadcastHeight); err != nil {
		return nil, nil, nil, nil, err
	}

	if err := bobChannelState.SyncPending(aliceAddr, broadcastHeight); err != nil {
		return nil, nil, nil, nil, err
	}

	cleanUpFunc := func() {
		dbAlice.Close()
		dbBob.Close()
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	pCache := &mockPreimageCache{
		// hash -> preimage
		preimageMap: make(map[[32]byte][]byte),
	}

	alicePool := lnwallet.NewSigPool(runtime.NumCPU(), aliceSigner)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, pCache, aliceChannelState, alicePool,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	alicePool.Start()

	bobPool := lnwallet.NewSigPool(runtime.NumCPU(), bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, pCache, bobChannelState, bobPool,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bobPool.Start()

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
		switch err {
		case nil:
		case bbolt.ErrDatabaseNotOpen:
			dbAlice, err = channeldb.Open(dbAlice.Path())
			if err != nil {
				return nil, nil, errors.Errorf("unable to reopen alice "+
					"db: %v", err)
			}

			aliceStoredChannels, err = dbAlice.FetchOpenChannels(aliceKeyPub)
			if err != nil {
				return nil, nil, errors.Errorf("unable to fetch alice "+
					"channel: %v", err)
			}
		default:
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

		newAliceChannel, err := lnwallet.NewLightningChannel(
			aliceSigner, nil, aliceStoredChannel, alicePool,
		)
		if err != nil {
			return nil, nil, errors.Errorf("unable to create new channel: %v",
				err)
		}

		bobStoredChannels, err := dbBob.FetchOpenChannels(bobKeyPub)
		switch err {
		case nil:
		case bbolt.ErrDatabaseNotOpen:
			dbBob, err = channeldb.Open(dbBob.Path())
			if err != nil {
				return nil, nil, errors.Errorf("unable to reopen bob "+
					"db: %v", err)
			}

			bobStoredChannels, err = dbBob.FetchOpenChannels(bobKeyPub)
			if err != nil {
				return nil, nil, errors.Errorf("unable to fetch bob "+
					"channel: %v", err)
			}
		default:
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

		newBobChannel, err := lnwallet.NewLightningChannel(
			bobSigner, nil, bobStoredChannel, bobPool,
		)
		if err != nil {
			return nil, nil, errors.Errorf("unable to create new channel: %v",
				err)
		}
		return newAliceChannel, newBobChannel, nil
	}

	return channelAlice, channelBob, cleanUpFunc, restore, nil
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

	bobServer            *mockServer
	firstBobChannelLink  *channelLink
	secondBobChannelLink *channelLink

	carolServer      *mockServer
	carolChannelLink *channelLink

	feeEstimator *mockFeeEstimator

	globalPolicy ForwardingPolicy
}

// generateHops creates the per hop payload, the total amount to be sent, and
// also the time lock value needed to route an HTLC with the target amount over
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

		var timeLock uint32
		// If this is the last, hop, then the time lock will be their
		// specified delta policy plus our starting height.
		if i == len(path)-1 {
			totalTimelock += lastHop.cfg.FwrdingPolicy.TimeLockDelta
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

// makePayment takes the destination node and amount as input, sends the
// payment and returns the error channel to wait for error to be received and
// invoice in order to check its status after the payment finished.
//
// With this function you can send payments:
// * from Alice to Bob
// * from Alice to Carol through the Bob
// * from Alice to some another peer through the Bob
func makePayment(sendingPeer, receivingPeer lnpeer.Peer,
	firstHop lnwire.ShortChannelID, hops []ForwardingInfo,
	invoiceAmt, htlcAmt lnwire.MilliSatoshi,
	timelock uint32) *paymentResponse {

	paymentErr := make(chan error, 1)

	var rhash lntypes.Hash

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
		_, err := sender.htlcSwitch.SendHTLC(
			firstHop, htlc, newMockDeobfuscator(),
		)
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

	_, _, firstChanID, secondChanID := genIDs()

	// Create lightning channels between Alice<->Bob and Bob<->Carol
	aliceChannel, firstBobChannel, cleanAliceBob, restoreAliceBob, err :=
		createTestChannel(alicePrivKey, bobPrivKey, aliceToBob,
			aliceToBob, 0, 0, firstChanID)
	if err != nil {
		return nil, nil, nil, errors.Errorf("unable to create "+
			"alice<->bob channel: %v", err)
	}

	secondBobChannel, carolChannel, cleanBobCarol, restoreBobCarol, err :=
		createTestChannel(bobPrivKey, carolPrivKey, bobToCarol,
			bobToCarol, 0, 0, secondChanID)
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

	aliceDb := aliceChannel.State().Db
	bobDb := firstBobChannel.State().Db
	carolDb := carolChannel.State().Db

	defaultDelta := uint32(6)

	// Create three peers/servers.
	aliceServer, err := newMockServer(
		t, "alice", startingHeight, aliceDb, defaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobServer, err := newMockServer(
		t, "bob", startingHeight, bobDb, defaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}
	carolServer, err := newMockServer(
		t, "carol", startingHeight, carolDb, defaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create carol server: %v", err)
	}

	// Create mock decoder instead of sphinx one in order to mock the route
	// which htlc should follow.
	aliceDecoder := newMockIteratorDecoder()
	bobDecoder := newMockIteratorDecoder()
	carolDecoder := newMockIteratorDecoder()

	feeEstimator := &mockFeeEstimator{
		byteFeeIn: make(chan lnwallet.SatPerKWeight),
		quit:      make(chan struct{}),
	}

	const (
		batchTimeout        = 50 * time.Millisecond
		fwdPkgTimeout       = 15 * time.Second
		minFeeUpdateTimeout = 30 * time.Minute
		maxFeeUpdateTimeout = 40 * time.Minute
	)

	pCache := &mockPreimageCache{
		// hash -> preimage
		preimageMap: make(map[[32]byte][]byte),
	}

	globalPolicy := ForwardingPolicy{
		MinHTLC:       lnwire.NewMSatFromSatoshis(5),
		BaseFee:       lnwire.NewMSatFromSatoshis(1),
		TimeLockDelta: defaultDelta,
	}
	obfuscator := NewMockObfuscator()

	aliceChannelLink := NewChannelLink(
		ChannelLinkConfig{
			Switch:             aliceServer.htlcSwitch,
			FwrdingPolicy:      globalPolicy,
			Peer:               bobServer,
			Circuits:           aliceServer.htlcSwitch.CircuitModifier(),
			ForwardPackets:     aliceServer.htlcSwitch.ForwardPackets,
			DecodeHopIterators: aliceDecoder.DecodeHopIterators,
			ExtractErrorEncrypter: func(*btcec.PublicKey) (
				ErrorEncrypter, lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               aliceServer.registry,
			FeeEstimator:           feeEstimator,
			PreimageCache:          pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents:         &contractcourt.ChainEventSubscription{},
			SyncStates:          true,
			BatchSize:           10,
			BatchTicker:         ticker.MockNew(batchTimeout),
			FwdPkgGCTicker:      ticker.MockNew(fwdPkgTimeout),
			MinFeeUpdateTimeout: minFeeUpdateTimeout,
			MaxFeeUpdateTimeout: maxFeeUpdateTimeout,
			OnChannelFailure:    func(lnwire.ChannelID, lnwire.ShortChannelID, LinkFailureError) {},
		},
		aliceChannel,
	)
	if err := aliceServer.htlcSwitch.AddLink(aliceChannelLink); err != nil {
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

	firstBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			Switch:             bobServer.htlcSwitch,
			FwrdingPolicy:      globalPolicy,
			Peer:               aliceServer,
			Circuits:           bobServer.htlcSwitch.CircuitModifier(),
			ForwardPackets:     bobServer.htlcSwitch.ForwardPackets,
			DecodeHopIterators: bobDecoder.DecodeHopIterators,
			ExtractErrorEncrypter: func(*btcec.PublicKey) (
				ErrorEncrypter, lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               bobServer.registry,
			FeeEstimator:           feeEstimator,
			PreimageCache:          pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents:         &contractcourt.ChainEventSubscription{},
			SyncStates:          true,
			BatchSize:           10,
			BatchTicker:         ticker.MockNew(batchTimeout),
			FwdPkgGCTicker:      ticker.MockNew(fwdPkgTimeout),
			MinFeeUpdateTimeout: minFeeUpdateTimeout,
			MaxFeeUpdateTimeout: maxFeeUpdateTimeout,
			OnChannelFailure:    func(lnwire.ChannelID, lnwire.ShortChannelID, LinkFailureError) {},
		},
		firstBobChannel,
	)
	if err := bobServer.htlcSwitch.AddLink(firstBobChannelLink); err != nil {
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

	secondBobChannelLink := NewChannelLink(
		ChannelLinkConfig{
			Switch:             bobServer.htlcSwitch,
			FwrdingPolicy:      globalPolicy,
			Peer:               carolServer,
			Circuits:           bobServer.htlcSwitch.CircuitModifier(),
			ForwardPackets:     bobServer.htlcSwitch.ForwardPackets,
			DecodeHopIterators: bobDecoder.DecodeHopIterators,
			ExtractErrorEncrypter: func(*btcec.PublicKey) (
				ErrorEncrypter, lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               bobServer.registry,
			FeeEstimator:           feeEstimator,
			PreimageCache:          pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents:         &contractcourt.ChainEventSubscription{},
			SyncStates:          true,
			BatchSize:           10,
			BatchTicker:         ticker.MockNew(batchTimeout),
			FwdPkgGCTicker:      ticker.MockNew(fwdPkgTimeout),
			MinFeeUpdateTimeout: minFeeUpdateTimeout,
			MaxFeeUpdateTimeout: maxFeeUpdateTimeout,
			OnChannelFailure:    func(lnwire.ChannelID, lnwire.ShortChannelID, LinkFailureError) {},
		},
		secondBobChannel,
	)
	if err := bobServer.htlcSwitch.AddLink(secondBobChannelLink); err != nil {
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

	carolChannelLink := NewChannelLink(
		ChannelLinkConfig{
			Switch:             carolServer.htlcSwitch,
			FwrdingPolicy:      globalPolicy,
			Peer:               bobServer,
			Circuits:           carolServer.htlcSwitch.CircuitModifier(),
			ForwardPackets:     carolServer.htlcSwitch.ForwardPackets,
			DecodeHopIterators: carolDecoder.DecodeHopIterators,
			ExtractErrorEncrypter: func(*btcec.PublicKey) (
				ErrorEncrypter, lnwire.FailCode) {
				return obfuscator, lnwire.CodeNone
			},
			FetchLastChannelUpdate: mockGetChanUpdateMessage,
			Registry:               carolServer.registry,
			FeeEstimator:           feeEstimator,
			PreimageCache:          pCache,
			UpdateContractSignals: func(*contractcourt.ContractSignals) error {
				return nil
			},
			ChainEvents:         &contractcourt.ChainEventSubscription{},
			SyncStates:          true,
			BatchSize:           10,
			BatchTicker:         ticker.MockNew(batchTimeout),
			FwdPkgGCTicker:      ticker.MockNew(fwdPkgTimeout),
			MinFeeUpdateTimeout: minFeeUpdateTimeout,
			MaxFeeUpdateTimeout: maxFeeUpdateTimeout,
			OnChannelFailure:    func(lnwire.ChannelID, lnwire.ShortChannelID, LinkFailureError) {},
		},
		carolChannel,
	)
	if err := carolServer.htlcSwitch.AddLink(carolChannelLink); err != nil {
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

		bobServer:            bobServer,
		firstBobChannelLink:  firstBobChannelLink.(*channelLink),
		secondBobChannelLink: secondBobChannelLink.(*channelLink),

		carolServer:      carolServer,
		carolChannelLink: carolChannelLink.(*channelLink),

		feeEstimator: feeEstimator,
		globalPolicy: globalPolicy,
	}
}
