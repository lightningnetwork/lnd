package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil/hdkeychain"

	"github.com/lightningnetwork/lnd/shachain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

const (
	// The size of the buffered queue of requests to the wallet from the
	// outside word.
	msgBufferSize = 100

	// revocationRootIndex is the top level HD key index from which secrets
	// used to generate producer roots should be derived from.
	revocationRootIndex = hdkeychain.HardenedKeyStart + 1

	// identityKeyIndex is the top level HD key index which is used to
	// generate/rotate identity keys.
	//
	// TODO(roasbeef): should instead be child to make room for future
	// rotations, etc.
	identityKeyIndex = hdkeychain.HardenedKeyStart + 2

	commitWeight int64 = 724
	htlcWeight   int64 = 172
)

var (
	// Namespace bucket keys.
	lightningNamespaceKey = []byte("ln-wallet")
	waddrmgrNamespaceKey  = []byte("waddrmgr")
	wtxmgrNamespaceKey    = []byte("wtxmgr")
)

// ErrInsufficientFunds is a type matching the error interface which is
// returned when coin selection for a new funding transaction fails to due
// having an insufficient amount of confirmed funds.
type ErrInsufficientFunds struct {
	amountAvailable btcutil.Amount
	amountSelected  btcutil.Amount
}

func (e *ErrInsufficientFunds) Error() string {
	return fmt.Sprintf("not enough outputs to create funding transaction,"+
		" need %v only have %v  available", e.amountAvailable,
		e.amountSelected)
}

// initFundingReserveReq is the first message sent to initiate the workflow
// required to open a payment channel with a remote peer. The initial required
// parameters are configurable across channels. These parameters are to be
// chosen depending on the fee climate within the network, and time value of
// funds to be locked up within the channel. Upon success a ChannelReservation
// will be created in order to track the lifetime of this pending channel.
// Outputs selected will be 'locked', making them unavailable, for any other
// pending reservations. Therefore, all channels in reservation limbo will be
// periodically after a timeout period in order to avoid "exhaustion" attacks.
//
// TODO(roasbeef): zombie reservation sweeper goroutine.
type initFundingReserveMsg struct {
	// chainHash denotes that chain to be used to ultimately open the
	// target channel.
	chainHash *chainhash.Hash

	// nodeId is the ID of the remote node we would like to open a channel
	// with.
	nodeID *btcec.PublicKey

	// nodeAddr is the IP address plus port that we used to either
	// establish or accept the connection which led to the negotiation of
	// this funding workflow.
	nodeAddr *net.TCPAddr

	// fundingAmount is the amount of funds requested for this channel.
	fundingAmount btcutil.Amount

	// capacity is the total capacity of the channel which includes the
	// amount of funds the remote party contributes (if any).
	capacity btcutil.Amount

	// feePerKw is the accepted satoshis/Kw fee for the funding
	// transaction. In order to ensure timely confirmation, it is
	// recommended that this fee should be generous, paying some multiple
	// of the accepted base fee rate of the network.
	feePerKw btcutil.Amount

	// pushMSat is the number of milli-satoshis that should be pushed over
	// the responder as part of the initial channel creation.
	pushMSat lnwire.MilliSatoshi

	// err is a channel in which all errors will be sent across. Will be
	// nil if this initial set is successful.
	//
	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error

	// resp is channel in which a ChannelReservation with our contributions
	// filled in will be sent across this channel in the case of a
	// successfully reservation initiation. In the case of an error, this
	// will read a nil pointer.
	//
	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	resp chan *ChannelReservation
}

// fundingReserveCancelMsg is a message reserved for cancelling an existing
// channel reservation identified by its reservation ID. Cancelling a reservation
// frees its locked outputs up, for inclusion within further reservations.
type fundingReserveCancelMsg struct {
	pendingFundingID uint64

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error // Buffered
}

