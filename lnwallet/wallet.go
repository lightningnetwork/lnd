package lnwallet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/LightningNetwork/lnd/elkrem"
	"github.com/lightningnetwork/lnd/chainntfs"
	"github.com/lightningnetwork/lnd/chainntfs/btcdnotify"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcrpcclient"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
)

const (
	// The size of the buffered queue of request to the wallet from the
	// outside word.
	msgBufferSize = 100
)

var (
	// Error types
	ErrInsufficientFunds = errors.New("not enough available outputs to " +
		"create funding transaction")

	// Namespace bucket keys.
	lightningNamespaceKey = []byte("ln-wallet")
	waddrmgrNamespaceKey  = []byte("waddrmgr")
	wtxmgrNamespaceKey    = []byte("wtxmgr")
)

// FundingType represents the type of the funding transaction. The type of
// funding transaction available depends entirely on the level of upgrades to
// Script on the current network. Across the network it's possible for asymmetric
// funding types to exist across hop. However, for direct links, the funding type
// supported by both parties must be identical. The most 'powerful' funding type
// is SEGWIT. This funding type also assumes that both CSV+CLTV are available on
// the network.
// NOTE: Ultimately, this will most likely be deprecated...
type FundingType uint16

const (
	// Use SegWit, assumes CSV+CLTV
	SEGWIT FundingType = iota

	// Use SIGHASH_NOINPUT, assumes CSV+CLTV
	SIGHASH

	// Use CSV without reserve
	CSV

	// Use CSV with reserve
	// Reserve is a permanent amount of funds locked and the capacity.
	CSV_RESERVE

	// CLTV with reserve.
	CLTV_RESERVE
)

