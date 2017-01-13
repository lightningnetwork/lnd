package lnwallet

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil/hdkeychain"

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

	// elkremRootIndex is the top level HD key index from which secrets
	// used to generate elkrem roots should be derived from.
	elkremRootIndex = hdkeychain.HardenedKeyStart + 1

	// identityKeyIndex is the top level HD key index which is used to
	// generate/rotate identity keys.
	//
	// TODO(roasbeef): should instead be child to make room for future
	// rotations, etc.
	identityKeyIndex = hdkeychain.HardenedKeyStart + 2

	commitFee = 5000
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
// chosen depending on the fee climate within the network, and time value of funds to
// be locked up within the channel. Upon success a ChannelReservation will be
// created in order to track the lifetime of this pending channel. Outputs
// selected will be 'locked', making them unavailable, for any other pending
// reservations. Therefore, all channels in reservation limbo will be periodically
// after a timeout period in order to avoid "exhaustion" attacks.
//
// TODO(roasbeef): zombie reservation sweeper goroutine.
type initFundingReserveMsg struct {
	// The ID of the remote node we would like to open a channel with.
	nodeID *btcec.PublicKey

	// The IP address plus port that we used to either establish or accept
	// the connection which led to the negotiation of this funding
	// workflow.
	nodeAddr *net.TCPAddr

	// The number of confirmations required before the channel is considered
	// open.
	numConfs uint16

	// The amount of funds requested for this channel.
	fundingAmount btcutil.Amount

	// The total capacity of the channel which includes the amount of funds
	// the remote party contributes (if any).
	capacity btcutil.Amount

	// The minimum accepted satoshis/KB fee for the funding transaction. In
	// order to ensure timely confirmation, it is recomened that this fee
	// should be generous, paying some multiple of the accepted base fee
	// rate of the network.
	// TODO(roasbeef): integrate fee estimation project...
	minFeeRate btcutil.Amount

	// ourDustLimit is the threshold below which no HTLC output should be
	// generated for our commitment transaction; ie. HTLCs below
	// this amount are not enforceable onchain from our point of view.
	ourDustLimit btcutil.Amount

	// pushSat is the number of satoshis that should be pushed over the the
	// responder as part of the initial channel creation.
	pushSat btcutil.Amount

	// The delay on the "pay-to-self" output(s) of the commitment transaction.
	csvDelay uint32

	// A channel in which all errors will be sent accross. Will be nil if
	// this initial set is succesful.
	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error

	// A ChannelReservation with our contributions filled in will be sent
	// accross this channel in the case of a succesfully reservation
	// initiation. In the case of an error, this will read a nil pointer.
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

	// revokeKey is the revocation public key derived by the remote node to
	// be used within the initial version of the commitment transaction we
	// construct for them.
	revokeKey *btcec.PublicKey

	// theirCommitmentSig are the 1/2 of the signatures needed to
	// succesfully spend our version of the commitment transaction.
	theirCommitmentSig []byte

	// obsfucator is the bytes to be used to obsfucate the state hints on
	// the commitment transaction.
	obsfucator [StateHintSize]byte

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// channelOpenMsg is the final message sent to finalize a single funder channel
// workflow to which we are the responder to. This message is sent once the
// remote peer deems the channel open, meaning it has reached a sufficient
// number of confirmations in the blockchain.
type channelOpenMsg struct {
	pendingFundingID uint64

	// TODO(roasbeef): move verification up to upper layer, yeh?
	spvProof []byte

	// NOTE: In order to avoid deadlocks, this channel MUST be buffered.
	err chan error
}

// LightningWallet is a domain specific, yet general Bitcoin wallet capable of
// executing workflow required to interact with the Lightning Network. It is
// domain specific in the sense that it understands all the fancy scripts used
// within the Lightning Network, channel lifetimes, etc. However, it embedds a
// general purpose Bitcoin wallet within it. Therefore, it is also able to serve
// as a regular Bitcoin wallet which uses HD keys. The wallet is highly concurrent
// internally. All communication, and requests towards the wallet are
// dispatched as messages over channels, ensuring thread safety across all
// operations. Interaction has been designed independent of any peer-to-peer
// communication protocol, allowing the wallet to be self-contained and embeddable
// within future projects interacting with the Lightning Network.
// NOTE: At the moment the wallet requires a btcd full node, as it's dependent
// on btcd's websockets notifications as even triggers during the lifetime of
// a channel. However, once the chainntnfs package is complete, the wallet
// will be compatible with multiple RPC/notification services such as Electrum,
// Bitcoin Core + ZeroMQ, etc. Eventually, the wallet won't require a full-node
// at all, as SPV support is integrated inot btcwallet.
type LightningWallet struct {
	// This mutex is to be held when generating external keys to be used
	// as multi-sig, and commitment keys within the channel.
	keyGenMtx sync.RWMutex

	// This mutex MUST be held when performing coin selection in order to
	// avoid inadvertently creating multiple funding transaction which
	// double spend inputs across each other.
	coinSelectMtx sync.RWMutex

	// A wrapper around a namespace within boltdb reserved for ln-based
	// wallet metadata. See the 'channeldb' package for further
	// information.
	ChannelDB *channeldb.DB

	// Used by in order to obtain notifications about funding transaction
	// reaching a specified confirmation depth, and to catch
	// counterparty's broadcasting revoked commitment states.
	chainNotifier chainntnfs.ChainNotifier

	// wallet is the the core wallet, all non Lightning Network specific
	// interaction is proxied to the internal wallet.
	WalletController

	// Signer is the wallet's current Signer implementation. This Signer is
	// used to generate signature for all inputs to potential funding
	// transactions, as well as for spends from the funding transaction to
	// update the commitment state.
	Signer Signer

	// ChainIO is an instance of the BlockChainIO interface. ChainIO is
	// used to lookup the existence of outputs within the UTXO set.
	ChainIO BlockChainIO

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

	netParams *chaincfg.Params

	started  int32
	shutdown int32
	quit     chan struct{}

	wg sync.WaitGroup

	// TODO(roasbeef): handle wallet lock/unlock
}

// NewLightningWallet creates/opens and initializes a LightningWallet instance.
// If the wallet has never been created (according to the passed dataDir), first-time
// setup is executed.
//
// NOTE: The passed channeldb, and ChainNotifier should already be fully
// initialized/started before being passed as a function arugment.
func NewLightningWallet(cdb *channeldb.DB, notifier chainntnfs.ChainNotifier,
	wallet WalletController, signer Signer, bio BlockChainIO,
	netParams *chaincfg.Params) (*LightningWallet, error) {

	// TODO(roasbeef): need a another wallet level config

	// Fetch the root derivation key from the wallet's HD chain. We'll use
	// this to generate specific Lightning related secrets on the fly.
	rootKey, err := wallet.FetchRootKey()
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): always re-derive on the fly?
	rootKeyRaw := rootKey.Serialize()
	rootMasterKey, err := hdkeychain.NewMaster(rootKeyRaw, netParams)
	if err != nil {
		return nil, err
	}

	return &LightningWallet{
		rootKey:          rootMasterKey,
		chainNotifier:    notifier,
		Signer:           signer,
		WalletController: wallet,
		ChainIO:          bio,
		ChannelDB:        cdb,
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

// LockOutpoints returns a list of all currently locked outpoint.
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
			case *channelOpenMsg:
				l.handleChannelOpen(msg)
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
func (l *LightningWallet) InitChannelReservation(capacity,
	ourFundAmt btcutil.Amount, theirID *btcec.PublicKey,
	theirAddr *net.TCPAddr, numConfs uint16,
	csvDelay uint32, ourDustLimit btcutil.Amount,
	pushSat btcutil.Amount) (*ChannelReservation, error) {

	// TODO(roasbeef): make the above into an initial config as part of the
	// refactor to implement spec compliant funding flow

	errChan := make(chan error, 1)
	respChan := make(chan *ChannelReservation, 1)

	l.msgChan <- &initFundingReserveMsg{
		capacity:      capacity,
		numConfs:      numConfs,
		fundingAmount: ourFundAmt,
		csvDelay:      csvDelay,
		ourDustLimit:  ourDustLimit,
		pushSat:       pushSat,
		nodeID:        theirID,
		nodeAddr:      theirAddr,
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

	id := atomic.AddUint64(&l.nextFundingID, 1)
	totalCapacity := req.capacity + commitFee
	reservation := NewChannelReservation(totalCapacity, req.fundingAmount,
		req.minFeeRate, l, id, req.numConfs, req.pushSat)

	// Grab the mutex on the ChannelReservation to ensure thread-safety
	reservation.Lock()
	defer reservation.Unlock()

	reservation.nodeAddr = req.nodeAddr
	reservation.ourContribution.CsvDelay = req.csvDelay

	reservation.partialState.IdentityPub = req.nodeID
	reservation.partialState.LocalCsvDelay = req.csvDelay
	reservation.partialState.OurDustLimit = req.ourDustLimit

	ourContribution := reservation.ourContribution

	// If we're on the receiving end of a single funder channel then we
	// don't need to perform any coin selection. Otherwise, attempt to
	// obtain enough coins to meet the required funding amount.
	if req.fundingAmount != 0 {
		// TODO(roasbeef): consult model for proper fee rate on funding
		// tx
		feeRate := uint64(10)
		amt := req.fundingAmount + commitFee
		err := l.selectCoinsAndChange(feeRate, amt, ourContribution)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}
	}

	// Grab two fresh keys from our HD chain, one will be used for the
	// multi-sig funding transaction, and the other for the commitment
	// transaction.
	multiSigKey, err := l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	commitKey, err := l.NewRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.partialState.OurMultiSigKey = multiSigKey
	ourContribution.MultiSigKey = multiSigKey
	reservation.partialState.OurCommitKey = commitKey
	ourContribution.CommitKey = commitKey

	// Generate a fresh address to be used in the case of a cooperative
	// channel close.
	deliveryAddress, err := l.NewAddress(WitnessPubKey, false)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	deliveryScript, err := txscript.PayToAddrScript(deliveryAddress)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.partialState.OurDeliveryScript = deliveryScript
	ourContribution.DeliveryAddress = deliveryAddress

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

	ourKey := pendingReservation.partialState.OurMultiSigKey
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
	pendingReservation.partialState.FundingWitnessScript = witnessScript

	// Sort the transaction. Since both side agree to a canonical
	// ordering, by sorting we no longer need to send the entire
	// transaction. Only signatures will be exchanged.
	fundingTx.AddTxOut(multiSigOut)
	txsort.InPlaceSort(pendingReservation.fundingTx)

	// Next, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	pendingReservation.ourFundingInputScripts = make([]*InputScript, 0, len(ourContribution.Inputs))
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

		inputScript, err := l.Signer.ComputeInputScript(fundingTx, &signDesc)
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
	pendingReservation.partialState.FundingOutpoint = fundingOutpoint

	// Initialize an empty sha-chain for them, tracking the current pending
	// revocation hash (we don't yet know the preimage so we can't add it
	// to the chain).
	e := &elkrem.ElkremReceiver{}
	pendingReservation.partialState.RemoteElkrem = e
	pendingReservation.partialState.TheirCurrentRevocation = theirContribution.RevocationKey

	masterElkremRoot, err := l.deriveMasterElkremRoot()
	if err != nil {
		req.err <- err
		return
	}

	// Now that we have their commitment key, we can create the revocation
	// key for the first version of our commitment transaction. To do so,
	// we'll first create our elkrem root, then grab the first pre-iamge
	// from it.
	elkremRoot := deriveElkremRoot(masterElkremRoot, ourKey, theirKey)
	elkremSender := elkrem.NewElkremSender(elkremRoot)
	pendingReservation.partialState.LocalElkrem = elkremSender
	firstPreimage, err := elkremSender.AtIndex(0)
	if err != nil {
		req.err <- err
		return
	}
	theirCommitKey := theirContribution.CommitKey
	ourRevokeKey := DeriveRevocationPubkey(theirCommitKey, firstPreimage[:])

	// Create the txIn to our commitment transaction; required to construct
	// the commitment transactions.
	fundingTxIn := wire.NewTxIn(wire.NewOutPoint(&fundingTxID, multiSigIndex), nil, nil)

	// With the funding tx complete, create both commitment transactions.
	// TODO(roasbeef): much cleanup + de-duplication
	ourBalance := pendingReservation.partialState.OurBalance
	theirBalance := pendingReservation.partialState.TheirBalance
	ourCommitKey := ourContribution.CommitKey
	ourCommitTx, err := CreateCommitTx(fundingTxIn, ourCommitKey, theirCommitKey,
		ourRevokeKey, ourContribution.CsvDelay,
		ourBalance, theirBalance)
	if err != nil {
		req.err <- err
		return
	}
	theirCommitTx, err := CreateCommitTx(fundingTxIn, theirCommitKey, ourCommitKey,
		theirContribution.RevocationKey, theirContribution.CsvDelay,
		theirBalance, ourBalance)
	if err != nil {
		req.err <- err
		return
	}

	// With both commitment transactions constructed, generate the state
	// obsfucator then use it to encode the current state number withi both
	// commitment transactions.
	// TODO(roasbeef): define obsfucator scheme for dual funder
	var stateObsfucator [StateHintSize]byte
	if pendingReservation.partialState.IsInitiator {
		stateObsfucator, err = deriveStateHintObsfucator(elkremSender)
		if err != nil {
			req.err <- err
			return
		}
	}
	err = initStateHints(ourCommitTx, theirCommitTx, stateObsfucator)
	if err != nil {
		req.err <- err
		return
	}

	// Sort both transactions according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(ourCommitTx)
	txsort.InPlaceSort(theirCommitTx)

	deliveryScript, err := txscript.PayToAddrScript(theirContribution.DeliveryAddress)
	if err != nil {
		req.err <- err
		return
	}

	// Record newly available information witin the open channel state.
	pendingReservation.partialState.RemoteCsvDelay = theirContribution.CsvDelay
	pendingReservation.partialState.TheirDeliveryScript = deliveryScript
	pendingReservation.partialState.ChanID = fundingOutpoint
	pendingReservation.partialState.TheirCommitKey = theirCommitKey
	pendingReservation.partialState.TheirMultiSigKey = theirContribution.MultiSigKey
	pendingReservation.partialState.OurCommitTx = ourCommitTx
	pendingReservation.partialState.StateHintObsfucator = stateObsfucator
	pendingReservation.ourContribution.RevocationKey = ourRevokeKey

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
	sigTheirCommit, err := l.Signer.SignOutputRaw(theirCommitTx, &signDesc)
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
		req.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Simply record the counterparty's contribution into the pending
	// reservation data as they'll be solely funding the channel entirely.
	pendingReservation.theirContribution = req.contribution
	theirContribution := pendingReservation.theirContribution

	// Additionally, we can now also record the redeem script of the
	// funding transaction.
	// TODO(roasbeef): switch to proper pubkey derivation
	ourKey := pendingReservation.partialState.OurMultiSigKey
	theirKey := theirContribution.MultiSigKey
	channelCapacity := int64(pendingReservation.partialState.Capacity)
	witnessScript, _, err := GenFundingPkScript(ourKey.SerializeCompressed(),
		theirKey.SerializeCompressed(), channelCapacity)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.partialState.FundingWitnessScript = witnessScript

	masterElkremRoot, err := l.deriveMasterElkremRoot()
	if err != nil {
		req.err <- err
		return
	}

	// Now that we know their commitment key, we can create the revocation
	// key for our version of the initial commitment transaction.
	elkremRoot := deriveElkremRoot(masterElkremRoot, ourKey, theirKey)
	elkremSender := elkrem.NewElkremSender(elkremRoot)
	firstPreimage, err := elkremSender.AtIndex(0)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.partialState.LocalElkrem = elkremSender
	theirCommitKey := theirContribution.CommitKey
	ourRevokeKey := DeriveRevocationPubkey(theirCommitKey, firstPreimage[:])

	// Initialize an empty sha-chain for them, tracking the current pending
	// revocation hash (we don't yet know the preimage so we can't add it
	// to the chain).
	remoteElkrem := &elkrem.ElkremReceiver{}
	pendingReservation.partialState.RemoteElkrem = remoteElkrem

	// Record the counterpaty's remaining contributions to the channel,
	// converting their delivery address into a public key script.
	deliveryScript, err := txscript.PayToAddrScript(theirContribution.DeliveryAddress)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.partialState.RemoteCsvDelay = theirContribution.CsvDelay
	pendingReservation.partialState.TheirDeliveryScript = deliveryScript
	pendingReservation.partialState.TheirCommitKey = theirContribution.CommitKey
	pendingReservation.partialState.TheirMultiSigKey = theirContribution.MultiSigKey
	pendingReservation.ourContribution.RevocationKey = ourRevokeKey

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
		msg.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
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
			prevOut := txin.PreviousOutPoint
			output, err := l.ChainIO.GetUtxo(&prevOut.Hash, prevOut.Index)
			if output == nil {
				msg.err <- fmt.Errorf("input to funding tx does not exist: %v", err)
				return
			}

			// Ensure that the witness+sigScript combo is valid.
			vm, err := txscript.NewEngine(output.PkScript,
				fundingTx, i, txscript.StandardVerifyFlags, nil,
				fundingHashCache, output.Value)
			if err != nil {
				// TODO(roasbeef): cancel at this stage if invalid sigs?
				msg.err <- fmt.Errorf("cannot create script engine: %s", err)
				return
			}
			if err = vm.Execute(); err != nil {
				msg.err <- fmt.Errorf("cannot validate transaction: %s", err)
				return
			}

			sigIndex++
		}
	}

	// At this point, we can also record and verify their signature for our
	// commitment transaction.
	res.theirCommitmentSig = msg.theirCommitmentSig
	commitTx := res.partialState.OurCommitTx
	theirKey := res.theirContribution.MultiSigKey

	// Re-generate both the witnessScript and p2sh output. We sign the
	// witnessScript script, but include the p2sh output as the subscript
	// for verification.
	witnessScript := res.partialState.FundingWitnessScript

	// Next, create the spending scriptSig, and then verify that the script
	// is complete, allowing us to spend from the funding transaction.
	theirCommitSig := msg.theirCommitmentSig
	channelValue := int64(res.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(commitTx)
	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, hashCache,
		txscript.SigHashAll, commitTx, 0, channelValue)
	if err != nil {
		msg.err <- fmt.Errorf("counterparty's commitment signature is invalid: %v", err)
		return
	}

	// Verify that we've received a valid signature from the remote party
	// for our version of the commitment transaction.
	sig, err := btcec.ParseSignature(theirCommitSig, btcec.S256())
	if err != nil {
		msg.err <- err
		return
	} else if !sig.Verify(sigHash, theirKey) {
		msg.err <- fmt.Errorf("counterparty's commitment signature is invalid")
		return
	}
	res.partialState.OurCommitSig = theirCommitSig

	// Funding complete, this entry can be removed from limbo.
	l.limboMtx.Lock()
	delete(l.fundingLimbo, res.reservationID)
	l.limboMtx.Unlock()

	walletLog.Infof("Broadcasting funding tx for ChannelPoint(%v): %v",
		res.partialState.FundingOutpoint, spew.Sdump(fundingTx))

	// Broacast the finalized funding transaction to the network.
	if err := l.PublishTransaction(fundingTx); err != nil {
		msg.err <- err
		return
	}

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	// TODO(roasbeef): revisit faul-tolerance of this flow
	nodeAddr := res.nodeAddr
	if err := res.partialState.FullSyncWithAddr(nodeAddr); err != nil {
		msg.err <- err
		return
	}

	// Create a goroutine to watch the chain so we can open the channel once
	// the funding tx has enough confirmations.
	go l.openChannelAfterConfirmations(res)

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
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	pendingReservation.partialState.FundingOutpoint = req.fundingOutpoint
	pendingReservation.partialState.TheirCurrentRevocation = req.revokeKey
	pendingReservation.partialState.ChanID = req.fundingOutpoint
	pendingReservation.partialState.StateHintObsfucator = req.obsfucator
	fundingTxIn := wire.NewTxIn(req.fundingOutpoint, nil, nil)

	// Now that we have the funding outpoint, we can generate both versions
	// of the commitment transaction, and generate a signature for the
	// remote node's commitment transactions.
	ourCommitKey := pendingReservation.ourContribution.CommitKey
	theirCommitKey := pendingReservation.theirContribution.CommitKey
	ourBalance := pendingReservation.partialState.OurBalance
	theirBalance := pendingReservation.partialState.TheirBalance
	ourCommitTx, err := CreateCommitTx(fundingTxIn, ourCommitKey, theirCommitKey,
		pendingReservation.ourContribution.RevocationKey,
		pendingReservation.ourContribution.CsvDelay, ourBalance, theirBalance)
	if err != nil {
		req.err <- err
		return
	}
	theirCommitTx, err := CreateCommitTx(fundingTxIn, theirCommitKey, ourCommitKey,
		req.revokeKey, pendingReservation.theirContribution.CsvDelay,
		theirBalance, ourBalance)
	if err != nil {
		req.err <- err
		return
	}

	// With both commitment transactions constructed, generate the state
	// obsfucator then use it to encode the current state number within
	// both commitment transactions.
	err = initStateHints(ourCommitTx, theirCommitTx, req.obsfucator)
	if err != nil {
		req.err <- err
		return
	}

	// Sort both transactions according to the agreed upon cannonical
	// ordering. This ensures that both parties sign the same sighash
	// without further synchronization.
	txsort.InPlaceSort(ourCommitTx)
	pendingReservation.partialState.OurCommitTx = ourCommitTx
	txsort.InPlaceSort(theirCommitTx)

	witnessScript := pendingReservation.partialState.FundingWitnessScript
	channelValue := int64(pendingReservation.partialState.Capacity)
	hashCache := txscript.NewTxSigHashes(ourCommitTx)
	theirKey := pendingReservation.theirContribution.MultiSigKey
	ourKey := pendingReservation.partialState.OurMultiSigKey

	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, hashCache,
		txscript.SigHashAll, ourCommitTx, 0, channelValue)
	if err != nil {
		req.err <- err
		return
	}

	// Verify that we've received a valid signature from the remote party
	// for our version of the commitment transaction.
	sig, err := btcec.ParseSignature(req.theirCommitmentSig, btcec.S256())
	if err != nil {
		req.err <- err
		return
	} else if !sig.Verify(sigHash, theirKey) {
		req.err <- fmt.Errorf("counterparty's commitment signature is invalid")
		return
	}
	pendingReservation.partialState.OurCommitSig = req.theirCommitmentSig

	// With their signature for our version of the commitment transactions
	// verified, we can now generate a signature for their version,
	// allowing the funding transaction to be safely broadcast.
	p2wsh, err := witnessScriptHash(witnessScript)
	if err != nil {
		req.err <- err
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
	sigTheirCommit, err := l.Signer.SignOutputRaw(theirCommitTx, &signDesc)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.ourCommitmentSig = sigTheirCommit

	req.err <- nil
}