// addContributionMsg represents a message executing the second phase of the
// channel reservation workflow. This message carries the counterparty's
// "contribution" to the payment channel. In the case that this message is
// processed without generating any errors, then channel reservation will then
// be able to construct the funding tx, both commitment transactions, and
// finally generate signatures for all our inputs to the funding transaction,
// and for the remote node's version of the commitment transaction.
type addContributionMsg struct {
	pendingFundingID uint64

	// TODO(roasbeef): Should also carry SPV proofs in we're in SPV mode
	contribution *ChannelContribution

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// addSingleContributionMsg represents a message executing the second phase of
// a single funder channel reservation workflow. This messages carries the
// counterparty's "contribution" to the payment channel. As this message is
// sent when on the responding side to a single funder workflow, no further
// action apart from storing the provided contribution is carried out.
type addSingleContributionMsg struct {
	pendingFundingID uint64

	contribution *ChannelContribution

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// addCounterPartySigsMsg represents the final message required to complete,
// and 'open' a payment channel. This message carries the counterparty's
// signatures for each of their inputs to the funding transaction, and also a
// signature allowing us to spend our version of the commitment transaction.
// If we're able to verify all the signatures are valid, the funding transaction
// will be broadcast to the network. After the funding transaction gains a
// configurable number of confirmations, the channel is officially considered
// 'open'.
type addCounterPartySigsMsg struct {
	pendingFundingID uint64

	// Should be order of sorted inputs that are theirs. Sorting is done
	// in accordance to BIP-69:
	// https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	theirFundingInputScripts []*InputScript

	// This should be 1/2 of the signatures needed to succesfully spend our
	// version of the commitment transaction.
	theirCommitmentSig []byte

	// This channel is used to return the completed channel after the wallet
	// has completed all of its stages in the funding process.
	completeChan chan *channeldb.OpenChannel

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// addSingleFunderSigsMsg represents the next-to-last message required to
// complete a single-funder channel workflow. Once the initiator is able to
// construct the funding transaction, they send both the outpoint and a
// signature for our version of the commitment transaction. Once this message
// is processed we (the responder) are able to construct both commitment
// transactions, signing the remote party's version.
type addSingleFunderSigsMsg struct {
	pendingFundingID uint64

	// fundingOutpoint is the outpoint of the completed funding
	// transaction as assembled by the workflow initiator.
	fundingOutpoint *wire.OutPoint

	// theirCommitmentSig are the 1/2 of the signatures needed to
	// succesfully spend our version of the commitment transaction.
	theirCommitmentSig []byte

	// This channel is used to return the completed channel after the wallet
	// has completed all of its stages in the funding process.
	completeChan chan *channeldb.OpenChannel

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// LightningWallet is a domain specific, yet general Bitcoin wallet capable of
// executing workflow required to interact with the Lightning Network. It is
// domain specific in the sense that it understands all the fancy scripts used
// within the Lightning Network, channel lifetimes, etc. However, it embedds a
// general purpose Bitcoin wallet within it. Therefore, it is also able to
// serve as a regular Bitcoin wallet which uses HD keys. The wallet is highly
// concurrent internally. All communication, and requests towards the wallet
// are dispatched as messages over channels, ensuring thread safety across all
// operations. Interaction has been designed independent of any peer-to-peer
// communication protocol, allowing the wallet to be self-contained and
// embeddable within future projects interacting with the Lightning Network.
//
// NOTE: At the moment the wallet requires a btcd full node, as it's dependent
// on btcd's websockets notifications as even triggers during the lifetime of a
// channel. However, once the chainntnfs package is complete, the wallet will
// be compatible with multiple RPC/notification services such as Electrum,
// Bitcoin Core + ZeroMQ, etc. Eventually, the wallet won't require a full-node
// at all, as SPV support is integrated into btcwallet.
type LightningWallet struct {
	// Cfg is the configuration struct that will be used by the wallet to
	// access the necessary interfaces and default it needs to carry on its
	// duties.
	Cfg Config

	// WalletController is the core wallet, all non Lightning Network
	// specific interaction is proxied to the internal wallet.
	WalletController

	// This mutex is to be held when generating external keys to be used as
	// multi-sig, and commitment keys within the channel.
	keyGenMtx sync.RWMutex

	// This mutex MUST be held when performing coin selection in order to
	// avoid inadvertently creating multiple funding transaction which
	// double spend inputs across each other.
	coinSelectMtx sync.RWMutex

	// rootKey is the root HD key derived from a WalletController private
	// key. This rootKey is used to derive all LN specific secrets.
	rootKey *hdkeychain.ExtendedKey

	// All messages to the wallet are to be sent across this channel.
	msgChan chan interface{}

	// Incomplete payment channels are stored in the map below. An intent
	// to create a payment channel is tracked as a "reservation" within
	// limbo. Once the final signatures have been exchanged, a reservation
	// is removed from limbo. Each reservation is tracked by a unique
	// monotonically integer. All requests concerning the channel MUST
	// carry a valid, active funding ID.
	fundingLimbo  map[uint64]*ChannelReservation
	nextFundingID uint64
	limboMtx      sync.RWMutex
	// TODO(roasbeef): zombie garbage collection routine to solve
	// lost-object/starvation problem/attack.

	// lockedOutPoints is a set of the currently locked outpoint. This
	// information is kept in order to provide an easy way to unlock all
	// the currently locked outpoints.
	lockedOutPoints map[wire.OutPoint]struct{}

	started  int32
	shutdown int32
	quit     chan struct{}

	wg sync.WaitGroup

	// TODO(roasbeef): handle wallet lock/unlock
}

// NewLightningWallet creates/opens and initializes a LightningWallet instance.
// If the wallet has never been created (according to the passed dataDir), first-time
// setup is executed.
func NewLightningWallet(Cfg Config) (*LightningWallet, error) {

	return &LightningWallet{
		Cfg:              Cfg,
		WalletController: Cfg.WalletController,
		msgChan:          make(chan interface{}, msgBufferSize),
		nextFundingID:    0,
		fundingLimbo:     make(map[uint64]*ChannelReservation),
		lockedOutPoints:  make(map[wire.OutPoint]struct{}),
		quit:             make(chan struct{}),
	}, nil
}

// Startup establishes a connection to the RPC source, and spins up all
// goroutines required to handle incoming messages.
func (l *LightningWallet) Startup() error {
	// Already started?
	if atomic.AddInt32(&l.started, 1) != 1 {
		return nil
	}

	// Start the underlying wallet controller.
	if err := l.Start(); err != nil {
		return err
	}

	// Fetch the root derivation key from the wallet's HD chain. We'll use
	// this to generate specific Lightning related secrets on the fly.
	rootKey, err := l.FetchRootKey()
	if err != nil {
		return err
	}

	// TODO(roasbeef): always re-derive on the fly?
	rootKeyRaw := rootKey.Serialize()
	l.rootKey, err = hdkeychain.NewMaster(rootKeyRaw, &l.Cfg.NetParams)
	if err != nil {
		return err
	}

	l.wg.Add(1)
	// TODO(roasbeef): multiple request handlers?
	go l.requestHandler()

	return nil
}

// Shutdown gracefully stops the wallet, and all active goroutines.
func (l *LightningWallet) Shutdown() error {
	if atomic.AddInt32(&l.shutdown, 1) != 1 {
		return nil
	}

	// Signal the underlying wallet controller to shutdown, waiting until
	// all active goroutines have been shutdown.
	if err := l.Stop(); err != nil {
		return err
	}

	close(l.quit)
	l.wg.Wait()
	return nil
}

// LockedOutpoints returns a list of all currently locked outpoint.
func (l *LightningWallet) LockedOutpoints() []*wire.OutPoint {
	outPoints := make([]*wire.OutPoint, 0, len(l.lockedOutPoints))
	for outPoint := range l.lockedOutPoints {
		outPoints = append(outPoints, &outPoint)
	}

	return outPoints
}

// ResetReservations reset the volatile wallet state which trakcs all currently
// active reservations.
func (l *LightningWallet) ResetReservations() {
	l.nextFundingID = 0
	l.fundingLimbo = make(map[uint64]*ChannelReservation)

	for outpoint := range l.lockedOutPoints {
		l.UnlockOutpoint(outpoint)
	}
	l.lockedOutPoints = make(map[wire.OutPoint]struct{})
}

// ActiveReservations returns a slice of all the currently active
// (non-cancalled) reservations.
func (l *LightningWallet) ActiveReservations() []*ChannelReservation {
	reservations := make([]*ChannelReservation, 0, len(l.fundingLimbo))
	for _, reservation := range l.fundingLimbo {
		reservations = append(reservations, reservation)
	}

	return reservations
}

// GetIdentitykey returns the identity private key of the wallet.
// TODO(roasbeef): should be moved elsewhere
func (l *LightningWallet) GetIdentitykey() (*btcec.PrivateKey, error) {
	identityKey, err := l.rootKey.Child(identityKeyIndex)
	if err != nil {
		return nil, err
	}

	return identityKey.ECPrivKey()
}

// requestHandler is the primary goroutine(s) responsible for handling, and
// dispatching relies to all messages.
func (l *LightningWallet) requestHandler() {
out:
	for {
		select {
		case m := <-l.msgChan:
			switch msg := m.(type) {
			case *initFundingReserveMsg:
				l.handleFundingReserveRequest(msg)
			case *fundingReserveCancelMsg:
				l.handleFundingCancelRequest(msg)
			case *addSingleContributionMsg:
				l.handleSingleContribution(msg)
			case *addContributionMsg:
				l.handleContributionMsg(msg)
			case *addSingleFunderSigsMsg:
				l.handleSingleFunderSigs(msg)
			case *addCounterPartySigsMsg:
				l.handleFundingCounterPartySigs(msg)
			}
		case <-l.quit:
			// TODO: do some clean up
			break out
		}
	}

	l.wg.Done()
}

// InitChannelReservation kicks off the 3-step workflow required to successfully
// open a payment channel with a remote node. As part of the funding
// reservation, the inputs selected for the funding transaction are 'locked'.
// This ensures that multiple channel reservations aren't double spending the
// same inputs in the funding transaction. If reservation initialization is
// successful, a ChannelReservation containing our completed contribution is
// returned. Our contribution contains all the items necessary to allow the
// counterparty to build the funding transaction, and both versions of the
// commitment transaction. Otherwise, an error occurred a nil pointer along with
// an error are returned.
//
// Once a ChannelReservation has been obtained, two additional steps must be
// processed before a payment channel can be considered 'open'. The second step
// validates, and processes the counterparty's channel contribution. The third,
// and final step verifies all signatures for the inputs of the funding
// transaction, and that the signature we records for our version of the
// commitment transaction is valid.
func (l *LightningWallet) InitChannelReservation(
	capacity, ourFundAmt btcutil.Amount, pushMSat lnwire.MilliSatoshi,
	feePerKw btcutil.Amount,
	theirID *btcec.PublicKey, theirAddr *net.TCPAddr,
	chainHash *chainhash.Hash) (*ChannelReservation, error) {

	errChan := make(chan error, 1)
	respChan := make(chan *ChannelReservation, 1)

	l.msgChan <- &initFundingReserveMsg{
		chainHash:     chainHash,
		nodeID:        theirID,
		nodeAddr:      theirAddr,
		fundingAmount: ourFundAmt,
		capacity:      capacity,
		feePerKw:      feePerKw,
		pushMSat:      pushMSat,
		err:           errChan,
		resp:          respChan,
	}

	return <-respChan, <-errChan
}

// handleFundingReserveRequest processes a message intending to create, and
// validate a funding reservation request.
func (l *LightningWallet) handleFundingReserveRequest(req *initFundingReserveMsg) {
	// It isn't possible to create a channel with zero funds committed.
	if req.fundingAmount+req.capacity == 0 {
		req.err <- fmt.Errorf("cannot have channel with zero " +
			"satoshis funded")
		req.resp <- nil
		return
	}

	// If the funding request is for a different chain than the one the
	// wallet is aware of, then we'll reject the request.
	if !bytes.Equal(l.Cfg.NetParams.GenesisHash[:], req.chainHash[:]) {
		req.err <- fmt.Errorf("unable to create channel reservation "+
			"for chain=%v, wallet is on chain=%v",
			req.chainHash, l.Cfg.NetParams.GenesisHash)
		req.resp <- nil
		return
	}

	id := atomic.AddUint64(&l.nextFundingID, 1)
	reservation := NewChannelReservation(req.capacity, req.fundingAmount,
		req.feePerKw, l, id, req.pushMSat, l.Cfg.NetParams.GenesisHash)

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	reservation.Lock()
	defer reservation.Unlock()

	reservation.nodeAddr = req.nodeAddr
	reservation.partialState.IdentityPub = req.nodeID

	// If we're on the receiving end of a single funder channel then we
	// don't need to perform any coin selection. Otherwise, attempt to
	// obtain enough coins to meet the required funding amount.
	if req.fundingAmount != 0 {
		// Coin selection is done on the basis of sat-per-byte, so
		// we'll query the fee estimator for a fee to use to ensure the
		// funding transaction gets into the _next_  block.
		//
		// TODO(roasbeef): shouldn't be targeting next block
		satPerByte := l.Cfg.FeeEstimator.EstimateFeePerByte(1)
		err := l.selectCoinsAndChange(satPerByte, req.fundingAmount,
			reservation.ourContribution)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}
	}

	// Next, we'll grab a series of keys from the wallet which will be used
	// for the duration of the channel. The keys include: our multi-sig
	// key, the base revocation key, the base payment key, and the delayed
	// payment key.
	var err error
	reservation.ourContribution.MultiSigKey, err = l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.ourContribution.RevocationBasePoint, err = l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.ourContribution.PaymentBasePoint, err = l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.ourContribution.DelayBasePoint, err = l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// With the above keys created, we'll also need to initialization our
	// initial revocation tree state. In order to do so in a deterministic
	// manner (for recovery purposes), we'll use the current block hash
	// along with the identity public key of the node we're creating the
	// channel with. In the event of a recovery, given these two items and
	// the initialize wallet HD seed, we can derive all of our revocation
	// secrets.
	masterElkremRoot, err := l.deriveMasterRevocationRoot()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	bestHash, _, err := l.Cfg.ChainIO.GetBestBlock()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	revocationRoot := DeriveRevocationRoot(masterElkremRoot, *bestHash,
		req.nodeID)

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	producer := shachain.NewRevocationProducer(revocationRoot)
	firstPreimage, err := producer.AtIndex(0)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.ourContribution.FirstCommitmentPoint = ComputeCommitmentPoint(
		firstPreimage[:],
	)

	reservation.partialState.RevocationProducer = producer
	reservation.ourContribution.ChannelConstraints = l.Cfg.DefaultConstraints

	// TODO(roasbeef): turn above into: initContributio()

	// Create a limbo and record entry for this newly pending funding
	// request.
	l.limboMtx.Lock()
	l.fundingLimbo[id] = reservation
	l.limboMtx.Unlock()

	// Funding reservation request successfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or cancelled.
	req.resp <- reservation
	req.err <- nil
}

// handleFundingReserveCancel cancels an existing channel reservation. As part
// of the cancellation, outputs previously selected as inputs for the funding
// transaction via coin selection are freed allowing future reservations to
// include them.
func (l *LightningWallet) handleFundingCancelRequest(req *fundingReserveCancelMsg) {
	// TODO(roasbeef): holding lock too long
	l.limboMtx.Lock()
	defer l.limboMtx.Unlock()

	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	if !ok {
		// TODO(roasbeef): make new error, "unkown funding state" or something
		req.err <- fmt.Errorf("attempted to cancel non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Mark all previously locked outpoints as usuable for future funding
	// requests.
	for _, unusedInput := range pendingReservation.ourContribution.Inputs {
		delete(l.lockedOutPoints, unusedInput.PreviousOutPoint)
		l.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): is it even worth it to keep track of unsed keys?

	// TODO(roasbeef): Is it possible to mark the unused change also as
	// available?

	delete(l.fundingLimbo, req.pendingFundingID)

	req.err <- nil
}

// CreateCommitmentTxns is a helper function that creates the initial
// commitment transaction for both parties. This function is used during the
// initial funding workflow as both sides must generate a signature for the
// remote party's commitment transaction, and verify the signature for their
// version of the commitment transaction.
func CreateCommitmentTxns(localBalance, remoteBalance btcutil.Amount,
	ourChanCfg, theirChanCfg *channeldb.ChannelConfig,
	localCommitPoint, remoteCommitPoint *btcec.PublicKey,
	fundingTxIn *wire.TxIn) (*wire.MsgTx, *wire.MsgTx, error) {

	remoteRevocation := DeriveRevocationPubkey(
		ourChanCfg.RevocationBasePoint,
		remoteCommitPoint,
	)
	localRevocation := DeriveRevocationPubkey(
		theirChanCfg.RevocationBasePoint,
		localCommitPoint,
	)

	remoteDelayKey := TweakPubKey(theirChanCfg.DelayBasePoint,
		remoteCommitPoint)
	localDelayKey := TweakPubKey(ourChanCfg.DelayBasePoint,
		localCommitPoint)

	// The payment keys go on the opposite commitment transaction, so we'll
	// swap the commitment points we use. As in the remote payment key will
	// be used within our commitment transaction, and the local payment key
	// used within the remote commitment transaction.
	remotePaymentKey := TweakPubKey(theirChanCfg.PaymentBasePoint,
		localCommitPoint)
	localPaymentKey := TweakPubKey(ourChanCfg.PaymentBasePoint,
		remoteCommitPoint)

	ourCommitTx, err := CreateCommitTx(fundingTxIn,
		localDelayKey, remotePaymentKey, localRevocation,
		uint32(ourChanCfg.CsvDelay), localBalance, remoteBalance,
		ourChanCfg.DustLimit)
	if err != nil {
		return nil, nil, err
	}

	otxn := btcutil.NewTx(ourCommitTx)
	if err := blockchain.CheckTransactionSanity(otxn); err != nil {
		return nil, nil, err
	}

	theirCommitTx, err := CreateCommitTx(fundingTxIn,
		remoteDelayKey, localPaymentKey, remoteRevocation,
		uint32(theirChanCfg.CsvDelay), remoteBalance, localBalance,
		theirChanCfg.DustLimit)
	if err != nil {
		return nil, nil, err
	}

	ttxn := btcutil.NewTx(theirCommitTx)
	if err := blockchain.CheckTransactionSanity(ttxn); err != nil {
		return nil, nil, err
	}

	return ourCommitTx, theirCommitTx, nil
}

// handleContributionMsg processes the second workflow step for the lifetime of
// a channel reservation. Upon completion, the reservation will carry a
// completed funding transaction (minus the counterparty's input signatures),
// both versions of the commitment transaction, and our signature for their
// version of the commitment transaction.
func (l *LightningWallet) handleContributionMsg(req *addContributionMsg) {

	l.limboMtx.Lock()
	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	l.limboMtx.Unlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Create a blank, fresh transaction. Soon to be a complete funding
	// transaction which will allow opening a lightning channel.
	pendingReservation.fundingTx = wire.NewMsgTx(1)
	fundingTx := pendingReservation.fundingTx

	// Some temporary variables to cut down on the resolution verbosity.
	pendingReservation.theirContribution = req.contribution
	theirContribution := req.contribution
	ourContribution := pendingReservation.ourContribution

	// Add all multi-party inputs and outputs to the transaction.
	for _, ourInput := range ourContribution.Inputs {
		fundingTx.AddTxIn(ourInput)
	}
	for _, theirInput := range theirContribution.Inputs {
		fundingTx.AddTxIn(theirInput)
	}
	for _, ourChangeOutput := range ourContribution.ChangeOutputs {
		fundingTx.AddTxOut(ourChangeOutput)
	}
	for _, theirChangeOutput := range theirContribution.ChangeOutputs {
		fundingTx.AddTxOut(theirChangeOutput)
	}

	ourKey := pendingReservation.ourContribution.MultiSigKey
	theirKey := theirContribution.MultiSigKey

	// Finally, add the 2-of-2 multi-sig output which will set up the lightning
	// channel.
	channelCapacity := int64(pendingReservation.partialState.Capacity)
	witnessScript, multiSigOut, err := GenFundingPkScript(ourKey.SerializeCompressed(),
		theirKey.SerializeCompressed(), channelCapacity)
	if err != nil {
		req.err <- err
		return
	}

	// Sort the transaction. Since both side agree to a canonical ordering,
	// by sorting we no longer need to send the entire transaction. Only
	// signatures will be exchanged.
	fundingTx.AddTxOut(multiSigOut)
	txsort.InPlaceSort(pendingReservation.fundingTx)

	// Next, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	pendingReservation.ourFundingInputScripts = make([]*InputScript, 0,
		len(ourContribution.Inputs))
	signDesc := SignDescriptor{
		HashType:  txscript.SigHashAll,
		SigHashes: txscript.NewTxSigHashes(fundingTx),
	}
	for i, txIn := range fundingTx.TxIn {
		info, err := l.FetchInputInfo(&txIn.PreviousOutPoint)
		if err == ErrNotMine {
			continue
		} else if err != nil {
			req.err <- err
			return
		}

		signDesc.Output = info
		signDesc.InputIndex = i

		inputScript, err := l.Cfg.Signer.ComputeInputScript(fundingTx,
			&signDesc)
		if err != nil {
			req.err <- err
			return
		}

		txIn.SignatureScript = inputScript.ScriptSig
		txIn.Witness = inputScript.Witness
		pendingReservation.ourFundingInputScripts = append(
			pendingReservation.ourFundingInputScripts,
			inputScript,
		)
	}

	// Locate the index of the multi-sig outpoint in order to record it
	// since the outputs are canonically sorted. If this is a single funder
	// workflow, then we'll also need to send this to the remote node.
	fundingTxID := fundingTx.TxHash()
	_, multiSigIndex := FindScriptOutputIndex(fundingTx, multiSigOut.PkScript)
	fundingOutpoint := wire.NewOutPoint(&fundingTxID, multiSigIndex)
	pendingReservation.partialState.FundingOutpoint = *fundingOutpoint

	// Initialize an empty sha-chain for them, tracking the current pending
	// revocation hash (we don't yet know the preimage so we can't add it
	// to the chain).
	s := shachain.NewRevocationStore()
	pendingReservation.partialState.RevocationStore = s

	// Store their current commitment point. We'll need this after the
	// first state transition in order to verify the authenticity of the
	// revocation.
	chanState := pendingReservation.partialState
	chanState.RemoteCurrentRevocation = theirContribution.FirstCommitmentPoint

	// Create the txin to our commitment transaction; required to construct
	// the commitment transactions.
	fundingTxIn := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  fundingTxID,
			Index: multiSigIndex,
		},
	}

	// With the funding tx complete, create both commitment transactions.
	localBalance := pendingReservation.partialState.LocalBalance.ToSatoshis()
	remoteBalance := pendingReservation.partialState.RemoteBalance.ToSatoshis()
	ourCommitTx, theirCommitTx, err := CreateCommitmentTxns(
		localBalance, remoteBalance, ourContribution.ChannelConfig,
		theirContribution.ChannelConfig,
		ourContribution.FirstCommitmentPoint,
		theirContribution.FirstCommitmentPoint, fundingTxIn,
	)
	if err != nil {
		req.err <- err
		return
	}

	// With both commitment transactions constructed, generate the state
	// obsfucator then use it to encode the current state number within
	// both commitment transactions.
	var stateObsfucator [StateHintSize]byte
	if chanState.ChanType == channeldb.SingleFunder {
		stateObsfucator = deriveStateHintObfuscator(
			ourContribution.PaymentBasePoint,
			theirContribution.PaymentBasePoint,
		)
	} else {
		ourSer := ourContribution.PaymentBasePoint.SerializeCompressed()
		theirSer := theirContribution.PaymentBasePoint.SerializeCompressed()
		switch bytes.Compare(ourSer, theirSer) {
		case -1:
			stateObsfucator = deriveStateHintObfuscator(
				ourContribution.PaymentBasePoint,
				theirContribution.PaymentBasePoint,
			)
		default:
			stateObsfucator = deriveStateHintObfuscator(
				theirContribution.PaymentBasePoint,
				ourContribution.PaymentBasePoint,
			)
		}
	}
	err = initStateHints(ourCommitTx, theirCommitTx, stateObsfucator)
	if err != nil {
		req.err <- err
		return
	}

	// Sort both transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourCommitTx)
	txsort.InPlaceSort(theirCommitTx)

	// Record newly available information within the open channel state.
	chanState.FundingOutpoint = *fundingOutpoint
	chanState.CommitTx = *ourCommitTx

	// Generate a signature for their version of the initial commitment
	// transaction.
	signDesc = SignDescriptor{
		WitnessScript: witnessScript,
		PubKey:        ourKey,
		Output:        multiSigOut,
		HashType:      txscript.SigHashAll,
		SigHashes:     txscript.NewTxSigHashes(theirCommitTx),
		InputIndex:    0,
	}
	sigTheirCommit, err := l.Cfg.Signer.SignOutputRaw(theirCommitTx, &signDesc)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.ourCommitmentSig = sigTheirCommit

	req.err <- nil
}

// handleSingleContribution is called as the second step to a single funder
// workflow to which we are the responder. It simply saves the remote peer's
// contribution to the channel, as solely the remote peer will contribute any
// funds to the channel.
func (l *LightningWallet) handleSingleContribution(req *addSingleContributionMsg) {
	l.limboMtx.Lock()
	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	l.limboMtx.Unlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existent funding state")
		return
	}

	// Grab the mutex on the channelReservation to ensure thread-safety.
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// TODO(roasbeef): verify sanity of remote party's parameters, fail if
	// disagree

	// Simply record the counterparty's contribution into the pending
	// reservation data as they'll be solely funding the channel entirely.
	pendingReservation.theirContribution = req.contribution
	theirContribution := pendingReservation.theirContribution
	chanState := pendingReservation.partialState

	// Initialize an empty sha-chain for them, tracking the current pending
	// revocation hash (we don't yet know the preimage so we can't add it
	// to the chain).
	remotePreimageStore := shachain.NewRevocationStore()
	chanState.RevocationStore = remotePreimageStore

	// Now that we've received their first commitment point, we'll store it
	// within the channel state so we can sync it to disk once the funding
	// process is complete.
	chanState.RemoteCurrentRevocation = theirContribution.FirstCommitmentPoint

	req.err <- nil
	return
}

// openChanDetails contains a "finalized" channel which can be considered
// "open" according to the requested confirmation depth at reservation
// initialization. Additionally, the struct contains additional details
// pertaining to the exact location in the main chain in-which the transaction
// was confirmed.
type openChanDetails struct {
	channel     *LightningChannel
	blockHeight uint32
	txIndex     uint32
}

// handleFundingCounterPartySigs is the final step in the channel reservation
// workflow. During this step, we validate *all* the received signatures for
// inputs to the funding transaction. If any of these are invalid, we bail,
// and forcibly cancel this funding request. Additionally, we ensure that the
// signature we received from the counterparty for our version of the commitment
// transaction allows us to spend from the funding output with the addition of
// our signature.
func (l *LightningWallet) handleFundingCounterPartySigs(msg *addCounterPartySigsMsg) {
	l.limboMtx.RLock()
	res, ok := l.fundingLimbo[msg.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		msg.err <- fmt.Errorf("attempted to update non-existent funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	res.Lock()
	defer res.Unlock()

	// Now we can complete the funding transaction by adding their
	// signatures to their inputs.
	res.theirFundingInputScripts = msg.theirFundingInputScripts
	inputScripts := msg.theirFundingInputScripts
	fundingTx := res.fundingTx
	sigIndex := 0
	fundingHashCache := txscript.NewTxSigHashes(fundingTx)
	for i, txin := range fundingTx.TxIn {
		if len(inputScripts) != 0 && len(txin.Witness) == 0 {
			// Attach the input scripts so we can verify it below.
			txin.Witness = inputScripts[sigIndex].Witness
			txin.SignatureScript = inputScripts[sigIndex].ScriptSig

			// Fetch the alleged previous output along with the
			// pkscript referenced by this input.
			// TODO(roasbeef): when dual funder pass actual height-hint
			output, err := l.Cfg.ChainIO.GetUtxo(&txin.PreviousOutPoint, 0)
			if output == nil {
				msg.err <- fmt.Errorf("input to funding tx "+
					"does not exist: %v", err)
				msg.completeChan <- nil
				return
			}

			// Ensure that the witness+sigScript combo is valid.
			vm, err := txscript.NewEngine(output.PkScript,
				fundingTx, i, txscript.StandardVerifyFlags, nil,
				fundingHashCache, output.Value)
			if err != nil {
				msg.err <- fmt.Errorf("cannot create script "+
					"engine: %s", err)
				msg.completeChan <- nil
				return
			}
			if err = vm.Execute(); err != nil {
				msg.err <- fmt.Errorf("cannot validate "+
					"transaction: %s", err)
				msg.completeChan <- nil
				return
			}

			sigIndex++
		}
	}

	// At this point, we can also record and verify their signature for our
	// commitment transaction.
	res.theirCommitmentSig = msg.theirCommitmentSig
	commitTx := res.partialState.CommitTx
	ourKey := res.ourContribution.MultiSigKey
	theirKey := res.theirContribution.MultiSigKey

	// Re-generate both the witnessScript and p2sh output. We sign the
	// witnessScript script, but include the p2sh output as the subscript
	// for verification.
	witnessScript, _, err := GenFundingPkScript(ourKey.SerializeCompressed(),
		theirKey.SerializeCompressed(), int64(res.partialState.Capacity))
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	}

	// Next, create the spending scriptSig, and then verify that the script
	// is complete, allowing us to spend from the funding transaction.
	theirCommitSig := msg.theirCommitmentSig
	channelValue := int64(res.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(&commitTx)
	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, hashCache,
		txscript.SigHashAll, &commitTx, 0, channelValue)
	if err != nil {
		msg.err <- fmt.Errorf("counterparty's commitment signature is "+
			"invalid: %v", err)
		msg.completeChan <- nil
		return
	}

	// Verify that we've received a valid signature from the remote party
	// for our version of the commitment transaction.
	sig, err := btcec.ParseSignature(theirCommitSig, btcec.S256())
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	} else if !sig.Verify(sigHash, theirKey) {
		msg.err <- fmt.Errorf("counterparty's commitment signature is invalid")
		msg.completeChan <- nil
		return
	}
	res.partialState.CommitSig = theirCommitSig

	// Funding complete, this entry can be removed from limbo.
	l.limboMtx.Lock()
	delete(l.fundingLimbo, res.reservationID)
	l.limboMtx.Unlock()

	// As we're about to broadcast the funding transaction, we'll take note
	// of the current height for record keeping purposes.
	//
	// TODO(roasbeef): this info can also be piped into light client's
	// basic fee estimation?
	_, bestHeight, err := l.Cfg.ChainIO.GetBestBlock()
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	}

	// As we've completed the funding process, we'll no convert the
	// contribution structs into their underlying channel config objects to
	// he stored within the database.
	res.partialState.LocalChanCfg = res.ourContribution.toChanConfig()
	res.partialState.RemoteChanCfg = res.theirContribution.toChanConfig()

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	// TODO(roasbeef):
	//  * attempt to retransmit funding transactions on re-start
	nodeAddr := res.nodeAddr
	err = res.partialState.SyncPending(nodeAddr, uint32(bestHeight))
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	}

	walletLog.Infof("Broadcasting funding tx for ChannelPoint(%v): %v",
		res.partialState.FundingOutpoint, spew.Sdump(fundingTx))

	// Broadcast the finalized funding transaction to the network.
	if err := l.PublishTransaction(fundingTx); err != nil {
		// TODO(roasbeef): need to make this into a concrete error
		if !strings.Contains(err.Error(), "already have") {
			msg.err <- err
			msg.completeChan <- nil
			return
		}
	}

	msg.completeChan <- res.partialState
	msg.err <- nil
}