// initFundingReserveReq is the first message sent to initiate the workflow
// required to open a payment channel with a remote peer. The initial required
// paramters are configurable accross channels. These paramters are to be chosen
// depending on the fee climate within the network, and time value of funds to
// be locked up within the channel. Upon success a ChannelReservation will be
// created in order to track the lifetime of this pending channel. Outputs
// selected will be 'locked', making them unavailable, for any other pending
// reservations. Therefore, all channels in reservation limbo will be periodically
// after a timeout period in order to avoid "exhaustion" attacks.
// NOTE: The workflow currently assumes fully balanced symmetric channels.
// Meaning both parties must encumber the same amount of funds.
// TODO(roasbeef): zombie reservation sweeper goroutine.
type initFundingReserveMsg struct {
	// The type of the funding transaction. See above for further details.
	fundingType FundingType

	// The amount of funds requested for this channel.
	fundingAmount btcutil.Amount

	// The minimum accepted satoshis/KB fee for the funding transaction. In
	// order to ensure timely confirmation, it is recomened that this fee
	// should be generous, paying some multiple of the accepted base fee
	// rate of the network.
	// TODO(roasbeef): integrate fee estimation project...
	minFeeRate btcutil.Amount

	// The ID of the remote node we would like to open a channel with.
	nodeID [32]byte

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
	theirFundingSigs [][]byte

	// This should be 1/2 of the signatures needed to succesfully spend our
	// version of the commitment transaction.
	theirCommitmentSig []byte

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
// operations. Interaction has been designed independant of any peer-to-peer
// communication protocol, allowing the wallet to be self-contained and embeddable
// within future projects interacting with the Lightning Network.
// NOTE: At the moment the wallet requires a btcd full node, as it's dependant
// on btcd's websockets notifications as even triggers during the lifetime of
// a channel. However, once the chainntnfs package is complete, the wallet
// will be compatible with multiple RPC/notification services such as Electrum,
// Bitcoin Core + ZeroMQ, etc. Eventually, the wallet won't require a full-node
// at all, as SPV support is integrated inot btcwallet.
type LightningWallet struct {
	// This mutex is to be held when generating external keys to be used
	// as multi-sig, and commitment keys within the channel.
	KeyGenMtx sync.RWMutex

	// This mutex MUST be held when performing coin selection in order to
	// avoid inadvertently creating multiple funding transaction which
	// double spend inputs accross each other.
	coinSelectMtx sync.RWMutex

	// A wrapper around a namespace within boltdb reserved for ln-based
	// wallet meta-data. See the 'channeldb' package for further
	// information.
	channelDB *channeldb.DB

	// Used by in order to obtain notifications about funding transaction
	// reaching a specified confirmation depth, and to catch
	// counterparty's broadcasting revoked commitment states.
	chainNotifier chainntnfs.ChainNotifier

	// The core wallet, all non Lightning Network specific interaction is
	// proxied to the internal wallet.
	*btcwallet.Wallet

	// An active RPC connection to a full-node. In the case of a btcd node,
	// websockets are used for notifications. If using Bitcoin Core,
	// notifications are either generated via long-polling or the usage of
	// ZeroMQ.
	// TODO(roasbeef): make into interface need: getrawtransaction + gettxout
	//  * getrawtransaction -> verify proof of channel links
	//  * gettxout -> verify inputs to funding tx exist and are unspent
	rpc *chain.RPCClient

	// All messages to the wallet are to be sent accross this channel.
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

	cfg *Config

	started  int32
	shutdown int32
	quit     chan struct{}

	wg sync.WaitGroup

	// TODO(roasbeef): handle wallet lock/unlock
}

// NewLightningWallet creates/opens and initializes a LightningWallet instance.
// If the wallet has never been created (according to the passed dataDir), first-time
// setup is executed.
// TODO(roasbeef): fin...add config
func NewLightningWallet(config *Config, cdb *channeldb.DB) (*LightningWallet, error) {
	// Ensure the wallet exists or create it when the create flag is set.
	netDir := networkDir(config.DataDir, config.NetParams)

	var pubPass []byte
	if config.PublicPass == nil {
		pubPass = defaultPubPassphrase
	} else {
		pubPass = config.PublicPass
	}

	loader := btcwallet.NewLoader(config.NetParams, netDir)
	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}

	var createID bool
	var wallet *btcwallet.Wallet
	if !walletExists {
		// Wallet has never been created, perform initial set up.
		wallet, err = loader.CreateNewWallet(pubPass, config.PrivatePass,
			config.HdSeed)
		if err != nil {
			return nil, err
		}

		createID = true
	} else {
		// Wallet has been created and been initialized at this point, open it
		// along with all the required DB namepsaces, and the DB itself.
		wallet, err = loader.OpenExistingWallet(pubPass, false)
		if err != nil {
			return nil, err
		}
	}

	if err := wallet.Manager.Unlock(config.PrivatePass); err != nil {
		return nil, err
	}

	// If we just created the wallet, then reserve, and store a key for
	// our ID within the Lightning Network.
	if createID {
		adrs, err := wallet.Manager.NextInternalAddresses(waddrmgr.DefaultAccountNum, 1)
		if err != nil {
			return nil, err
		}

		idPubkeyHash := adrs[0].Address().ScriptAddress()
		if err := cdb.PutIdKey(idPubkeyHash); err != nil {
			return nil, err
		}
		log.Infof("stored identity key pubkey hash in channeldb")
	}

	// Create a special websockets rpc client for btcd which will be used
	// by the wallet for notifications, calls, etc.
	rpcc, err := chain.NewRPCClient(config.NetParams, config.RpcHost,
		config.RpcUser, config.RpcPass, config.CACert, false, 20)
	if err != nil {
		return nil, err
	}

	// Using the same authentication info, create a config for a second
	// rpcclient which will be used by the current default chain
	// notifier implemenation.
	rpcConfig := &btcrpcclient.ConnConfig{
		Host:                 config.RpcHost,
		Endpoint:             "ws",
		User:                 config.RpcUser,
		Pass:                 config.RpcPass,
		Certificates:         config.CACert,
		DisableTLS:           false,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
	}
	chainNotifier, err := btcdnotify.NewBtcdNotifier(rpcConfig)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): logging
	return &LightningWallet{
		chainNotifier: chainNotifier,
		rpc:           rpcc,
		Wallet:        wallet,
		channelDB:     cdb,
		msgChan:       make(chan interface{}, msgBufferSize),
		// TODO(roasbeef): make this atomic.Uint32 instead? Which is
		// faster, locks or CAS? I'm guessing CAS because assembly:
		//  * https://golang.org/src/sync/atomic/asm_amd64.s
		nextFundingID: 0,
		cfg:           config,
		fundingLimbo:  make(map[uint64]*ChannelReservation),
		quit:          make(chan struct{}),
	}, nil
}

