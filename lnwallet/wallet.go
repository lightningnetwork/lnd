package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanvalidate"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

const (
	// The size of the buffered queue of requests to the wallet from the
	// outside word.
	msgBufferSize = 100
)

// ErrInsufficientFunds is a type matching the error interface which is
// returned when coin selection for a new funding transaction fails to due
// having an insufficient amount of confirmed funds.
type ErrInsufficientFunds struct {
	amountAvailable btcutil.Amount
	amountSelected  btcutil.Amount
}

func (e *ErrInsufficientFunds) Error() string {
	return fmt.Sprintf("not enough witness outputs to create funding transaction,"+
		" need %v only have %v  available", e.amountAvailable,
		e.amountSelected)
}

// InitFundingReserveMsg is the first message sent to initiate the workflow
// required to open a payment channel with a remote peer. The initial required
// parameters are configurable across channels. These parameters are to be
// chosen depending on the fee climate within the network, and time value of
// funds to be locked up within the channel. Upon success a ChannelReservation
// will be created in order to track the lifetime of this pending channel.
// Outputs selected will be 'locked', making them unavailable, for any other
// pending reservations. Therefore, all channels in reservation limbo will be
// periodically timed out after an idle period in order to avoid "exhaustion"
// attacks.
type InitFundingReserveMsg struct {
	// ChainHash denotes that chain to be used to ultimately open the
	// target channel.
	ChainHash *chainhash.Hash

	// NodeID is the ID of the remote node we would like to open a channel
	// with.
	NodeID *btcec.PublicKey

	// NodeAddr is the address port that we used to either establish or
	// accept the connection which led to the negotiation of this funding
	// workflow.
	NodeAddr net.Addr

	// SubtractFees should be set if we intend to spend exactly
	// LocalFundingAmt when opening the channel, subtracting the fees from
	// the funding output. This can be used for instance to use all our
	// remaining funds to open the channel, since it will take fees into
	// account.
	SubtractFees bool

	// LocalFundingAmt is the amount of funds requested from us for this
	// channel.
	LocalFundingAmt btcutil.Amount

	// RemoteFundingAmnt is the amount of funds the remote will contribute
	// to this channel.
	RemoteFundingAmt btcutil.Amount

	// CommitFeePerKw is the starting accepted satoshis/Kw fee for the set
	// of initial commitment transactions. In order to ensure timely
	// confirmation, it is recommended that this fee should be generous,
	// paying some multiple of the accepted base fee rate of the network.
	CommitFeePerKw chainfee.SatPerKWeight

	// FundingFeePerKw is the fee rate in sat/kw to use for the initial
	// funding transaction.
	FundingFeePerKw chainfee.SatPerKWeight

	// PushMSat is the number of milli-satoshis that should be pushed over
	// the responder as part of the initial channel creation.
	PushMSat lnwire.MilliSatoshi

	// Flags are the channel flags specified by the initiator in the
	// open_channel message.
	Flags lnwire.FundingFlag

	// MinConfs indicates the minimum number of confirmations that each
	// output selected to fund the channel should satisfy.
	MinConfs int32

	// Tweakless indicates if the channel should use the new tweakless
	// commitment format or not.
	Tweakless bool

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
	theirFundingInputScripts []*input.Script

	// This should be 1/2 of the signatures needed to successfully spend our
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
	// successfully spend our version of the commitment transaction.
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
// within the Lightning Network, channel lifetimes, etc. However, it embeds a
// general purpose Bitcoin wallet within it. Therefore, it is also able to
// serve as a regular Bitcoin wallet which uses HD keys. The wallet is highly
// concurrent internally. All communication, and requests towards the wallet
// are dispatched as messages over channels, ensuring thread safety across all
// operations. Interaction has been designed independent of any peer-to-peer
// communication protocol, allowing the wallet to be self-contained and
// embeddable within future projects interacting with the Lightning Network.
//
// NOTE: At the moment the wallet requires a btcd full node, as it's dependent
// on btcd's websockets notifications as event triggers during the lifetime of a
// channel. However, once the chainntnfs package is complete, the wallet will
// be compatible with multiple RPC/notification services such as Electrum,
// Bitcoin Core + ZeroMQ, etc. Eventually, the wallet won't require a full-node
// at all, as SPV support is integrated into btcwallet.
type LightningWallet struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	nextFundingID uint64 // To be used atomically.

	// Cfg is the configuration struct that will be used by the wallet to
	// access the necessary interfaces and default it needs to carry on its
	// duties.
	Cfg Config

	// WalletController is the core wallet, all non Lightning Network
	// specific interaction is proxied to the internal wallet.
	WalletController

	// SecretKeyRing is the interface we'll use to derive any keys related
	// to our purpose within the network including: multi-sig keys, node
	// keys, revocation keys, etc.
	keychain.SecretKeyRing

	// This mutex MUST be held when performing coin selection in order to
	// avoid inadvertently creating multiple funding transaction which
	// double spend inputs across each other.
	coinSelectMtx sync.RWMutex

	// All messages to the wallet are to be sent across this channel.
	msgChan chan interface{}

	// Incomplete payment channels are stored in the map below. An intent
	// to create a payment channel is tracked as a "reservation" within
	// limbo. Once the final signatures have been exchanged, a reservation
	// is removed from limbo. Each reservation is tracked by a unique
	// monotonically integer. All requests concerning the channel MUST
	// carry a valid, active funding ID.
	fundingLimbo map[uint64]*ChannelReservation
	limboMtx     sync.RWMutex

	// lockedOutPoints is a set of the currently locked outpoint. This
	// information is kept in order to provide an easy way to unlock all
	// the currently locked outpoints.
	lockedOutPoints map[wire.OutPoint]struct{}

	quit chan struct{}

	wg sync.WaitGroup

	// TODO(roasbeef): handle wallet lock/unlock
}