// handleSingleFunderSigs is called once the remote peer who initiated the
// single funder workflow has assembled the funding transaction, and generated
// a signature for our version of the commitment transaction. This method
// progresses the workflow by generating a signature for the remote peer's
// version of the commitment transaction.
func (l *LightningWallet) handleSingleFunderSigs(req *addSingleFunderSigsMsg) {
	l.limboMtx.RLock()
	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existant funding state")
		req.completeChan <- nil
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	chanState := pendingReservation.partialState
	chanState.FundingOutpoint = *req.fundingOutpoint
	fundingTxIn := wire.NewTxIn(req.fundingOutpoint, nil, nil)

	// Now that we have the funding outpoint, we can generate both versions
	// of the commitment transaction, and generate a signature for the
	// remote node's commitment transactions.
	localBalance := pendingReservation.partialState.LocalBalance.ToSatoshis()
	remoteBalance := pendingReservation.partialState.RemoteBalance.ToSatoshis()
	ourCommitTx, theirCommitTx, err := CreateCommitmentTxns(
		localBalance, remoteBalance,
		pendingReservation.ourContribution.ChannelConfig,
		pendingReservation.theirContribution.ChannelConfig,
		pendingReservation.ourContribution.FirstCommitmentPoint,
		pendingReservation.theirContribution.FirstCommitmentPoint,
		fundingTxIn,
	)

	// With both commitment transactions constructed, we can now use the
	// generator state obfuscator to encode the current state number within
	// both commitment transactions.
	stateObsfucator := deriveStateHintObfuscator(
		pendingReservation.theirContribution.PaymentBasePoint,
		pendingReservation.ourContribution.PaymentBasePoint)
	err = initStateHints(ourCommitTx, theirCommitTx, stateObsfucator)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	// Sort both transactions according to the agreed upon canonical
	// ordering. This ensures that both parties sign the same sighash
	// without further synchronization.
	txsort.InPlaceSort(ourCommitTx)
	txsort.InPlaceSort(theirCommitTx)
	chanState.CommitTx = *ourCommitTx

	channelValue := int64(pendingReservation.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(ourCommitTx)
	theirKey := pendingReservation.theirContribution.MultiSigKey
	ourKey := pendingReservation.ourContribution.MultiSigKey
	witnessScript, _, err := GenFundingPkScript(ourKey.SerializeCompressed(),
		theirKey.SerializeCompressed(), channelValue)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, hashCache,
		txscript.SigHashAll, ourCommitTx, 0, channelValue)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	// Verify that we've received a valid signature from the remote party
	// for our version of the commitment transaction.
	sig, err := btcec.ParseSignature(req.theirCommitmentSig, btcec.S256())
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	} else if !sig.Verify(sigHash, theirKey) {
		req.err <- fmt.Errorf("counterparty's commitment signature is invalid")
		req.completeChan <- nil
		return
	}
	chanState.CommitSig = req.theirCommitmentSig

	// With their signature for our version of the commitment transactions
	// verified, we can now generate a signature for their version,
	// allowing the funding transaction to be safely broadcast.
	p2wsh, err := witnessScriptHash(witnessScript)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}
	signDesc := SignDescriptor{
		WitnessScript: witnessScript,
		PubKey:        ourKey,
		Output: &wire.TxOut{
			PkScript: p2wsh,
			Value:    channelValue,
		},
		HashType:   txscript.SigHashAll,
		SigHashes:  txscript.NewTxSigHashes(theirCommitTx),
		InputIndex: 0,
	}
	sigTheirCommit, err := l.Cfg.Signer.SignOutputRaw(theirCommitTx, &signDesc)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}
	pendingReservation.ourCommitmentSig = sigTheirCommit

	_, bestHeight, err := l.Cfg.ChainIO.GetBestBlock()
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	chanState.LocalChanCfg = pendingReservation.ourContribution.toChanConfig()
	chanState.RemoteChanCfg = pendingReservation.theirContribution.toChanConfig()
	err = chanState.SyncPending(pendingReservation.nodeAddr, uint32(bestHeight))
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	req.completeChan <- chanState
	req.err <- nil

	l.limboMtx.Lock()
	delete(l.fundingLimbo, req.pendingFundingID)
	l.limboMtx.Unlock()
}