// Startup establishes a connection to the RPC source, and spins up all
// goroutines required to handle incoming messages.
func (l *LightningWallet) Startup() error {
	// Already started?
	if atomic.AddInt32(&l.started, 1) != 1 {
		return nil
	}

	// Establish an RPC connection in additino to starting the goroutines
	// in the underlying wallet.
	if err := l.rpc.Start(); err != nil {
		return err
	}
	l.Start()

	// Start the notification server. This is used so channel managment
	// goroutines can be notified when a funding transaction reaches a
	// sufficient number of confirmations, or when the input for the funding
	// transaction is spent in an attempt at an uncooperative close by the
	// counter party.
	if err := l.chainNotifier.Start(); err != nil {
		return err
	}

	// Pass the rpc client into the wallet so it can sync up to the current
	// main chain.
	l.SynchronizeRPC(l.rpc)

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

	l.Stop()

	l.rpc.Shutdown()

	l.chainNotifier.Stop()

	close(l.quit)
	l.wg.Wait()
	return nil
}

// requestHandler is the primary goroutine(s) resposible for handling, and
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
			case *addContributionMsg:
				l.handleContributionMsg(msg)
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

// InitChannelReservation kicks off the 3-step workflow required to succesfully
// open a payment channel with a remote node. As part of the funding
// reservation, the inputs selected for the funding transaction are 'locked'.
// This ensures that multiple channel reservations aren't double spending the
// same inputs in the funding transaction. If reservation initialization is
// succesful, a ChannelReservation containing our completed contribution is
// returned. Our contribution contains all the items neccessary to allow the
// counter party to build the funding transaction, and both versions of the
// commitment transaction. Otherwise, an error occured a nil pointer along with
// an error are returned.
//
// Once a ChannelReservation has been obtained, two
// additional steps must be processed before a payment channel can be considered
// 'open'. The second step validates, and processes the counterparty's channel
// contribution. The third, and final step verifies all signatures for the inputs
// of the funding transaction, and that the signature we records for our version
// of the commitment transaction is valid.
func (l *LightningWallet) InitChannelReservation(a btcutil.Amount, t FundingType,
	theirID [32]byte, csvDelay uint32) (*ChannelReservation, error) {

	errChan := make(chan error, 1)
	respChan := make(chan *ChannelReservation, 1)

	l.msgChan <- &initFundingReserveMsg{
		fundingAmount: a,
		fundingType:   t,
		csvDelay:      csvDelay,
		nodeID:        theirID,
		err:           errChan,
		resp:          respChan,
	}

	return <-respChan, <-errChan
}