// NewLightningWallet creates/opens and initializes a LightningWallet instance.
// If the wallet has never been created (according to the passed dataDir), first-time
// setup is executed.
func NewLightningWallet(Cfg Config) (*LightningWallet, error) {

	return &LightningWallet{
		Cfg:              Cfg,
		SecretKeyRing:    Cfg.SecretKeyRing,
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

// ResetReservations reset the volatile wallet state which tracks all currently
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
// (non-canceled) reservations.
func (l *LightningWallet) ActiveReservations() []*ChannelReservation {
	reservations := make([]*ChannelReservation, 0, len(l.fundingLimbo))
	for _, reservation := range l.fundingLimbo {
		reservations = append(reservations, reservation)
	}

	return reservations
}

// requestHandler is the primary goroutine(s) responsible for handling, and
// dispatching replies to all messages.
func (l *LightningWallet) requestHandler() {
out:
	for {
		select {
		case m := <-l.msgChan:
			switch msg := m.(type) {
			case *InitFundingReserveMsg:
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
// commitment transaction. Otherwise, an error occurred and a nil pointer along
// with an error are returned.
//
// Once a ChannelReservation has been obtained, two additional steps must be
// processed before a payment channel can be considered 'open'. The second step
// validates, and processes the counterparty's channel contribution. The third,
// and final step verifies all signatures for the inputs of the funding
// transaction, and that the signature we record for our version of the
// commitment transaction is valid.
func (l *LightningWallet) InitChannelReservation(
	req *InitFundingReserveMsg) (*ChannelReservation, error) {

	req.resp = make(chan *ChannelReservation, 1)
	req.err = make(chan error, 1)

	select {
	case l.msgChan <- req:
	case <-l.quit:
		return nil, errors.New("wallet shutting down")
	}

	return <-req.resp, <-req.err
}

// handleFundingReserveRequest processes a message intending to create, and
// validate a funding reservation request.
func (l *LightningWallet) handleFundingReserveRequest(req *InitFundingReserveMsg) {
	// It isn't possible to create a channel with zero funds committed.
	if req.LocalFundingAmt+req.RemoteFundingAmt == 0 {
		err := ErrZeroCapacity()
		req.err <- err
		req.resp <- nil
		return
	}

	// If the funding request is for a different chain than the one the
	// wallet is aware of, then we'll reject the request.
	if !bytes.Equal(l.Cfg.NetParams.GenesisHash[:], req.ChainHash[:]) {
		err := ErrChainMismatch(
			l.Cfg.NetParams.GenesisHash, req.ChainHash,
		)
		req.err <- err
		req.resp <- nil
		return
	}

	localFundingAmt := req.LocalFundingAmt

	var (
		selected *coinSelection
		err      error
	)

	// If we're on the receiving end of a single funder channel then we
	// don't need to perform any coin selection, and the remote contributes
	// all funds. Otherwise, attempt to obtain enough coins to meet the
	// required funding amount.
	if req.LocalFundingAmt != 0 {
		// Coin selection is done on the basis of sat/kw, so we'll use
		// the fee rate passed in to perform coin selection.
		var err error
		selected, err = l.selectCoinsAndChange(
			req.FundingFeePerKw, req.LocalFundingAmt, req.MinConfs,
			req.SubtractFees,
		)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}

		localFundingAmt = selected.fundingAmt
	}

	// The total channel capacity will be the size of the funding output we
	// created plus the remote contribution.
	capacity := localFundingAmt + req.RemoteFundingAmt

	id := atomic.AddUint64(&l.nextFundingID, 1)
	reservation, err := NewChannelReservation(
		capacity, localFundingAmt, req.CommitFeePerKw, l, id,
		req.PushMSat, l.Cfg.NetParams.GenesisHash, req.Flags,
		req.Tweakless,
	)
	if err != nil {
		selected.unlockCoins()
		req.err <- err
		req.resp <- nil
		return
	}

	err = l.initOurContribution(
		reservation, selected, req.NodeAddr, req.NodeID,
	)
	if err != nil {
		selected.unlockCoins()
		req.err <- err
		req.resp <- nil
		return
	}

	// Create a limbo and record entry for this newly pending funding
	// request.
	l.limboMtx.Lock()
	l.fundingLimbo[id] = reservation
	l.limboMtx.Unlock()

	// Funding reservation request successfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or canceled.
	req.resp <- reservation
	req.err <- nil
}

// initOurContribution initializes the given ChannelReservation with our coins
// and change reserved for the channel, and derives the keys to use for this
// channel.
func (l *LightningWallet) initOurContribution(reservation *ChannelReservation,
	selected *coinSelection, nodeAddr net.Addr, nodeID *btcec.PublicKey) error {

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	reservation.Lock()
	defer reservation.Unlock()

	if selected != nil {
		reservation.ourContribution.Inputs = selected.coins
		reservation.ourContribution.ChangeOutputs = selected.change
	}

	reservation.nodeAddr = nodeAddr
	reservation.partialState.IdentityPub = nodeID

	// Next, we'll grab a series of keys from the wallet which will be used
	// for the duration of the channel. The keys include: our multi-sig
	// key, the base revocation key, the base htlc key,the base payment
	// key, and the delayed payment key.
	//
	// TODO(roasbeef): "salt" each key as well?
	var err error
	reservation.ourContribution.MultiSigKey, err = l.DeriveNextKey(
		keychain.KeyFamilyMultiSig,
	)
	if err != nil {
		return err
	}
	reservation.ourContribution.RevocationBasePoint, err = l.DeriveNextKey(
		keychain.KeyFamilyRevocationBase,
	)
	if err != nil {
		return err
	}
	reservation.ourContribution.HtlcBasePoint, err = l.DeriveNextKey(
		keychain.KeyFamilyHtlcBase,
	)
	if err != nil {
		return err
	}
	reservation.ourContribution.PaymentBasePoint, err = l.DeriveNextKey(
		keychain.KeyFamilyPaymentBase,
	)
	if err != nil {
		return err
	}
	reservation.ourContribution.DelayBasePoint, err = l.DeriveNextKey(
		keychain.KeyFamilyDelayBase,
	)
	if err != nil {
		return err
	}

	// With the above keys created, we'll also need to initialization our
	// initial revocation tree state.
	nextRevocationKeyDesc, err := l.DeriveNextKey(
		keychain.KeyFamilyRevocationRoot,
	)
	if err != nil {
		return err
	}
	revocationRoot, err := l.DerivePrivKey(nextRevocationKeyDesc)
	if err != nil {
		return err
	}

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	revRoot, err := chainhash.NewHash(revocationRoot.Serialize())
	if err != nil {
		return err
	}
	producer := shachain.NewRevocationProducer(*revRoot)
	firstPreimage, err := producer.AtIndex(0)
	if err != nil {
		return err
	}
	reservation.ourContribution.FirstCommitmentPoint = input.ComputeCommitmentPoint(
		firstPreimage[:],
	)

	reservation.partialState.RevocationProducer = producer
	reservation.ourContribution.ChannelConstraints = l.Cfg.DefaultConstraints

	return nil
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
		// TODO(roasbeef): make new error, "unknown funding state" or something
		req.err <- fmt.Errorf("attempted to cancel non-existent funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Mark all previously locked outpoints as useable for future funding
	// requests.
	for _, unusedInput := range pendingReservation.ourContribution.Inputs {
		delete(l.lockedOutPoints, unusedInput.PreviousOutPoint)
		l.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): is it even worth it to keep track of unused keys?

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
	fundingTxIn wire.TxIn,
	tweaklessCommit bool) (*wire.MsgTx, *wire.MsgTx, error) {

	localCommitmentKeys := DeriveCommitmentKeys(
		localCommitPoint, true, tweaklessCommit, ourChanCfg,
		theirChanCfg,
	)
	remoteCommitmentKeys := DeriveCommitmentKeys(
		remoteCommitPoint, false, tweaklessCommit, ourChanCfg,
		theirChanCfg,
	)

	ourCommitTx, err := CreateCommitTx(fundingTxIn, localCommitmentKeys,
		uint32(ourChanCfg.CsvDelay), localBalance, remoteBalance,
		ourChanCfg.DustLimit)
	if err != nil {
		return nil, nil, err
	}

	otxn := btcutil.NewTx(ourCommitTx)
	if err := blockchain.CheckTransactionSanity(otxn); err != nil {
		return nil, nil, err
	}

	theirCommitTx, err := CreateCommitTx(fundingTxIn, remoteCommitmentKeys,
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
		req.err <- fmt.Errorf("attempted to update non-existent funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thread-safety
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
	witnessScript, multiSigOut, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(),
		theirKey.PubKey.SerializeCompressed(), channelCapacity,
	)
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
	pendingReservation.ourFundingInputScripts = make([]*input.Script, 0,
		len(ourContribution.Inputs))
	signDesc := input.SignDescriptor{
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

		signDesc.Output = &wire.TxOut{
			PkScript: info.PkScript,
			Value:    int64(info.Value),
		}
		signDesc.InputIndex = i

		inputScript, err := l.Cfg.Signer.ComputeInputScript(
			fundingTx, &signDesc,
		)
		if err != nil {
			req.err <- err
			return
		}

		txIn.SignatureScript = inputScript.SigScript
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
	_, multiSigIndex := input.FindScriptOutputIndex(fundingTx, multiSigOut.PkScript)
	fundingOutpoint := wire.NewOutPoint(&fundingTxID, multiSigIndex)
	pendingReservation.partialState.FundingOutpoint = *fundingOutpoint

	walletLog.Debugf("Funding tx for ChannelPoint(%v) generated: %v",
		fundingOutpoint, spew.Sdump(fundingTx))

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
	fundingTxIn := wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  fundingTxID,
			Index: multiSigIndex,
		},
	}

	// With the funding tx complete, create both commitment transactions.
	localBalance := pendingReservation.partialState.LocalCommitment.LocalBalance.ToSatoshis()
	remoteBalance := pendingReservation.partialState.LocalCommitment.RemoteBalance.ToSatoshis()
	tweaklessCommits := pendingReservation.partialState.ChanType.IsTweakless()
	ourCommitTx, theirCommitTx, err := CreateCommitmentTxns(
		localBalance, remoteBalance, ourContribution.ChannelConfig,
		theirContribution.ChannelConfig,
		ourContribution.FirstCommitmentPoint,
		theirContribution.FirstCommitmentPoint, fundingTxIn,
		tweaklessCommits,
	)
	if err != nil {
		req.err <- err
		return
	}

	// With both commitment transactions constructed, generate the state
	// obfuscator then use it to encode the current state number within
	// both commitment transactions.
	var stateObfuscator [StateHintSize]byte
	if chanState.ChanType.IsSingleFunder() {
		stateObfuscator = DeriveStateHintObfuscator(
			ourContribution.PaymentBasePoint.PubKey,
			theirContribution.PaymentBasePoint.PubKey,
		)
	} else {
		ourSer := ourContribution.PaymentBasePoint.PubKey.SerializeCompressed()
		theirSer := theirContribution.PaymentBasePoint.PubKey.SerializeCompressed()
		switch bytes.Compare(ourSer, theirSer) {
		case -1:
			stateObfuscator = DeriveStateHintObfuscator(
				ourContribution.PaymentBasePoint.PubKey,
				theirContribution.PaymentBasePoint.PubKey,
			)
		default:
			stateObfuscator = DeriveStateHintObfuscator(
				theirContribution.PaymentBasePoint.PubKey,
				ourContribution.PaymentBasePoint.PubKey,
			)
		}
	}
	err = initStateHints(ourCommitTx, theirCommitTx, stateObfuscator)
	if err != nil {
		req.err <- err
		return
	}

	// Sort both transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourCommitTx)
	txsort.InPlaceSort(theirCommitTx)

	walletLog.Debugf("Local commit tx for ChannelPoint(%v): %v",
		fundingOutpoint, spew.Sdump(ourCommitTx))
	walletLog.Debugf("Remote commit tx for ChannelPoint(%v): %v",
		fundingOutpoint, spew.Sdump(theirCommitTx))

	// Record newly available information within the open channel state.
	chanState.FundingOutpoint = *fundingOutpoint
	chanState.LocalCommitment.CommitTx = ourCommitTx
	chanState.RemoteCommitment.CommitTx = theirCommitTx

	// Generate a signature for their version of the initial commitment
	// transaction.
	signDesc = input.SignDescriptor{
		WitnessScript: witnessScript,
		KeyDesc:       ourKey,
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
			txin.SignatureScript = inputScripts[sigIndex].SigScript

			// Fetch the alleged previous output along with the
			// pkscript referenced by this input.
			//
			// TODO(roasbeef): when dual funder pass actual
			// height-hint
			pkScript, err := input.WitnessScriptHash(
				txin.Witness[len(txin.Witness)-1],
			)
			if err != nil {
				msg.err <- fmt.Errorf("cannot create script: "+
					"%v", err)
				msg.completeChan <- nil
				return
			}

			output, err := l.Cfg.ChainIO.GetUtxo(
				&txin.PreviousOutPoint,
				pkScript, 0, l.quit,
			)
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
	commitTx := res.partialState.LocalCommitment.CommitTx
	ourKey := res.ourContribution.MultiSigKey
	theirKey := res.theirContribution.MultiSigKey

	// Re-generate both the witnessScript and p2sh output. We sign the
	// witnessScript script, but include the p2sh output as the subscript
	// for verification.
	witnessScript, _, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(),
		theirKey.PubKey.SerializeCompressed(),
		int64(res.partialState.Capacity),
	)
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	}

	// Next, create the spending scriptSig, and then verify that the script
	// is complete, allowing us to spend from the funding transaction.
	channelValue := int64(res.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(commitTx)
	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, hashCache,
		txscript.SigHashAll, commitTx, 0, channelValue)
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	}

	// Verify that we've received a valid signature from the remote party
	// for our version of the commitment transaction.
	theirCommitSig := msg.theirCommitmentSig
	sig, err := btcec.ParseSignature(theirCommitSig, btcec.S256())
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
	} else if !sig.Verify(sigHash, theirKey.PubKey) {
		msg.err <- fmt.Errorf("counterparty's commitment signature is invalid")
		msg.completeChan <- nil
		return
	}
	res.partialState.LocalCommitment.CommitSig = theirCommitSig

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

	// We'll also record the finalized funding txn, which will allow us to
	// rebroadcast on startup in case we fail.
	res.partialState.FundingTxn = fundingTx

	// Add the complete funding transaction to the DB, in its open bucket
	// which will be used for the lifetime of this channel.
	nodeAddr := res.nodeAddr
	err = res.partialState.SyncPending(nodeAddr, uint32(bestHeight))
	if err != nil {
		msg.err <- err
		msg.completeChan <- nil
		return
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
		req.err <- fmt.Errorf("attempted to update non-existent funding state")
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
	localBalance := pendingReservation.partialState.LocalCommitment.LocalBalance.ToSatoshis()
	remoteBalance := pendingReservation.partialState.LocalCommitment.RemoteBalance.ToSatoshis()
	tweaklessCommits := pendingReservation.partialState.ChanType.IsTweakless()
	ourCommitTx, theirCommitTx, err := CreateCommitmentTxns(
		localBalance, remoteBalance,
		pendingReservation.ourContribution.ChannelConfig,
		pendingReservation.theirContribution.ChannelConfig,
		pendingReservation.ourContribution.FirstCommitmentPoint,
		pendingReservation.theirContribution.FirstCommitmentPoint,
		*fundingTxIn, tweaklessCommits,
	)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}

	// With both commitment transactions constructed, we can now use the
	// generator state obfuscator to encode the current state number within
	// both commitment transactions.
	stateObfuscator := DeriveStateHintObfuscator(
		pendingReservation.theirContribution.PaymentBasePoint.PubKey,
		pendingReservation.ourContribution.PaymentBasePoint.PubKey,
	)
	err = initStateHints(ourCommitTx, theirCommitTx, stateObfuscator)
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
	chanState.LocalCommitment.CommitTx = ourCommitTx
	chanState.RemoteCommitment.CommitTx = theirCommitTx

	walletLog.Debugf("Local commit tx for ChannelPoint(%v): %v",
		req.fundingOutpoint, spew.Sdump(ourCommitTx))
	walletLog.Debugf("Remote commit tx for ChannelPoint(%v): %v",
		req.fundingOutpoint, spew.Sdump(theirCommitTx))

	channelValue := int64(pendingReservation.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(ourCommitTx)
	theirKey := pendingReservation.theirContribution.MultiSigKey
	ourKey := pendingReservation.ourContribution.MultiSigKey
	witnessScript, _, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(),
		theirKey.PubKey.SerializeCompressed(), channelValue,
	)
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
	} else if !sig.Verify(sigHash, theirKey.PubKey) {
		req.err <- fmt.Errorf("counterparty's commitment signature " +
			"is invalid")
		req.completeChan <- nil
		return
	}
	chanState.LocalCommitment.CommitSig = req.theirCommitmentSig

	// With their signature for our version of the commitment transactions
	// verified, we can now generate a signature for their version,
	// allowing the funding transaction to be safely broadcast.
	p2wsh, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		req.err <- err
		req.completeChan <- nil
		return
	}
	signDesc := input.SignDescriptor{
		WitnessScript: witnessScript,
		KeyDesc:       ourKey,
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

// WithCoinSelectLock will execute the passed function closure in a
// synchronized manner preventing any coin selection operations from proceeding
// while the closure if executing. This can be seen as the ability to execute a
// function closure under an exclusive coin selection lock.
func (l *LightningWallet) WithCoinSelectLock(f func() error) error {
	l.coinSelectMtx.Lock()
	defer l.coinSelectMtx.Unlock()

	return f()
}

// coinSelection holds the result from selectCoinsAndChange.
type coinSelection struct {
	coins       []*wire.TxIn
	change      []*wire.TxOut
	fundingAmt  btcutil.Amount
	unlockCoins func()
}

// selectCoinsAndChange performs coin selection in order to obtain witness
// outputs which sum to at least 'amt' amount of satoshis. If necessary,
// a change address will also be generated. If coin selection is
// successful/possible, then the selected coins and change outputs are
// returned, and the value of the resulting funding output. This method locks
// the selected outputs, and a function closure to unlock them in case of an
// error is returned.
func (l *LightningWallet) selectCoinsAndChange(feeRate chainfee.SatPerKWeight,
	amt btcutil.Amount, minConfs int32, subtractFees bool) (
	*coinSelection, error) {

	// We hold the coin select mutex while querying for outputs, and
	// performing coin selection in order to avoid inadvertent double
	// spends across funding transactions.
	l.coinSelectMtx.Lock()
	defer l.coinSelectMtx.Unlock()

	walletLog.Infof("Performing funding tx coin selection using %v "+
		"sat/kw as fee rate", int64(feeRate))

	// Find all unlocked unspent witness outputs that satisfy the minimum
	// number of confirmations required.
	coins, err := l.ListUnspentWitness(minConfs, math.MaxInt32)
	if err != nil {
		return nil, err
	}

	var (
		selectedCoins []*Utxo
		fundingAmt    btcutil.Amount
		changeAmt     btcutil.Amount
	)

	// Perform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount
	// requirements.
	switch {
	// In case this request want the fees subtracted from the local amount,
	// we'll call the specialized method for that. This ensures that we
	// won't deduct more that the specified balance from our wallet.
	case subtractFees:
		dustLimit := l.Cfg.DefaultConstraints.DustLimit
		selectedCoins, fundingAmt, changeAmt, err = coinSelectSubtractFees(
			feeRate, amt, dustLimit, coins,
		)
		if err != nil {
			return nil, err
		}

	// Ótherwise do a normal coin selection where we target a given funding
	// amount.
	default:
		fundingAmt = amt
		selectedCoins, changeAmt, err = coinSelect(feeRate, amt, coins)
		if err != nil {
			return nil, err
		}
	}

	// Record any change output(s) generated as a result of the coin
	// selection, but only if the addition of the output won't lead to the
	// creation of dust.
	var changeOutputs []*wire.TxOut
	if changeAmt != 0 && changeAmt > DefaultDustLimit() {
		changeAddr, err := l.NewAddress(WitnessPubKey, true)
		if err != nil {
			return nil, err
		}
		changeScript, err := txscript.PayToAddrScript(changeAddr)
		if err != nil {
			return nil, err
		}

		changeOutputs = make([]*wire.TxOut, 1)
		changeOutputs[0] = &wire.TxOut{
			Value:    int64(changeAmt),
			PkScript: changeScript,
		}
	}

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	inputs := make([]*wire.TxIn, len(selectedCoins))
	for i, coin := range selectedCoins {
		outpoint := &coin.OutPoint
		l.lockedOutPoints[*outpoint] = struct{}{}
		l.LockOutpoint(*outpoint)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		inputs[i] = wire.NewTxIn(outpoint, nil, nil)
	}

	unlock := func() {
		l.coinSelectMtx.Lock()
		defer l.coinSelectMtx.Unlock()

		for _, coin := range selectedCoins {
			outpoint := &coin.OutPoint
			delete(l.lockedOutPoints, *outpoint)
			l.UnlockOutpoint(*outpoint)
		}
	}

	return &coinSelection{
		coins:       inputs,
		change:      changeOutputs,
		fundingAmt:  fundingAmt,
		unlockCoins: unlock,
	}, nil
}

// DeriveStateHintObfuscator derives the bytes to be used for obfuscating the
// state hints from the root to be used for a new channel. The obfuscator is
// generated via the following computation:
//
//   * sha256(initiatorKey || responderKey)[26:]
//     * where both keys are the multi-sig keys of the respective parties
//
// The first 6 bytes of the resulting hash are used as the state hint.
func DeriveStateHintObfuscator(key1, key2 *btcec.PublicKey) [StateHintSize]byte {
	h := sha256.New()
	h.Write(key1.SerializeCompressed())
	h.Write(key2.SerializeCompressed())

	sha := h.Sum(nil)

	var obfuscator [StateHintSize]byte
	copy(obfuscator[:], sha[26:])

	return obfuscator
}

// initStateHints properly sets the obfuscated state hints on both commitment
// transactions using the passed obfuscator.
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
// selection amount. If input selection is unable to succeed due to insufficient
// funds, a non-nil error is returned. Additionally, the total amount of the
// selected coins are returned in order for the caller to properly handle
// change+fees.
func selectInputs(amt btcutil.Amount, coins []*Utxo) (btcutil.Amount, []*Utxo, error) {
	satSelected := btcutil.Amount(0)
	for i, coin := range coins {
		satSelected += coin.Value
		if satSelected >= amt {
			return satSelected, coins[:i+1], nil
		}
	}
	return 0, nil, &ErrInsufficientFunds{amt, satSelected}
}

// coinSelect attempts to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhering to the specified fee rate. The
// specified fee rate should be expressed in sat/kw for coin selection to
// function properly.
func coinSelect(feeRate chainfee.SatPerKWeight, amt btcutil.Amount,
	coins []*Utxo) ([]*Utxo, btcutil.Amount, error) {

	amtNeeded := amt
	for {
		// First perform an initial round of coin selection to estimate
		// the required fee.
		totalSat, selectedUtxos, err := selectInputs(amtNeeded, coins)
		if err != nil {
			return nil, 0, err
		}

		var weightEstimate input.TxWeightEstimator

		for _, utxo := range selectedUtxos {
			switch utxo.AddressType {
			case WitnessPubKey:
				weightEstimate.AddP2WKHInput()
			case NestedWitnessPubKey:
				weightEstimate.AddNestedP2WKHInput()
			default:
				return nil, 0, fmt.Errorf("unsupported address type: %v",
					utxo.AddressType)
			}
		}

		// Channel funding multisig output is P2WSH.
		weightEstimate.AddP2WSHOutput()

		// Assume that change output is a P2WKH output.
		//
		// TODO: Handle wallets that generate non-witness change
		// addresses.
		// TODO(halseth): make coinSelect not estimate change output
		// for dust change.
		weightEstimate.AddP2WKHOutput()

		// The difference between the selected amount and the amount
		// requested will be used to pay fees, and generate a change
		// output with the remaining.
		overShootAmt := totalSat - amt

		// Based on the estimated size and fee rate, if the excess
		// amount isn't enough to pay fees, then increase the requested
		// coin amount by the estimate required fee, performing another
		// round of coin selection.
		totalWeight := int64(weightEstimate.Weight())
		requiredFee := feeRate.FeeForWeight(totalWeight)
		if overShootAmt < requiredFee {
			amtNeeded = amt + requiredFee
			continue
		}

		// If the fee is sufficient, then calculate the size of the
		// change output.
		changeAmt := overShootAmt - requiredFee

		return selectedUtxos, changeAmt, nil
	}
}

// coinSelectSubtractFees attempts to select coins such that we'll spend up to
// amt in total after fees, adhering to the specified fee rate. The selected
// coins, the final output and change values are returned.
func coinSelectSubtractFees(feeRate chainfee.SatPerKWeight, amt,
	dustLimit btcutil.Amount, coins []*Utxo) ([]*Utxo, btcutil.Amount,
	btcutil.Amount, error) {

	// First perform an initial round of coin selection to estimate
	// the required fee.
	totalSat, selectedUtxos, err := selectInputs(amt, coins)
	if err != nil {
		return nil, 0, 0, err
	}

	var weightEstimate input.TxWeightEstimator
	for _, utxo := range selectedUtxos {
		switch utxo.AddressType {
		case WitnessPubKey:
			weightEstimate.AddP2WKHInput()
		case NestedWitnessPubKey:
			weightEstimate.AddNestedP2WKHInput()
		default:
			return nil, 0, 0, fmt.Errorf("unsupported "+
				"address type: %v", utxo.AddressType)
		}
	}

	// Channel funding multisig output is P2WSH.
	weightEstimate.AddP2WSHOutput()

	// At this point we've got two possibilities, either create a
	// change output, or not. We'll first try without creating a
	// change output.
	//
	// Estimate the fee required for a transaction without a change
	// output.
	totalWeight := int64(weightEstimate.Weight())
	requiredFee := feeRate.FeeForWeight(totalWeight)

	// For a transaction without a change output, we'll let everything go
	// to our multi-sig output after subtracting fees.
	outputAmt := totalSat - requiredFee
	changeAmt := btcutil.Amount(0)

	// If the the output is too small after subtracting the fee, the coin
	// selection cannot be performed with an amount this small.
	if outputAmt <= dustLimit {
		return nil, 0, 0, fmt.Errorf("output amount(%v) after "+
			"subtracting fees(%v) below dust limit(%v)", outputAmt,
			requiredFee, dustLimit)
	}

	// We were able to create a transaction with no change from the
	// selected inputs. We'll remember the resulting values for
	// now, while we try to add a change output. Assume that change output
	// is a P2WKH output.
	weightEstimate.AddP2WKHOutput()

	// Now that we have added the change output, redo the fee
	// estimate.
	totalWeight = int64(weightEstimate.Weight())
	requiredFee = feeRate.FeeForWeight(totalWeight)

	// For a transaction with a change output, everything we don't spend
	// will go to change.
	newChange := totalSat - amt
	newOutput := amt - requiredFee

	// If adding a change output leads to both outputs being above
	// the dust limit, we'll add the change output. Otherwise we'll
	// go with the no change tx we originally found.
	if newChange > dustLimit && newOutput > dustLimit {
		outputAmt = newOutput
		changeAmt = newChange
	}

	// Sanity check the resulting output values to make sure we
	// don't burn a great part to fees.
	totalOut := outputAmt + changeAmt
	fee := totalSat - totalOut

	// Fail if more than 20% goes to fees.
	// TODO(halseth): smarter fee limit. Make configurable or dynamic wrt
	// total funding size?
	if fee > totalOut/5 {
		return nil, 0, 0, fmt.Errorf("fee %v on total output"+
			"value %v", fee, totalOut)
	}

	return selectedUtxos, outputAmt, changeAmt, nil
}

// ValidateChannel will attempt to fully validate a newly mined channel, given
// its funding transaction and existing channel state. If this method returns
// an error, then the mined channel is invalid, and shouldn't be used.
func (l *LightningWallet) ValidateChannel(channelState *channeldb.OpenChannel,
	fundingTx *wire.MsgTx) error {

	// First, we'll obtain a fully signed commitment transaction so we can
	// pass into it on the chanvalidate package for verification.
	channel, err := NewLightningChannel(l.Cfg.Signer, channelState, nil)
	if err != nil {
		return err
	}
	signedCommitTx, err := channel.getSignedCommitTx()
	if err != nil {
		return err
	}

	// We'll also need the multi-sig witness script itself so the
	// chanvalidate package can check it for correctness against the
	// funding transaction, and also commitment validity.
	localKey := channelState.LocalChanCfg.MultiSigKey.PubKey
	remoteKey := channelState.RemoteChanCfg.MultiSigKey.PubKey
	witnessScript, err := input.GenMultiSigScript(
		localKey.SerializeCompressed(),
		remoteKey.SerializeCompressed(),
	)
	if err != nil {
		return err
	}
	pkScript, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return err
	}

	// Finally, we'll pass in all the necessary context needed to fully
	// validate that this channel is indeed what we expect, and can be
	// used.
	_, err = chanvalidate.Validate(&chanvalidate.Context{
		Locator: &chanvalidate.OutPointChanLocator{
			ChanPoint: channelState.FundingOutpoint,
		},
		MultiSigPkScript: pkScript,
		FundingTx:        fundingTx,
		CommitCtx: &chanvalidate.CommitmentContext{
			Value:               channel.Capacity,
			FullySignedCommitTx: signedCommitTx,
		},
	})
	if err != nil {
		return err
	}

	return nil
}