// selectCoinsAndChange performs coin selection in order to obtain witness
// outputs which sum to at least 'numCoins' amount of satoshis. If coin
// selection is successful/possible, then the selected coins are available
// within the passed contribution's inputs. If necessary, a change address will
// also be generated.
// TODO(roasbeef): remove hardcoded fees and req'd confs for outputs.
func (l *LightningWallet) selectCoinsAndChange(feeRate uint64, amt btcutil.Amount,
	contribution *ChannelContribution) error {

	// We hold the coin select mutex while querying for outputs, and
	// performing coin selection in order to avoid inadvertent double
	// spends across funding transactions.
	l.coinSelectMtx.Lock()
	defer l.coinSelectMtx.Unlock()

	walletLog.Infof("Performing coin selection using %v sat/byte as fee "+
		"rate", feeRate)

	// Find all unlocked unspent witness outputs with greater than 1
	// confirmation.
	// TODO(roasbeef): make num confs a configuration parameter
	coins, err := l.ListUnspentWitness(1)
	if err != nil {
		return err
	}

	// Perform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount
	// requirements.
	selectedCoins, changeAmt, err := coinSelect(feeRate, amt, coins)
	if err != nil {
		return err
	}

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	contribution.Inputs = make([]*wire.TxIn, len(selectedCoins))
	for i, coin := range selectedCoins {
		l.lockedOutPoints[*coin] = struct{}{}
		l.LockOutpoint(*coin)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		contribution.Inputs[i] = wire.NewTxIn(coin, nil, nil)
	}

	// Record any change output(s) generated as a result of the coin
	// selection.
	if changeAmt != 0 {
		changeAddr, err := l.NewAddress(WitnessPubKey, true)
		if err != nil {
			return err
		}
		changeScript, err := txscript.PayToAddrScript(changeAddr)
		if err != nil {
			return err
		}

		contribution.ChangeOutputs = make([]*wire.TxOut, 1)
		contribution.ChangeOutputs[0] = &wire.TxOut{
			Value:    int64(changeAmt),
			PkScript: changeScript,
		}
	}

	return nil
}