// handleFundingReserveRequest processes a message intending to create, and
// validate a funding reservation request.
func (l *LightningWallet) handleFundingReserveRequest(req *initFundingReserveMsg) {
	// Create a limbo and record entry for this newly pending funding request.
	l.limboMtx.Lock()

	id := l.nextFundingID
	reservation := newChannelReservation(req.fundingType, req.fundingAmount, req.minFeeRate, l, id)
	l.nextFundingID++
	l.fundingLimbo[id] = reservation

	l.limboMtx.Unlock()

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	reservation.Lock()
	defer reservation.Unlock()

	reservation.partialState.TheirLNID = req.nodeID
	ourContribution := reservation.ourContribution
	ourContribution.CsvDelay = req.csvDelay
	reservation.partialState.LocalCsvDelay = req.csvDelay

	// We hold the coin select mutex while querying for outputs, and
	// performing coin selection in order to avoid inadvertent double spends
	// accross funding transactions.
	// NOTE: we don't use defer her so we can properly release the lock
	// when we encounter an error condition.
	l.coinSelectMtx.Lock()

	// TODO(roasbeef): check if balance is insufficient, if so then select
	// on two channels, one is a time.After that will bail out with
	// insuffcient funds, the other is a notification that the balance has
	// been updated make(chan struct{}, 1).

	// Find all unlocked unspent outputs with greater than 6 confirmations.
	maxConfs := int32(math.MaxInt32)
	// TODO(roasbeef): make 6 a config paramter?
	unspentOutputs, err := l.ListUnspent(6, maxConfs, nil)
	if err != nil {
		l.coinSelectMtx.Unlock()
		req.err <- err
		req.resp <- nil
		return
	}

	// Convert the outputs to coins for coin selection below.
	coins, err := outputsToCoins(unspentOutputs)
	if err != nil {
		l.coinSelectMtx.Unlock()
		req.err <- err
		req.resp <- nil
		return
	}

	// Peform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount requirements.
	//
	// TODO(roasbeef): Should extend coinset with optimal coin selection
	// heuristics for our use case.
	// TODO(roasbeef): factor in fees..
	// TODO(roasbeef): possibly integrate the fee prediction project? if
	// results hold up...
	// NOTE: this current selection assumes "priority" is still a thing.
	selector := &coinset.MaxValueAgeCoinSelector{
		MaxInputs:       10,
		MinChangeAmount: 10000,
	}
	selectedCoins, err := selector.CoinSelect(req.fundingAmount, coins)
	if err != nil {
		l.coinSelectMtx.Unlock()
		req.err <- err
		req.resp <- nil
		return
	}

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	ourContribution.Inputs = make([]*wire.TxIn, len(selectedCoins.Coins()))
	for i, coin := range selectedCoins.Coins() {
		txout := wire.NewOutPoint(coin.Hash(), coin.Index())
		l.LockOutpoint(*txout)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		outPoint := wire.NewOutPoint(coin.Hash(), coin.Index())
		ourContribution.Inputs[i] = wire.NewTxIn(outPoint, nil, nil)
	}

	l.coinSelectMtx.Unlock()

	// Create some possibly neccessary change outputs.
	selectedTotalValue := coinset.NewCoinSet(selectedCoins.Coins()).TotalValue()
	if selectedTotalValue > req.fundingAmount {
		ourContribution.ChangeOutputs = make([]*wire.TxOut, 1)
		// Change is necessary. Query for an available change address to
		// send the remainder to.
		changeAddr, err := l.NewChangeAddress(waddrmgr.DefaultAccountNum)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}

		changeAddrScript, err := txscript.PayToAddrScript(changeAddr)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}

		changeAmount := selectedTotalValue - req.fundingAmount
		ourContribution.ChangeOutputs[0] = wire.NewTxOut(int64(changeAmount),
			changeAddrScript)
	}

	// TODO(roasbeef): re-calculate fees here to minFeePerKB, may need more inputs

	// Grab two fresh keys from out HD chain, one will be used for the
	// multi-sig funding transaction, and the other for the commitment
	// transaction.
	multiSigKey, err := l.getNextRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	commitKey, err := l.getNextRawKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	reservation.partialState.MultiSigKey = multiSigKey
	ourContribution.MultiSigKey = multiSigKey.PubKey()
	reservation.partialState.OurCommitKey = commitKey
	ourContribution.CommitKey = commitKey.PubKey()

	// Generate a fresh address to be used in the case of a cooperative
	// channel close.
	deliveryAddress, err := l.NewAddress(waddrmgr.DefaultAccountNum)
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

	// Create a new elkrem for verifiable transaction revocations. This
	// will be used to generate revocation hashes for our past/current
	// commitment transactions once we start to make payments within the
	// channel.
	// TODO(roabeef): should be HMAC based...REMOVE BEFORE ALPHA
	var zero wire.ShaHash
	elkremSender := elkrem.NewElkremSender(63, zero)
	reservation.partialState.LocalElkrem = &elkremSender
	firstPrimage, err := elkremSender.AtIndex(0)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}
	copy(ourContribution.RevocationHash[:], btcutil.Hash160(firstPrimage[:]))

	// Funding reservation request succesfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or cancecled.
	req.resp <- reservation
	req.err <- nil
}