// handleChannelOpen completes a single funder reservation to which we are the
// responder. This method saves the channel state to disk, finally "opening"
// the channel by sending it over to the caller of the reservation via the
// channel dispatch channel.
func (l *LightningWallet) handleChannelOpen(req *channelOpenMsg) {
	l.limboMtx.RLock()
	res, ok := l.fundingLimbo[req.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existant " +
			"funding state")
		res.chanOpen <- nil
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	res.Lock()
	defer res.Unlock()

	// Funding complete, this entry can be removed from limbo.
	l.limboMtx.Lock()
	delete(l.fundingLimbo, res.reservationID)
	l.limboMtx.Unlock()

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	if err := res.partialState.FullSyncWithAddr(res.nodeAddr); err != nil {
		req.err <- err
		res.chanOpen <- nil
		return
	}

	// Finally, create and officially open the payment channel!
	// TODO(roasbeef): CreationTime once tx is 'open'
	channel, _ := NewLightningChannel(l.Signer, l.ChainIO, l.chainNotifier,
		res.partialState)

	res.chanOpen <- &openChanDetails{
		channel: channel,
	}
	req.err <- nil
}

// openChannelAfterConfirmations creates, and opens a payment channel after
// the funding transaction created within the passed channel reservation
// obtains the specified number of confirmations.
func (l *LightningWallet) openChannelAfterConfirmations(res *ChannelReservation) {
	// Register with the ChainNotifier for a notification once the funding
	// transaction reaches `numConfs` confirmations.
	txid := res.fundingTx.TxHash()
	numConfs := uint32(res.numConfsToOpen)
	confNtfn, _ := l.chainNotifier.RegisterConfirmationsNtfn(&txid, numConfs)

	walletLog.Infof("Waiting for funding tx (txid: %v) to reach %v confirmations",
		txid, numConfs)

	// Wait until the specified number of confirmations has been reached,
	// or the wallet signals a shutdown.
	var (
		confDetails *chainntnfs.TxConfirmation
		ok          bool
	)
out:
	select {
	case confDetails, ok = <-confNtfn.Confirmed:
		// Reading a falsey value for the second parameter indicates that
		// the notifier is in the process of shutting down. Therefore, we
		// don't count this as the signal that the funding transaction has
		// been confirmed.
		if !ok {
			res.chanOpen <- nil
			return
		}

		break out
	case <-l.quit:
		res.chanOpen <- nil
		return
	}

	// Finally, create and officially open the payment channel!
	// TODO(roasbeef): CreationTime once tx is 'open'
	channel, _ := NewLightningChannel(l.Signer, l.ChainIO, l.chainNotifier,
		res.partialState)
	res.chanOpen <- &openChanDetails{
		channel:     channel,
		blockHeight: confDetails.BlockHeight,
		txIndex:     confDetails.TxIndex,
	}
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

	// Find all unlocked unspent witness outputs with greater than 1
	// confirmation.
	// TODO(roasbeef): make num confs a configuration paramter
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

// deriveMasterElkremRoot derives the private key which serves as the master
// elkrem root. This master secret is used as the secret input to a HKDF to
// generate elkrem secrets based on random, but public data.
func (l *LightningWallet) deriveMasterElkremRoot() (*btcec.PrivateKey, error) {
	masterElkremRoot, err := l.rootKey.Child(elkremRootIndex)
	if err != nil {
		return nil, err
	}

	return masterElkremRoot.ECPrivKey()
}

// deriveStateHintObsfucator derives the bytes to be used for obsfucatating the
// state hints from the elkerem root to be used for a new channel. The
// obsfucator is generated by performing an additional sha256 hash of the first
// child derived from the elkrem root. The leading 4 bytes are used for the
// obsfucator.
func deriveStateHintObsfucator(elkremRoot *elkrem.ElkremSender) ([StateHintSize]byte, error) {
	var obsfucator [StateHintSize]byte

	firstChild, err := elkremRoot.AtIndex(0)
	if err != nil {
		return obsfucator, err
	}

	grandChild := fastsha256.Sum256(firstChild[:])
	copy(obsfucator[:], grandChild[:])

	return obsfucator, nil
}

// initStateHints properly sets the obsfucated state ints ob both commitment
// transactions using the passed obsfucator.
func initStateHints(commit1, commit2 *wire.MsgTx,
	obsfucator [StateHintSize]byte) error {

	if err := SetStateNumHint(commit1, 0, obsfucator); err != nil {
		return err
	}
	if err := SetStateNumHint(commit2, 0, obsfucator); err != nil {
		return err
	}

	return nil
}

// selectInputs selects a slice of inputs necessary to meet the specified
// selection amount. If input selection is unable to suceed to to insuffcient
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

// coinSelect attemps to select a sufficient amount of coins, including a
// change output to fund amt satoshis, adhearing to the specified fee rate. The
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

		// The difference bteween the selected amount and the amount
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

		// If the fee is sufficient, then calculate the size of the change output.
		changeAmt := overShootAmt - requiredFee

		return selectedUtxos, changeAmt, nil
	}
}