// deriveMasterRevocationRoot derives the private key which serves as the master
// producer root. This master secret is used as the secret input to a HKDF to
// generate revocation secrets based on random, but public data.
func (l *LightningWallet) deriveMasterRevocationRoot() (*btcec.PrivateKey, error) {
	masterElkremRoot, err := l.rootKey.Child(revocationRootIndex)
	if err != nil {
		return nil, err
	}

	return masterElkremRoot.ECPrivKey()
}

// deriveStateHintObfuscator derives the bytes to be used for obfuscating the
// state hints from the root to be used for a new channel. The obsfucsator is
// generated via the following computation:
//
//   * sha256(initiatorKey || responderKey)[26:]
//     * where both keys are the multi-sig keys of the respective parties
//
// The first 6 bytes of the resulting hash are used as the state hint.
func deriveStateHintObfuscator(key1, key2 *btcec.PublicKey) [StateHintSize]byte {
	h := sha256.New()
	h.Write(key1.SerializeCompressed())
	h.Write(key2.SerializeCompressed())

	sha := h.Sum(nil)

	var obfuscator [StateHintSize]byte
	copy(obfuscator[:], sha[26:])

	return obfuscator
}

// initStateHints properly sets the obsfucated state hints on both commitment
// transactions using the passed obsfucator.
func initStateHints(commit1, commit2 *wire.MsgTx,
	obfuscator [StateHintSize]byte) error {

	if err := SetStateNumHint(commit1, 0, obfuscator); err != nil {
		return err
	}
	if err := SetStateNumHint(commit2, 0, obfuscator); err != nil {
		return err
	}

	return nil
}