// handleFundingReserveCancel cancels an existing channel reservation. As part
// of the cancellation, outputs previously selected as inputs for the funding
// transaction via coin selection are freed allowing future reservations to
// include them.
func (l *LightningWallet) handleFundingCancelRequest(req *fundingReserveCancelMsg) {
	// TODO(roasbeef): holding lock too long
	// RLOCK?
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
		l.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): is it even worth it to keep track of unsed keys?

	// TODO(roasbeef): Is it possible to mark the unused change also as
	// available?

	delete(l.fundingLimbo, req.pendingFundingID)

	req.err <- nil
}

// handleFundingCounterPartyFunds processes the second workflow step for the
// lifetime of a channel reservation. Upon completion, the reservation will
// carry a completed funding transaction (minus the counterparty's input
// signatures), both versions of the commitment transaction, and our signature
// for their version of the commitment transaction.
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
	pendingReservation.partialState.FundingTx = wire.NewMsgTx()
	fundingTx := pendingReservation.partialState.FundingTx

	// Some temporary variables to cut down on the resolution verbosity.
	pendingReservation.theirContribution = req.contribution
	theirContribution := req.contribution
	ourContribution := pendingReservation.ourContribution

	// First, add all multi-party inputs to the transaction
	// TODO(roasbeef); handle case that tx doesn't exist, fake input
	// TODO(roasbeef): validate SPV proof from other side if in SPV mode.
	//  * actually, pure SPV would need fraud proofs right? must prove input
	//    is unspent
	//  * or, something like getutxo?
	for _, ourInput := range ourContribution.Inputs {
		fundingTx.AddTxIn(ourInput)
	}
	for _, theirInput := range theirContribution.Inputs {
		fundingTx.AddTxIn(theirInput)
	}

	// Next, add all multi-party outputs to the transaction. This includes
	// change outputs for both side.
	for _, ourChangeOutput := range ourContribution.ChangeOutputs {
		fundingTx.AddTxOut(ourChangeOutput)
	}
	for _, theirChangeOutput := range theirContribution.ChangeOutputs {
		fundingTx.AddTxOut(theirChangeOutput)
	}

	ourKey := pendingReservation.partialState.MultiSigKey
	theirKey := theirContribution.MultiSigKey

	// Finally, add the 2-of-2 multi-sig output which will set up the lightning
	// channel.
	channelCapacity := int64(pendingReservation.partialState.Capacity)
	redeemScript, multiSigOut, err := fundMultiSigOut(ourKey.PubKey().SerializeCompressed(),
		theirKey.SerializeCompressed(), channelCapacity)
	if err != nil {
		req.err <- err
		return
	}

	// Register intent for notifications related to the funding output.
	// This'll allow us to properly track the number of confirmations the
	// funding tx has once it has been broadcasted.
	lastBlock := l.Manager.SyncedTo()
	scriptAddr, err := l.Manager.ImportScript(redeemScript, &lastBlock)
	if err != nil {
		req.err <- err
		return
	}
	if err := l.rpc.NotifyReceived([]btcutil.Address{scriptAddr.Address()}); err != nil {
		req.err <- err
		return
	}

	pendingReservation.partialState.FundingRedeemScript = redeemScript
	fundingTx.AddTxOut(multiSigOut)

	// Sort the transaction. Since both side agree to a cannonical
	// ordering, by sorting we no longer need to send the entire
	// transaction. Only signatures will be exchanged.
	txsort.InPlaceSort(pendingReservation.partialState.FundingTx)

	// Next, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	pendingReservation.ourFundingSigs = make([][]byte, 0, len(ourContribution.Inputs))
	for i, txIn := range fundingTx.TxIn {
		// Does the wallet know about the txin?
		txDetail, _ := l.TxStore.TxDetails(&txIn.PreviousOutPoint.Hash)
		if txDetail == nil {
			continue
		}

		// Is this our txin? TODO(roasbeef): assumes all inputs are P2PKH...
		prevIndex := txIn.PreviousOutPoint.Index
		prevOut := txDetail.TxRecord.MsgTx.TxOut[prevIndex]
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(prevOut.PkScript, l.cfg.NetParams)
		apkh, ok := addrs[0].(*btcutil.AddressPubKeyHash)
		if !ok {
			req.err <- fmt.Errorf("only p2pkh wallet outputs are supported")
		}

		ai, err := l.Manager.Address(apkh)
		if err != nil {
			req.err <- fmt.Errorf("cannot get address info: %v", err)
			return
		}
		pka := ai.(waddrmgr.ManagedPubKeyAddress)
		privkey, err := pka.PrivKey()
		if err != nil {
			req.err <- fmt.Errorf("cannot get private key: %v", err)
			return
		}

		sigscript, err := txscript.SignatureScript(pendingReservation.partialState.FundingTx, i,
			prevOut.PkScript, txscript.SigHashAll, privkey,
			ai.Compressed())
		if err != nil {
			req.err <- fmt.Errorf("cannot create sigscript: %s", err)
			return
		}

		fundingTx.TxIn[i].SignatureScript = sigscript
		pendingReservation.ourFundingSigs = append(pendingReservation.ourFundingSigs, sigscript)
	}

	// Initialize an empty sha-chain for them, tracking the current pending
	// revocation hash (we don't yet know the pre-image so we can't add it
	// to the chain).
	e := elkrem.NewElkremReceiver(63)
	// TODO(roasbeef): this is incorrect!! fix before lnstate integration
	var zero wire.ShaHash
	if err := e.AddNext(&zero); err != nil {
		req.err <- nil
		return
	}

	pendingReservation.partialState.RemoteElkrem = &e
	pendingReservation.partialState.TheirCurrentRevocation = theirContribution.RevocationHash

	// Grab the hash of the current pre-image in our chain, this is needed
	// for our commitment tx.
	// TODO(roasbeef): grab partial state above to avoid long attr chain
	ourCurrentRevokeHash := pendingReservation.ourContribution.RevocationHash

	// Create the txIn to our commitment transaction. In the process, we
	// need to locate the index of the multi-sig output on the funding tx
	// since the outputs are cannonically sorted.
	fundingNTxid := fundingTx.TxSha() // NOTE: assumes testnet-L
	_, multiSigIndex := findScriptOutputIndex(fundingTx, multiSigOut.PkScript)
	fundingTxIn := wire.NewTxIn(wire.NewOutPoint(&fundingNTxid, multiSigIndex), nil, nil)

	// With the funding tx complete, create both commitment transactions.
	initialBalance := ourContribution.FundingAmount
	pendingReservation.fundingLockTime = theirContribution.CsvDelay
	ourCommitKey := ourContribution.CommitKey
	theirCommitKey := theirContribution.CommitKey
	ourCommitTx, err := createCommitTx(fundingTxIn, ourCommitKey, theirCommitKey,
		ourCurrentRevokeHash[:], theirContribution.CsvDelay,
		initialBalance, initialBalance)
	if err != nil {
		req.err <- err
		return
	}
	theirCommitTx, err := createCommitTx(fundingTxIn, theirCommitKey, ourCommitKey,
		theirContribution.RevocationHash[:], theirContribution.CsvDelay,
		initialBalance, initialBalance)
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
	pendingReservation.partialState.ChanID = fundingNTxid
	pendingReservation.partialState.TheirCommitKey = theirCommitKey
	pendingReservation.partialState.TheirCommitTx = theirCommitTx
	pendingReservation.partialState.OurCommitTx = ourCommitTx

	// Generate a signature for their version of the initial commitment
	// transaction.
	sigTheirCommit, err := txscript.RawTxInSignature(theirCommitTx, 0, redeemScript,
		txscript.SigHashAll, ourKey)
	if err != nil {
		req.err <- err
		return
	}
	pendingReservation.ourCommitmentSig = sigTheirCommit

	req.err <- nil
}

// handleFundingCounterPartySigs is the final step in the channel reservation
// workflow. During this setp, we validate *all* the received signatures for
// inputs to the funding transaction. If any of these are invalid, we bail,
// and forcibly cancel this funding request. Additionally, we ensure that the
// signature we received from the counterparty for our version of the commitment
// transaction allows us to spend from the funding output with the addition of
// our signature.
func (l *LightningWallet) handleFundingCounterPartySigs(msg *addCounterPartySigsMsg) {
	l.limboMtx.RLock()
	pendingReservation, ok := l.fundingLimbo[msg.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		msg.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Now we can complete the funding transaction by adding their
	// signatures to their inputs.
	pendingReservation.theirFundingSigs = msg.theirFundingSigs
	fundingTx := pendingReservation.partialState.FundingTx
	sigIndex := 0
	for i, txin := range fundingTx.TxIn {
		if txin.SignatureScript == nil {
			// Attach the signature so we can verify it below.
			txin.SignatureScript = pendingReservation.theirFundingSigs[sigIndex]

			// Fetch the alleged previous output along with the
			// pkscript referenced by this input.
			prevOut := txin.PreviousOutPoint
			output, err := l.rpc.GetTxOut(&prevOut.Hash, prevOut.Index, false)
			if output == nil {
				// TODO(roasbeef): do this at the start to avoid wasting out time?
				//  8 or a set of nodes "we" run with exposed unauthenticated RPC?
				msg.err <- fmt.Errorf("input to funding tx does not exist: %v", err)
				return
			}
			pkscript, err := hex.DecodeString(output.ScriptPubKey.Hex)
			if err != nil {
				msg.err <- err
				return
			}

			// Ensure that the signature is valid.
			vm, err := txscript.NewEngine(pkscript,
				fundingTx, i, txscript.StandardVerifyFlags, nil)
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
	pendingReservation.theirCommitmentSig = msg.theirCommitmentSig
	commitTx := pendingReservation.partialState.OurCommitTx
	theirKey := pendingReservation.theirContribution.MultiSigKey
	ourKey := pendingReservation.partialState.MultiSigKey

	// Re-generate both the redeemScript and p2sh output. We sign the
	// redeemScript script, but include the p2sh output as the subscript
	// for verification.
	redeemScript := pendingReservation.partialState.FundingRedeemScript
	p2sh, err := scriptHashPkScript(redeemScript)
	if err != nil {
		msg.err <- err
		return
	}

	// First, we sign our copy of the commitment transaction ourselves.
	ourCommitSig, err := txscript.RawTxInSignature(commitTx, 0, redeemScript,
		txscript.SigHashAll, ourKey)
	if err != nil {
		msg.err <- err
		return
	}

	// Next, create the spending scriptSig, and then verify that the script
	// is complete, allowing us to spend from the funding transaction.
	theirCommitSig := msg.theirCommitmentSig
	ourKeySer := ourKey.PubKey().SerializeCompressed()
	theirKeySer := theirKey.SerializeCompressed()
	scriptSig, err := spendMultiSig(redeemScript, ourKeySer, ourCommitSig,
		theirKeySer, theirCommitSig)
	if err != nil {
		msg.err <- err
		return
	}

	// Finally, create an instance of a Script VM, and ensure that the
	// Script executes succesfully.
	commitTx.TxIn[0].SignatureScript = scriptSig
	vm, err := txscript.NewEngine(p2sh, commitTx, 0,
		txscript.StandardVerifyFlags, nil)
	if err != nil {
		msg.err <- err
		return
	}
	if err := vm.Execute(); err != nil {
		msg.err <- fmt.Errorf("counterparty's commitment signature is invalid: %v", err)
		return
	}

	// Funding complete, this entry can be removed from limbo.
	l.limboMtx.Lock()
	delete(l.fundingLimbo, pendingReservation.reservationID)
	// TODO(roasbeef): unlock outputs here, Store.InsertTx will handle marking
	// input in unconfirmed tx, so future coin selects don't pick it up
	//  * also record location of change address so can use AddCredit
	l.limboMtx.Unlock()

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	if err := pendingReservation.partialState.FullSync(); err != nil {
		msg.err <- err
		return
	}

	// Create a goroutine to watch the chain so we can open the channel once
	// the funding tx has enough confirmations.
	// TODO(roasbeef): add number of confs to the confi
	go l.openChannelAfterConfirmations(pendingReservation, 3)
	msg.err <- nil
}

// openChannelAfterConfirmations creates, and opens a payment channel after
// the funding transaction created within the passed channel reservation
// obtains the specified number of confirmations.
func (l *LightningWallet) openChannelAfterConfirmations(res *ChannelReservation, numConfs uint32) {
	// Register with the ChainNotifier for a notification once the funding
	// transaction reaches `numConfs` confirmations.
	txid := res.partialState.FundingTx.TxSha()
	confNtfn, _ := l.chainNotifier.RegisterConfirmationsNtfn(&txid, numConfs)

	// Wait until the specified number of confirmations has been reached,
	// or the wallet signals a shutdown.
out:
	select {
	case <-confNtfn.Confirmed:
		break out
	case <-l.quit:
		res.chanOpen <- nil
		return
	}

	// Finally, create and officially open the payment channel!
	// TODO(roasbeef): CreationTime once tx is 'open'
	channel, _ := newLightningChannel(l, l.chainNotifier, l.channelDB,
		res.partialState)
	res.chanOpen <- channel
}

// getNextRawKey retrieves the next key within our HD key-chain for use within
// as a multi-sig key within the funding transaction, or within the commitment
// transaction's outputs.
// TODO(roasbeef): on shutdown, write state of pending keys, then read back?
func (l *LightningWallet) getNextRawKey() (*btcec.PrivateKey, error) {
	l.KeyGenMtx.Lock()
	defer l.KeyGenMtx.Unlock()

	nextAddr, err := l.Manager.NextExternalAddresses(waddrmgr.DefaultAccountNum, 1)
	if err != nil {
		return nil, err
	}

	pkAddr := nextAddr[0].(waddrmgr.ManagedPubKeyAddress)

	return pkAddr.PrivKey()
}

type WaddrmgrEncryptorDecryptor struct {
	M *waddrmgr.Manager
}

func (w *WaddrmgrEncryptorDecryptor) Encrypt(p []byte) ([]byte, error) {
	return w.M.Encrypt(waddrmgr.CKTPrivate, p)
}

func (w *WaddrmgrEncryptorDecryptor) Decrypt(c []byte) ([]byte, error) {
	return w.M.Decrypt(waddrmgr.CKTPrivate, c)
}

func (w *WaddrmgrEncryptorDecryptor) OverheadSize() uint32 {
	return 24
}