// selectInputs selects a slice of inputs necessary to meet the specified
// selection amount. If input selection is unable to succeed to to insufficient
// funds, a non-nil error is returned. Additionally, the total amount of the
// selected coins are returned in order for the caller to properly handle
// change+fees.
func selectInputs(amt btcutil.Amount, coins []*Utxo) (btcutil.Amount, []*wire.OutPoint, error) {
	var (
		selectedUtxos []*wire.OutPoint
		satSelected   btcutil.Amount
	)

	i := 0
	for satSelected < amt {
		// If we're about to go past the number of available coins,
		// then exit with an error.
		if i > len(coins)-1 {
			return 0, nil, &ErrInsufficientFunds{amt, satSelected}
		}

		// Otherwise, collect this new coin as it may be used for final
		// coin selection.
		coin := coins[i]
		utxo := &wire.OutPoint{
			Hash:  coin.Hash,
			Index: coin.Index,
		}

		selectedUtxos = append(selectedUtxos, utxo)
		satSelected += coin.Value

		i++
	}

	return satSelected, selectedUtxos, nil
}

// coinSelect attempts to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhering to the specified fee rate. The
// specified fee rate should be expressed in sat/byte for coin selection to
// function properly.
func coinSelect(feeRate uint64, amt btcutil.Amount,
	coins []*Utxo) ([]*wire.OutPoint, btcutil.Amount, error) {

	const (
		// txOverhead is the overhead of a transaction residing within
		// the version number and lock time.
		txOverhead = 8

		// p2wkhSpendSize an estimate of the number of bytes it takes
		// to spend a p2wkh output.
		//
		// (p2wkh witness) + txid + index + varint script size + sequence
		// TODO(roasbeef): div by 3 due to witness size?
		p2wkhSpendSize = (1 + 73 + 1 + 33) + 32 + 4 + 1 + 4

		// p2wkhOutputSize is an estimate of the size of a regualr
		// p2wkh output.
		//
		// 8 (output) + 1 (var int script) + 22 (p2wkh output)
		p2wkhOutputSize = 8 + 1 + 22

		// p2wkhOutputSize is an estimate of the p2wsh funding uotput.
		p2wshOutputSize = 8 + 1 + 34
	)

	var estimatedSize int

	amtNeeded := amt
	for {
		// First perform an initial round of coin selection to estimate
		// the required fee.
		totalSat, selectedUtxos, err := selectInputs(amtNeeded, coins)
		if err != nil {
			return nil, 0, err
		}

		// Based on the selected coins, estimate the size of the final
		// fully signed transaction.
		estimatedSize = ((len(selectedUtxos) * p2wkhSpendSize) +
			p2wshOutputSize + txOverhead)

		// The difference between the selected amount and the amount
		// requested will be used to pay fees, and generate a change
		// output with the remaining.
		overShootAmt := totalSat - amtNeeded

		// Based on the estimated size and fee rate, if the excess
		// amount isn't enough to pay fees, then increase the requested
		// coin amount by the estimate required fee, performing another
		// round of coin selection.
		requiredFee := btcutil.Amount(uint64(estimatedSize) * feeRate)
		if overShootAmt < requiredFee {
			amtNeeded += requiredFee
			continue
		}

		// If the fee is sufficient, then calculate the size of the
		// change output.
		changeAmt := overShootAmt - requiredFee

		return selectedUtxos, changeAmt, nil
	}
}

// StaticFeeEstimator will return a static value for all fee calculation
// requests. It is designed to be replaced by a proper fee calculation
// implementation.
type StaticFeeEstimator struct {
	FeeRate      uint64
	Confirmation uint32
}

// EstimateFeePerByte will return a static value for fee calculations.
func (e StaticFeeEstimator) EstimateFeePerByte(numBlocks uint32) uint64 {
	return e.FeeRate
}

// EstimateFeePerWeight will return a static value for fee calculations.
func (e StaticFeeEstimator) EstimateFeePerWeight(numBlocks uint32) uint64 {
	return e.FeeRate / 4
}

// EstimateConfirmation will return a static value representing the estimated
// number of blocks that will be required to confirm a transaction for the
// given fee rate.
func (e StaticFeeEstimator) EstimateConfirmation(satPerByte int64) uint32 {
	return e.Confirmation
}
