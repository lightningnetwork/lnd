package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/salsa20"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"google.golang.org/grpc"
)

const (
	// TODO(roasbeef): tune
	msgBufferSize = 50

	defaultCsvDelay = 4

	// maxFundingAmount is a soft-limit of the maximum channel size
	// accepted within the Lightning Protocol Currently. This limit is
	// currently defined in BOLT-0002, and serves as an initial
	// precaturioary limit while implementations are battle tested in the
	// real world.
	//
	// TODO(roasbeef): add command line param to modify
	maxFundingAmount = btcutil.Amount(1 << 24)

	// maxWaitNumBlocksFundingConf is the maximum number of blocks to wait
	// for the funding transaction to be confirmed before forgetting about
	// the channel. 288 blocks is ~48 hrs
	maxWaitNumBlocksFundingConf = 288
)

// reservationWithCtx encapsulates a pending channel reservation. This wrapper
// struct is used internally within the funding manager to track and progress
// the funding workflow initiated by incoming/outgoing methods from the target
// peer. Additionally, this struct houses a response and error channel which is
// used to respond to the caller in the case a channel workflow is initiated
// via a local signal such as RPC.
//
// TODO(roasbeef): actually use the context package
//  * deadlines, etc.
type reservationWithCtx struct {
	reservation *lnwallet.ChannelReservation
	peerAddress *lnwire.NetAddress

	chanAmt btcutil.Amount

	updates chan *lnrpc.OpenStatusUpdate
	err     chan error
}

// initFundingMsg is sent by an outside subsystem to the funding manager in
// order to kick off a funding workflow with a specified target peer. The
// original request which defines the parameters of the funding workflow are
// embedded within this message giving the funding manager full context w.r.t
// the workflow.
type initFundingMsg struct {
	peerAddress *lnwire.NetAddress
	*openChanReq
}

// fundingOpenMsg couples an lnwire.OpenChannel message with the peer who sent
// the message. This allows the funding manager to queue a response directly to
// the peer, progressing the funding workflow.
type fundingOpenMsg struct {
	msg         *lnwire.OpenChannel
	peerAddress *lnwire.NetAddress
}

// fundingAcceptMsg couples an lnwire.AcceptChannel message with the peer who
// sent the message. This allows the funding manager to queue a response
// directly to the peer, progressing the funding workflow.
type fundingAcceptMsg struct {
	msg         *lnwire.AcceptChannel
	peerAddress *lnwire.NetAddress
}

// fundingCreatedMsg couples an lnwire.FundingCreated message with the peer who
// sent the message. This allows the funding manager to queue a response
// directly to the peer, progressing the funding workflow.
type fundingCreatedMsg struct {
	msg         *lnwire.FundingCreated
	peerAddress *lnwire.NetAddress
}

// fundingSignedMsg couples an lnwire.FundingSigned message with the peer who
// sent the message. This allows the funding manager to queue a response
// directly to the peer, progressing the funding workflow.
type fundingSignedMsg struct {
	msg         *lnwire.FundingSigned
	peerAddress *lnwire.NetAddress
}

// fundingLockedMsg couples an lnwire.FundingLocked message with the peer who
// sent the message. This allows the funding manager to finalize the funding
// process and announce the existence of the new channel.
type fundingLockedMsg struct {
	msg         *lnwire.FundingLocked
	peerAddress *lnwire.NetAddress
}

// fundingErrorMsg couples an lnwire.Error message with the peer who sent the
// message. This allows the funding manager to properly process the error.
type fundingErrorMsg struct {
	err         *lnwire.Error
	peerAddress *lnwire.NetAddress
}

// pendingChannels is a map instantiated per-peer which tracks all active
// pending single funded channels indexed by their pending channel identifier,
// which is a set of 32-bytes generated via a CSPRNG.
type pendingChannels map[[32]byte]*reservationWithCtx

// serializedPubKey is used within the FundingManager's activeReservations list
// to identify the nodes with which the FundingManager is actively working to
// initiate new channels.
type serializedPubKey [33]byte

// newSerializedKey creates a new serialized public key from an instance of a
// live pubkey object.
func newSerializedKey(pubKey *btcec.PublicKey) serializedPubKey {
	var s serializedPubKey
	copy(s[:], pubKey.SerializeCompressed())
	return s
}

// fundingConfig defines the configuration for the FundingManager. All elements
// within the configuration MUST be non-nil for the FundingManager to carry out
// its duties.
type fundingConfig struct {
	// IDKey is the PublicKey that is used to identify this node within the
	// Lightning Network.
	IDKey *btcec.PublicKey

	// Wallet handles the parts of the funding process that involves moving
	// funds from on-chain transaction outputs into Lightning channels.
	Wallet *lnwallet.LightningWallet

	// FeeEstimator calculates appropriate fee rates based on historical
	// transaction information.
	FeeEstimator lnwallet.FeeEstimator

	// ArbiterChan allows the FundingManager to notify the BreachArbiter
	// that a new channel has been created that should be observed to
	// ensure that the channel counterparty hasn't broadcast an invalid
	// commitment transaction.
	ArbiterChan chan<- *lnwallet.LightningChannel

	// Notifier is used by the FundingManager to determine when the
	// channel's funding transaction has been confirmed on the blockchain
	// so that the channel creation process can be completed.
	Notifier chainntnfs.ChainNotifier

	// SignMessage signs an arbitrary method with a given public key. The
	// actual digest signed is the double sha-256 of the message. In the
	// case that the private key corresponding to the passed public key
	// cannot be located, then an error is returned.
	//
	// TODO(roasbeef): should instead pass on this responsibility to a
	// distinct sub-system?
	SignMessage func(pubKey *btcec.PublicKey, msg []byte) (*btcec.Signature, error)

	// CurrentNodeAnnouncement should return the latest, fully signed node
	// announcement from the backing Lighting Network node.
	CurrentNodeAnnouncement func() (lnwire.NodeAnnouncement, error)

	// SendAnnouncement is used by the FundingManager to announce newly
	// created channels to the rest of the Lightning Network.
	SendAnnouncement func(msg lnwire.Message) error

	// SendToPeer allows the FundingManager to send messages to the peer
	// node during the multiple steps involved in the creation of the
	// channel's funding transaction and initial commitment transaction.
	SendToPeer func(target *btcec.PublicKey, msgs ...lnwire.Message) error

	// FindPeer searches the list of peers connected to the node so that
	// the FundingManager can notify other daemon subsystems as necessary
	// during the funding process.
	FindPeer func(peerKey *btcec.PublicKey) (*peer, error)

	// FindChannel queries the database for the channel with the given
	// channel ID.
	FindChannel func(chanID lnwire.ChannelID) (*lnwallet.LightningChannel, error)

	// TempChanIDSeed is a cryptographically random string of bytes that's
	// used as a seed to generate pending channel ID's.
	TempChanIDSeed [32]byte

	// DefaultRoutingPolicy is the default routing policy used when
	// initially announcing channels.
	DefaultRoutingPolicy htlcswitch.ForwardingPolicy

	// NumRequiredConfs is a function closure that helps the funding
	// manager decide how many confirmations it should require for a
	// channel extended to it. The function is able to take into account
	// the amount of the channel, and any funds we'll be pushed in the
	// process to determine how many confirmations we'll require.
	NumRequiredConfs func(btcutil.Amount, lnwire.MilliSatoshi) uint16

	// RequiredRemoteDelay is a function that maps the total amount in a
	// proposed channel to the CSV delay that we'll require for the remote
	// party. Naturally a larger channel should require a higher CSV delay
	// in order to give us more time to claim funds in the case of a
	// contract breach.
	RequiredRemoteDelay func(btcutil.Amount) uint16
}

// fundingManager acts as an orchestrator/bridge between the wallet's
// 'ChannelReservation' workflow, and the wire protocol's funding initiation
// messages. Any requests to initiate the funding workflow for a channel,
// either kicked-off locally or remotely handled by the funding manager.
// Once a channel's funding workflow has been completed, any local callers, the
// local peer, and possibly the remote peer are notified of the completion of
// the channel workflow. Additionally, any temporary or permanent access
// controls between the wallet and remote peers are enforced via the funding
// manager.
type fundingManager struct {
	// MUST be used atomically.
	started int32
	stopped int32

	// cfg is a copy of the configuration struct that the FundingManager was
	// initialized with.
	cfg *fundingConfig

	// chanIDKey is a cryptographically random key that's used to generate
	// temporary channel ID's.
	chanIDKey [32]byte

	// chanIDNonce is a nonce that's incremented for each new funding
	// reservation created.
	nonceMtx    sync.RWMutex
	chanIDNonce uint64

	// activeReservations is a map which houses the state of all pending
	// funding workflows.
	activeReservations map[serializedPubKey]pendingChannels

	// signedReservations is a utility map that maps the permanent channel
	// ID of a funding reservation to its temporary channel ID. This is
	// required as mid funding flow, we switch to referencing the channel
	// by its full channel ID once the commitment transactions have been
	// signed by both parties.
	signedReservations map[lnwire.ChannelID][32]byte

	// resMtx guards both of the maps above to ensure that all access is
	// goroutine stafe.
	resMtx sync.RWMutex

	// fundingMsgs is a channel which receives wrapped wire messages
	// related to funding workflow from outside peers.
	fundingMsgs chan interface{}

	// queries is a channel which receives requests to query the internal
	// state of the funding manager.
	queries chan interface{}

	// fundingRequests is a channel used to receive channel initiation
	// requests from a local subsystem within the daemon.
	fundingRequests chan *initFundingMsg

	// newChanBarriers is a map from a channel ID to a 'barrier' which will
	// be signalled once the channel is fully open. This barrier acts as a
	// synchronization point for any incoming/outgoing HTLCs before the
	// channel has been fully opened.
	barrierMtx      sync.RWMutex
	newChanBarriers map[lnwire.ChannelID]chan struct{}

	localDiscoveryMtx     sync.Mutex
	localDiscoverySignals map[lnwire.ChannelID]chan struct{}

	quit chan struct{}
	wg   sync.WaitGroup
}

// channelOpeningState represents the different states a channel can be in
// between the funding transaction has been confirmed and the channel is
// announced to the network and ready to be used.
type channelOpeningState uint8

const (
	// markedOpen is the opening state of a channel if the funding
	// transaction is confirmed on-chain, but fundingLocked is not yet
	// successfully sent to the other peer.
	markedOpen channelOpeningState = iota

	// fundingLockedSent is the opening state of a channel if the
	// fundingLocked message has successfully been sent to the other peer,
	// but we still haven't announced the channel to the network.
	fundingLockedSent
)

var (
	// channelOpeningStateBucket is the database bucket used to store the
	// channelOpeningState for each channel that is currently in the process
	// of being opened.
	channelOpeningStateBucket = []byte("channelOpeningState")

	// ErrChannelNotFound is returned when we are looking for a specific
	// channel opening state in the FundingManager's internal database, but
	// the channel in question is not considered being in an opening state.
	ErrChannelNotFound = fmt.Errorf("channel not found in db")
)

// newFundingManager creates and initializes a new instance of the
// fundingManager.
func newFundingManager(cfg fundingConfig) (*fundingManager, error) {
	return &fundingManager{
		cfg:                   &cfg,
		chanIDKey:             cfg.TempChanIDSeed,
		activeReservations:    make(map[serializedPubKey]pendingChannels),
		signedReservations:    make(map[lnwire.ChannelID][32]byte),
		newChanBarriers:       make(map[lnwire.ChannelID]chan struct{}),
		fundingMsgs:           make(chan interface{}, msgBufferSize),
		fundingRequests:       make(chan *initFundingMsg, msgBufferSize),
		localDiscoverySignals: make(map[lnwire.ChannelID]chan struct{}),
		queries:               make(chan interface{}, 1),
		quit:                  make(chan struct{}),
	}, nil
}

// Start launches all helper goroutines required for handling requests sent
// to the funding manager.
func (f *fundingManager) Start() error {
	if atomic.AddInt32(&f.started, 1) != 1 { // TODO(roasbeef): CAS instead
		return nil
	}

	fndgLog.Tracef("Funding manager running")

	// Upon restart, the Funding Manager will check the database to load any
	// channels that were  waiting for their funding transactions to be
	// confirmed on the blockchain at the time when the daemon last went
	// down.
	// TODO(roasbeef): store height that funding finished?
	//  * would then replace call below
	pendingChannels, err := f.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		return err
	}

	// For any channels that were in a pending state when the daemon was
	// last connected, the Funding Manager will re-initialize the channel
	// barriers and will also launch waitForFundingConfirmation to wait for
	// the channel's funding transaction to be confirmed on the blockchain.
	for _, channel := range pendingChannels {
		f.barrierMtx.Lock()
		fndgLog.Tracef("Loading pending ChannelPoint(%v), creating chan "+
			"barrier", channel.FundingOutpoint)
		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
		f.newChanBarriers[chanID] = make(chan struct{})
		f.barrierMtx.Unlock()

		f.localDiscoverySignals[chanID] = make(chan struct{})

		doneChan := make(chan struct{})
		timeoutChan := make(chan struct{})

		go func(ch *channeldb.OpenChannel) {
			go f.waitForFundingWithTimeout(ch, doneChan, timeoutChan)

			select {
			case <-timeoutChan:
				// Timeout waiting for the funding transaction
				// to confirm, so we forget the channel and
				// delete it from the database.
				closeInfo := &channeldb.ChannelCloseSummary{
					ChanPoint: ch.FundingOutpoint,
					RemotePub: ch.IdentityPub,
					CloseType: channeldb.FundingCanceled,
				}
				if err := ch.CloseChannel(closeInfo); err != nil {
					fndgLog.Errorf("Failed closing channel "+
						"%v: %v", ch.FundingOutpoint, err)
				}
			case <-f.quit:
				// The fundingManager is shutting down, and will
				// resume wait on startup.
			case <-doneChan:
				// Success, funding transaction was confirmed.
			}
		}(channel)
	}

	// Fetch all our open channels, and make sure they all finalized the
	// opening process.
	// TODO(halseth): this check is only done on restart atm, but should
	// also be done if a peer that disappeared during the opening process
	// reconnects.
	openChannels, err := f.cfg.Wallet.Cfg.Database.FetchAllChannels()
	if err != nil {
		return err
	}

	for _, channel := range openChannels {
		channelState, shortChanID, err := f.getChannelOpeningState(
			&channel.FundingOutpoint)
		if err == ErrChannelNotFound {
			// Channel not in fundingManager's opening database,
			// meaning it was successully announced to the network.
			continue
		} else if err != nil {
			return err
		}

		fndgLog.Debugf("channel with opening state %v found",
			channelState)

		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
		f.localDiscoverySignals[chanID] = make(chan struct{})

		// If we did find the channel in the opening state database, we
		// have seen the funding transaction being confirmed, but we
		// did not finish the rest of the setup procedure before we shut
		// down. We handle the remaining steps of this setup by
		// continuing the procedure where we left off.
		switch channelState {
		case markedOpen:
			// The funding transaction was confirmed, but we did not
			// successfully send the fundingLocked message to the
			// peer, so let's do that now.
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.sendFundingLockedAndAnnounceChannel(channel,
					shortChanID)
			}()

		case fundingLockedSent:
			// fundingLocked was sent to peer, but the channel
			// announcement was not sent.
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()

				lnChannel, err := lnwallet.NewLightningChannel(
					nil, nil, f.cfg.FeeEstimator, channel)
				if err != nil {
					fndgLog.Errorf("error creating "+
						"lightning channel: %v", err)
				}
				defer lnChannel.Stop()

				f.sendChannelAnnouncement(channel, lnChannel,
					shortChanID)
			}()

		default:
			fndgLog.Errorf("undefined channelState: %v",
				channelState)
		}
	}

	f.wg.Add(1) // TODO(roasbeef): tune
	go f.reservationCoordinator()

	return nil
}

// Stop signals all helper goroutines to execute a graceful shutdown. This
// method will block until all goroutines have exited.
func (f *fundingManager) Stop() error {
	if atomic.AddInt32(&f.stopped, 1) != 1 {
		return nil
	}

	fndgLog.Infof("Funding manager shutting down")

	close(f.quit)
	f.wg.Wait()

	return nil
}

// nextPendingChanID returns the next free pending channel ID to be used to
// identify a particular future channel funding workflow.
func (f *fundingManager) nextPendingChanID() [32]byte {
	// Obtain a fresh nonce. We do this by encoding the current nonce
	// counter, then incrementing it by one.
	f.nonceMtx.Lock()
	var nonce [8]byte
	binary.LittleEndian.PutUint64(nonce[:], f.chanIDNonce)
	f.chanIDNonce++
	f.nonceMtx.Unlock()

	// We'll generate the next pending channelID by "encrypting" 32-bytes
	// of zeroes which'll extract 32 random bytes from our stream cipher.
	var (
		nextChanID [32]byte
		zeroes     [32]byte
	)
	salsa20.XORKeyStream(nextChanID[:], zeroes[:], nonce[:], &f.chanIDKey)

	return nextChanID
}

type pendingChannel struct {
	identityPub   *btcec.PublicKey
	channelPoint  *wire.OutPoint
	capacity      btcutil.Amount
	localBalance  btcutil.Amount
	remoteBalance btcutil.Amount
}

type pendingChansReq struct {
	resp chan []*pendingChannel
	err  chan error
}

// PendingChannels returns a slice describing all the channels which are
// currently pending at the last state of the funding workflow.
func (f *fundingManager) PendingChannels() ([]*pendingChannel, error) {
	respChan := make(chan []*pendingChannel, 1)
	errChan := make(chan error)

	req := &pendingChansReq{
		resp: respChan,
		err:  errChan,
	}
	f.queries <- req

	return <-respChan, <-errChan
}

// failFundingFlow will fail the active funding flow with the target peer,
// identified by it's unique temporary channel ID. This method is send an error
// to the remote peer, and also remove the reservation from our set of pending
// reservations.
//
// TODO(roasbeef): if peer disconnects, and haven't yet broadcast funding
// transaction, then all reservations should be cleared.
func (f *fundingManager) failFundingFlow(peer *btcec.PublicKey,
	tempChanID [32]byte, msg []byte) {

	errMsg := &lnwire.Error{
		ChanID: tempChanID,
		Data:   msg,
	}

	fndgLog.Errorf("Failing funding flow: %v", spew.Sdump(errMsg))

	err := f.cfg.SendToPeer(peer, errMsg)
	if err != nil {
		fndgLog.Errorf("unable to send error message to peer %v", err)
		return
	}

	f.cancelReservationCtx(peer, tempChanID)
	return
}

// reservationCoordinator is the primary goroutine tasked with progressing the
// funding workflow between the wallet, and any outside peers or local callers.
//
// NOTE: This MUST be run as a goroutine.
func (f *fundingManager) reservationCoordinator() {
	defer f.wg.Done()

	for {
		select {

		case msg := <-f.fundingMsgs:
			switch fmsg := msg.(type) {
			case *fundingOpenMsg:
				f.handleFundingOpen(fmsg)
			case *fundingAcceptMsg:
				f.handleFundingAccept(fmsg)
			case *fundingCreatedMsg:
				f.handleFundingCreated(fmsg)
			case *fundingSignedMsg:
				f.handleFundingSigned(fmsg)
			case *fundingLockedMsg:
				go f.handleFundingLocked(fmsg)
			case *fundingErrorMsg:
				f.handleErrorMsg(fmsg)
			}
		case req := <-f.fundingRequests:
			f.handleInitFundingMsg(req)

		case req := <-f.queries:
			switch msg := req.(type) {
			case *pendingChansReq:
				f.handlePendingChannels(msg)
			}
		case <-f.quit:
			return
		}
	}
}

// handlePendingChannels responds to a request for details concerning all
// currently pending channels waiting for the final phase of the funding
// workflow (funding txn confirmation).
func (f *fundingManager) handlePendingChannels(msg *pendingChansReq) {
	var pendingChannels []*pendingChannel

	dbPendingChannels, err := f.cfg.Wallet.Cfg.Database.FetchPendingChannels()
	if err != nil {
		msg.resp <- nil
		msg.err <- err
		return
	}

	for _, dbPendingChan := range dbPendingChannels {
		pendingChan := &pendingChannel{
			identityPub:   dbPendingChan.IdentityPub,
			channelPoint:  &dbPendingChan.FundingOutpoint,
			capacity:      dbPendingChan.Capacity,
			localBalance:  dbPendingChan.LocalBalance.ToSatoshis(),
			remoteBalance: dbPendingChan.RemoteBalance.ToSatoshis(),
		}

		pendingChannels = append(pendingChannels, pendingChan)
	}

	msg.resp <- pendingChannels
	msg.err <- nil
}

// processFundingOpen sends a message to the fundingManager allowing it to
// initiate the new funding workflow with the source peer.
func (f *fundingManager) processFundingOpen(msg *lnwire.OpenChannel,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingOpenMsg{msg, peerAddress}
}

// handleFundingOpen creates an initial 'ChannelReservation' within the wallet,
// then responds to the source peer with an accept channel message progressing
// the funding workflow.
//
// TODO(roasbeef): add error chan to all, let channelManager handle
// error+propagate
func (f *fundingManager) handleFundingOpen(fmsg *fundingOpenMsg) {
	// Check number of pending channels to be smaller than maximum allowed
	// number and send ErrorGeneric to remote peer if condition is
	// violated.
	peerIDKey := newSerializedKey(fmsg.peerAddress.IdentityKey)

	msg := fmsg.msg
	amt := msg.FundingAmount

	// TODO(roasbeef): modify to only accept a _single_ pending channel per
	// block unless white listed
	if len(f.activeReservations[peerIDKey]) >= cfg.MaxPendingChannels {
		f.failFundingFlow(
			fmsg.peerAddress.IdentityKey, fmsg.msg.PendingChannelID,
			lnwire.ErrorData{byte(lnwire.ErrMaxPendingChannels)},
		)
		return
	}

	// We'll also reject any requests to create channels until we're fully
	// synced to the network as we won't be able to properly validate the
	// confirmation of the funding transaction.
	isSynced, err := f.cfg.Wallet.IsSynced()
	if err != nil {
		fndgLog.Errorf("unable to query wallet: %v", err)
		return
	}
	if !isSynced {
		f.failFundingFlow(
			fmsg.peerAddress.IdentityKey, fmsg.msg.PendingChannelID,
			lnwire.ErrorData{byte(lnwire.ErrSynchronizingChain)},
		)
		return
	}

	// We'll reject any request to create a channel that's above the
	// current soft-limit for channel size.
	if msg.FundingAmount > maxFundingAmount {
		f.failFundingFlow(
			fmsg.peerAddress.IdentityKey, fmsg.msg.PendingChannelID,
			lnwire.ErrorData{byte(lnwire.ErrChanTooLarge)},
		)
		return
	}

	// TODO(roasbeef): error if funding flow already ongoing
	fndgLog.Infof("Recv'd fundingRequest(amt=%v, push=%v, delay=%v, "+
		"pendingId=%x) from peer(%x)", amt, msg.PushAmount,
		msg.CsvDelay, msg.PendingChannelID,
		fmsg.peerAddress.IdentityKey.SerializeCompressed())

	// Attempt to initialize a reservation within the wallet. If the wallet
	// has insufficient resources to create the channel, then the
	// reservation attempt may be rejected. Note that since we're on the
	// responding side of a single funder workflow, we don't commit any
	// funds to the channel ourselves.
	//
	// TODO(roasbeef): assuming this was an inbound connection, replace
	// port with default advertised port
	chainHash := chainhash.Hash(msg.ChainHash)
	reservation, err := f.cfg.Wallet.InitChannelReservation(amt, 0,
		msg.PushAmount, btcutil.Amount(msg.FeePerKiloWeight),
		fmsg.peerAddress.IdentityKey, fmsg.peerAddress.Address,
		&chainHash)
	if err != nil {
		fndgLog.Errorf("Unable to initialize reservation: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		return
	}

	// As we're the responder, we get to specify the number of
	// confirmations that we require before both of us consider the channel
	// open. We'll use out mapping to derive the proper number of
	// confirmations based on the amount of the channel, and also if any
	// funds are being pushed to us.
	numConfsReq := f.cfg.NumRequiredConfs(msg.FundingAmount, msg.PushAmount)
	reservation.SetNumConfsRequired(numConfsReq)

	// We'll also validate and apply all the constraints the initiating
	// party is attempting to dictate for our commitment transaction.
	err = reservation.CommitConstraints(
		uint16(msg.CsvDelay), msg.MaxAcceptedHTLCs,
		msg.MaxValueInFlight, msg.ChannelReserve,
	)
	if err != nil {
		f.failFundingFlow(
			fmsg.peerAddress.IdentityKey, fmsg.msg.PendingChannelID,
			[]byte(fmt.Sprintf("Unacceptable channel "+
				"constraints: %v", err)),
		)
		return
	}

	fndgLog.Infof("Requiring %v confirmations for pendingChan(%x): "+
		"amt=%v, push_amt=%v", numConfsReq, fmsg.msg.PendingChannelID,
		amt, msg.PushAmount)

	// Once the reservation has been created successfully, we add it to
	// this peers map of pending reservations to track this particular
	// reservation until either abort or completion.
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}
	f.activeReservations[peerIDKey][msg.PendingChannelID] = &reservationWithCtx{
		reservation: reservation,
		chanAmt:     amt,
		err:         make(chan error, 1),
		peerAddress: fmsg.peerAddress,
	}
	f.resMtx.Unlock()

	// Using the RequiredRemoteDelay closure, we'll compute the remote CSV
	// delay we require given the total amount of funds within the channel.
	remoteCsvDelay := f.cfg.RequiredRemoteDelay(amt)

	// We'll also generate our required constraints for the remote party,
	chanReserve, maxValue, maxHtlcs := reservation.RemoteChanConstraints()

	// With our parameters set, we'll now process their contribution so we
	// can move the funding workflow ahead.
	remoteContribution := &lnwallet.ChannelContribution{
		FundingAmount:        amt,
		FirstCommitmentPoint: msg.FirstCommitmentPoint,
		ChannelConfig: &channeldb.ChannelConfig{
			ChannelConstraints: channeldb.ChannelConstraints{
				DustLimit:        msg.DustLimit,
				MaxPendingAmount: maxValue,
				ChanReserve:      chanReserve,
				MinHTLC:          msg.HtlcMinimum,
				MaxAcceptedHtlcs: maxHtlcs,
			},
			CsvDelay:            remoteCsvDelay,
			MultiSigKey:         copyPubKey(msg.FundingKey),
			RevocationBasePoint: copyPubKey(msg.RevocationPoint),
			PaymentBasePoint:    copyPubKey(msg.PaymentPoint),
			DelayBasePoint:      copyPubKey(msg.DelayedPaymentPoint),
		},
	}
	err = reservation.ProcessSingleContribution(remoteContribution)
	if err != nil {
		fndgLog.Errorf("unable to add contribution reservation: %v", err)
		// TODO(roasbeef): verify only sending sane info over
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		return
	}

	fndgLog.Infof("Sending fundingResp for pendingID(%x)",
		msg.PendingChannelID)
	fndgLog.Debugf("Remote party accepted commitment constraints: %v",
		spew.Sdump(remoteContribution.ChannelConfig.ChannelConstraints))

	// With the initiator's contribution recorded, respond with our
	// contribution in the next message of the workflow.
	ourContribution := reservation.OurContribution()
	fundingAccept := lnwire.AcceptChannel{
		PendingChannelID:     msg.PendingChannelID,
		DustLimit:            ourContribution.DustLimit,
		MaxValueInFlight:     maxValue,
		ChannelReserve:       chanReserve,
		MinAcceptDepth:       uint32(numConfsReq),
		HtlcMinimum:          ourContribution.MinHTLC,
		CsvDelay:             uint16(remoteCsvDelay),
		MaxAcceptedHTLCs:     maxHtlcs,
		FundingKey:           ourContribution.MultiSigKey,
		RevocationPoint:      ourContribution.RevocationBasePoint,
		PaymentPoint:         ourContribution.PaymentBasePoint,
		DelayedPaymentPoint:  ourContribution.DelayBasePoint,
		FirstCommitmentPoint: ourContribution.FirstCommitmentPoint,
	}
	err = f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, &fundingAccept)
	if err != nil {
		fndgLog.Errorf("unable to send funding response to peer: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		return
	}
}

// processFundingAccept sends a message to the fundingManager allowing it to
// continue the second phase of a funding workflow with the target peer.
func (f *fundingManager) processFundingAccept(msg *lnwire.AcceptChannel,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingAcceptMsg{msg, peerAddress}
}

// handleFundingAceept processes a response to the workflow initiation sent by
// the remote peer. This message then queues a message with the funding
// outpoint, and a commitment signature to the remote peer.
func (f *fundingManager) handleFundingAccept(fmsg *fundingAcceptMsg) {
	msg := fmsg.msg
	pendingChanID := fmsg.msg.PendingChannelID
	peerKey := fmsg.peerAddress.IdentityKey

	resCtx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		fndgLog.Warnf("Can't find reservation (peerKey:%v, chanID:%v)",
			peerKey, pendingChanID)
		return
	}

	fndgLog.Infof("Recv'd fundingResponse for pendingID(%x)", pendingChanID)

	// We'll also specify the responder's preference for the number of
	// required confirmations, and also the set of channel constraints
	// they've specified for commitment states we can create.
	resCtx.reservation.SetNumConfsRequired(uint16(msg.MinAcceptDepth))
	err = resCtx.reservation.CommitConstraints(
		uint16(msg.CsvDelay), msg.MaxAcceptedHTLCs,
		msg.MaxValueInFlight, msg.ChannelReserve,
	)
	if err != nil {
		f.failFundingFlow(
			fmsg.peerAddress.IdentityKey, fmsg.msg.PendingChannelID,
			[]byte(fmt.Sprintf("Unacceptable channel "+
				"constraints: %v", err)),
		)
		return
	}

	// As they've accepted our channel constraints, we'll regenerate them
	// here so we can properly commit their accepted constraints to the
	// reservation.
	chanReserve, maxValue, maxHtlcs := resCtx.reservation.RemoteChanConstraints()

	// The remote node has responded with their portion of the channel
	// contribution. At this point, we can process their contribution which
	// allows us to construct and sign both the commitment transaction, and
	// the funding transaction.
	remoteContribution := &lnwallet.ChannelContribution{
		FirstCommitmentPoint: msg.FirstCommitmentPoint,
		ChannelConfig: &channeldb.ChannelConfig{
			ChannelConstraints: channeldb.ChannelConstraints{
				DustLimit:        msg.DustLimit,
				MaxPendingAmount: maxValue,
				ChanReserve:      chanReserve,
				MinHTLC:          msg.HtlcMinimum,
				MaxAcceptedHtlcs: maxHtlcs,
			},
			MultiSigKey:         copyPubKey(msg.FundingKey),
			RevocationBasePoint: copyPubKey(msg.RevocationPoint),
			PaymentBasePoint:    copyPubKey(msg.PaymentPoint),
			DelayBasePoint:      copyPubKey(msg.DelayedPaymentPoint),
		},
	}
	remoteContribution.CsvDelay = f.cfg.RequiredRemoteDelay(resCtx.chanAmt)
	err = resCtx.reservation.ProcessContribution(remoteContribution)
	if err != nil {
		fndgLog.Errorf("Unable to process contribution from %v: %v",
			fmsg.peerAddress.IdentityKey, err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		resCtx.err <- err
		return
	}

	fndgLog.Infof("pendingChan(%x): remote party proposes num_confs=%v, "+
		"csv_delay=%v", pendingChanID, msg.MinAcceptDepth, msg.CsvDelay)
	fndgLog.Debugf("Remote party accepted commitment constraints: %v",
		spew.Sdump(remoteContribution.ChannelConfig.ChannelConstraints))

	// Now that we have their contribution, we can extract, then send over
	// both the funding out point and our signature for their version of
	// the commitment transaction to the remote peer.
	outPoint := resCtx.reservation.FundingOutpoint()
	_, sig := resCtx.reservation.OurSignatures()
	commitSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		fndgLog.Errorf("Unable to parse signature: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		resCtx.err <- err
		return
	}

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	channelID := lnwire.NewChanIDFromOutPoint(outPoint)
	fndgLog.Debugf("Creating chan barrier for ChanID(%v)", channelID)
	f.newChanBarriers[channelID] = make(chan struct{})
	f.barrierMtx.Unlock()

	// The next message that advances the funding flow will reference the
	// channel via its permanent channel ID, so we'll set up this mapping
	// so we can retrieve the reservation context once we get the
	// FundingSigned message.
	f.resMtx.Lock()
	f.signedReservations[channelID] = pendingChanID
	f.resMtx.Unlock()

	fndgLog.Infof("Generated ChannelPoint(%v) for pendingID(%x)", outPoint,
		pendingChanID)

	fundingCreated := &lnwire.FundingCreated{
		PendingChannelID: pendingChanID,
		FundingPoint:     *outPoint,
		CommitSig:        commitSig,
	}
	err = f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, fundingCreated)
	if err != nil {
		fndgLog.Errorf("Unable to send funding complete message: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			msg.PendingChannelID, []byte(err.Error()))
		resCtx.err <- err
		return
	}
}

// processFundingCreated queues a funding complete message coupled with the
// source peer to the fundingManager.
func (f *fundingManager) processFundingCreated(msg *lnwire.FundingCreated,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingCreatedMsg{msg, peerAddress}
}

// handleFundingCreated progresses the funding workflow when the daemon is on
// the responding side of a single funder workflow. Once this message has been
// processed, a signature is sent to the remote peer allowing it to broadcast
// the funding transaction, progressing the workflow into the final stage.
func (f *fundingManager) handleFundingCreated(fmsg *fundingCreatedMsg) {
	peerKey := fmsg.peerAddress.IdentityKey
	pendingChanID := fmsg.msg.PendingChannelID

	resCtx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			peerKey, pendingChanID)
		return
	}

	// The channel initiator has responded with the funding outpoint of the
	// final funding transaction, as well as a signature for our version of
	// the commitment transaction. So at this point, we can validate the
	// initiator's commitment transaction, then send our own if it's valid.
	// TODO(roasbeef): make case (p vs P) consistent throughout
	fundingOut := fmsg.msg.FundingPoint
	fndgLog.Infof("completing pendingID(%x) with ChannelPoint(%v)",
		pendingChanID, fundingOut)

	// With all the necessary data available, attempt to advance the
	// funding workflow to the next stage. If this succeeds then the
	// funding transaction will broadcast after our next message.
	// CompleteReservationSingle will also mark the channel as 'IsPending'
	// in the database.
	commitSig := fmsg.msg.CommitSig.Serialize()
	completeChan, err := resCtx.reservation.CompleteReservationSingle(
		&fundingOut, commitSig)
	if err != nil {
		// TODO(roasbeef): better error logging: peerID, channelID, etc.
		fndgLog.Errorf("unable to complete single reservation: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err.Error()))
		return
	}

	// If something goes wrong before the funding transaction is confirmed,
	// we use this convenience method to delete the pending OpenChannel
	// from the database.
	deleteFromDatabase := func() {
		closeInfo := &channeldb.ChannelCloseSummary{
			ChanPoint: completeChan.FundingOutpoint,
			RemotePub: completeChan.IdentityPub,
			CloseType: channeldb.FundingCanceled,
		}

		if err := completeChan.CloseChannel(closeInfo); err != nil {
			fndgLog.Errorf("Failed closing channel %v: %v",
				completeChan.FundingOutpoint, err)
		}
	}

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	channelID := lnwire.NewChanIDFromOutPoint(&fundingOut)
	fndgLog.Debugf("Creating chan barrier for ChanID(%v)", channelID)
	f.newChanBarriers[channelID] = make(chan struct{})
	f.barrierMtx.Unlock()

	fndgLog.Infof("sending signComplete for pendingID(%x) over ChannelPoint(%v)",
		pendingChanID, fundingOut)

	// With their signature for our version of the commitment transaction
	// verified, we can now send over our signature to the remote peer.
	//
	// TODO(roasbeef): just have raw bytes in wire msg? avoids decoding
	// then decoding shortly afterwards.
	_, sig := resCtx.reservation.OurSignatures()
	ourCommitSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		fndgLog.Errorf("unable to parse signature: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err.Error()))
		deleteFromDatabase()
		return
	}

	fundingSigned := &lnwire.FundingSigned{
		ChanID:    channelID,
		CommitSig: ourCommitSig,
	}
	if err := f.cfg.SendToPeer(peerKey, fundingSigned); err != nil {
		fndgLog.Errorf("unable to send FundingSigned message: %v", err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err.Error()))
		deleteFromDatabase()
		return
	}

	// Create an entry in the local discovery map so we can ensure that we
	// process the channel confirmation fully before we receive a funding
	// locked message.
	f.localDiscoveryMtx.Lock()
	f.localDiscoverySignals[channelID] = make(chan struct{})
	f.localDiscoveryMtx.Unlock()

	// With this last message, our job as the responder is now complete.
	// We'll wait for the funding transaction to reach the specified number
	// of confirmations, then start normal operations.
	//
	// When we get to this point we have sent the signComplete message to
	// the channel funder, and BOLT#2 specifies that we MUST remember the
	// channel for reconnection. The channel is already marked
	// as pending in the database, so in case of a disconnect or restart,
	// we will continue waiting for the confirmation the next time we start
	// the funding manager. In case the funding transaction never appears
	// on the blockchain, we must forget this channel. We therefore
	// completely forget about this channel if we haven't seen the funding
	// transaction in 288 blocks (~ 48 hrs), by canceling the reservation
	// and canceling the wait for the funding confirmation.
	go func() {
		doneChan := make(chan struct{})
		timeoutChan := make(chan struct{})
		go f.waitForFundingWithTimeout(completeChan, doneChan,
			timeoutChan)

		select {
		case <-timeoutChan:
			// We did not see the funding confirmation before
			// timeout, so we forget the channel.
			deleteFromDatabase()
		case <-f.quit:
			// The fundingManager is shutting down, will resume
			// wait for funding transaction on startup.
		case <-doneChan:
			// Success, funding transaction was confirmed.
			f.deleteReservationCtx(peerKey,
				fmsg.msg.PendingChannelID)
		}
	}()
}

// processFundingSigned sends a single funding sign complete message along with
// the source peer to the funding manager.
func (f *fundingManager) processFundingSigned(msg *lnwire.FundingSigned,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingSignedMsg{msg, peerAddress}
}

// handleFundingSigned processes the final message received in a single funder
// workflow. Once this message is processed, the funding transaction is
// broadcast. Once the funding transaction reaches a sufficient number of
// confirmations, a message is sent to the responding peer along with a compact
// encoding of the location of the channel within the blockchain.
func (f *fundingManager) handleFundingSigned(fmsg *fundingSignedMsg) {
	// As the funding signed message will reference the reservation by it's
	// permanent channel ID, we'll need to perform an intermediate look up
	// before we can obtain the reservation.
	f.resMtx.Lock()
	pendingChanID, ok := f.signedReservations[fmsg.msg.ChanID]
	delete(f.signedReservations, fmsg.msg.ChanID)
	f.resMtx.Unlock()
	if !ok {
		err := fmt.Sprintf("Unable to find signed reservation for "+
			"chan_id=%x", fmsg.msg.ChanID)
		fndgLog.Warnf(err)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err))
		return
	}

	peerKey := fmsg.peerAddress.IdentityKey
	resCtx, err := f.getReservationCtx(fmsg.peerAddress.IdentityKey,
		pendingChanID)
	if err != nil {
		fndgLog.Warnf("Unable to find reservation (peerID:%v, chanID:%v)",
			peerKey, pendingChanID)
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err.Error()))
		return
	}

	// Create an entry in the local discovery map so we can ensure that we
	// process the channel confirmation fully before we receive a funding
	// locked message.
	fundingPoint := resCtx.reservation.FundingOutpoint()
	permChanID := lnwire.NewChanIDFromOutPoint(fundingPoint)
	f.localDiscoveryMtx.Lock()
	f.localDiscoverySignals[permChanID] = make(chan struct{})
	f.localDiscoveryMtx.Unlock()

	// The remote peer has responded with a signature for our commitment
	// transaction. We'll verify the signature for validity, then commit
	// the state to disk as we can now open the channel.
	commitSig := fmsg.msg.CommitSig.Serialize()
	completeChan, err := resCtx.reservation.CompleteReservation(nil, commitSig)
	if err != nil {
		fndgLog.Errorf("Unable to complete reservation sign complete: %v", err)
		resCtx.err <- err
		f.failFundingFlow(fmsg.peerAddress.IdentityKey,
			pendingChanID, []byte(err.Error()))
		return
	}

	fndgLog.Infof("Finalizing pendingID(%x) over ChannelPoint(%v), "+
		"waiting for channel open on-chain", pendingChanID, fundingPoint)

	// Send an update to the upstream client that the negotiation process
	// is over.
	// TODO(roasbeef): add abstraction over updates to accommodate
	// long-polling, or SSE, etc.
	resCtx.updates <- &lnrpc.OpenStatusUpdate{
		Update: &lnrpc.OpenStatusUpdate_ChanPending{
			ChanPending: &lnrpc.PendingUpdate{
				Txid:        fundingPoint.Hash[:],
				OutputIndex: fundingPoint.Index,
			},
		},
	}

	go func() {
		doneChan := make(chan struct{})
		cancelChan := make(chan struct{})

		// In case the fundingManager is stopped at some point during
		// the remaining part of the opening process, we must wait for
		// this process to finish (either successully or with some
		// error), before the fundingManager can be shut down.
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.waitForFundingConfirmation(completeChan, cancelChan,
				doneChan)
		}()

		select {
		case <-f.quit:
			return
		case <-doneChan:
		}

		// Finally give the caller a final update notifying them that
		// the channel is now open.
		// TODO(roasbeef): only notify after recv of funding locked?
		resCtx.updates <- &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanOpen{
				ChanOpen: &lnrpc.ChannelOpenUpdate{
					ChannelPoint: &lnrpc.ChannelPoint{
						FundingTxid: fundingPoint.Hash[:],
						OutputIndex: fundingPoint.Index,
					},
				},
			},
		}

		f.deleteReservationCtx(peerKey, pendingChanID)
	}()
}

// waitForFundingWithTimeout is a wrapper around waitForFundingConfirmation that
// will cancel the wait for confirmation if maxWaitNumBlocksFundingConf has
// passed from bestHeight. In the case of timeout, the timeoutChan will be
// closed. In case of confirmation or error, doneChan will be closed.
func (f *fundingManager) waitForFundingWithTimeout(completeChan *channeldb.OpenChannel,
	doneChan chan<- struct{}, timeoutChan chan<- struct{}) {

	epochClient, err := f.cfg.Notifier.RegisterBlockEpochNtfn()
	if err != nil {
		fndgLog.Errorf("unable to register for epoch notification: %v",
			err)
		close(doneChan)
		return
	}

	defer epochClient.Cancel()

	waitingDoneChan := make(chan struct{})
	cancelChan := make(chan struct{})

	// Add this goroutine to wait group so we can be sure that it is
	// properly stopped before the funding manager can be shut down.
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.waitForFundingConfirmation(completeChan, cancelChan,
			waitingDoneChan)
	}()

	// On block maxHeight we will cancel the funding confirmation wait.
	maxHeight := completeChan.FundingBroadcastHeight + maxWaitNumBlocksFundingConf
	for {
		select {
		case epoch, ok := <-epochClient.Epochs:
			if !ok {
				fndgLog.Warnf("Epoch client shutting down")
				return
			}

			if uint32(epoch.Height) >= maxHeight {
				fndgLog.Warnf("waited for %v blocks without "+
					"seeing funding transaction confirmed,"+
					" cancelling.", maxWaitNumBlocksFundingConf)

				// Cancel the waitForFundingConfirmation
				// goroutine.
				close(cancelChan)

				// Notify the caller of the timeout.
				close(timeoutChan)
				return
			}
		case <-f.quit:
			// The fundingManager is shutting down, will resume
			// waiting for the funding transaction on startup.
			return
		case <-waitingDoneChan:
			close(doneChan)
			return
		}
	}
}

// waitForFundingConfirmation handles the final stages of the channel funding
// process once the funding transaction has been broadcast. The primary
// function of waitForFundingConfirmation is to wait for blockchain
// confirmation, and then to notify the other systems that must be notified
// when a channel has become active for lightning transactions.
// The wait can be canceled by closing the cancelChan.
func (f *fundingManager) waitForFundingConfirmation(completeChan *channeldb.OpenChannel,
	cancelChan <-chan struct{}, doneChan chan<- struct{}) {

	defer close(doneChan)

	// Register with the ChainNotifier for a notification once the funding
	// transaction reaches `numConfs` confirmations.
	txid := completeChan.FundingOutpoint.Hash
	numConfs := uint32(completeChan.NumConfsRequired)
	confNtfn, err := f.cfg.Notifier.RegisterConfirmationsNtfn(&txid,
		numConfs, completeChan.FundingBroadcastHeight)
	if err != nil {
		fndgLog.Errorf("Unable to register for confirmation of "+
			"ChannelPoint(%v)", completeChan.FundingOutpoint)
		return
	}

	fndgLog.Infof("Waiting for funding tx (%v) to reach %v confirmations",
		txid, numConfs)

	var confDetails *chainntnfs.TxConfirmation
	var ok bool

	// Wait until the specified number of confirmations has been reached,
	// we get a cancel signal, or the wallet signals a shutdown.
	select {
	case confDetails, ok = <-confNtfn.Confirmed:
		// fallthrough
	case <-cancelChan:
		fndgLog.Warnf("canceled waiting for funding confirmation, "+
			"stopping funding flow for ChannelPoint(%v)",
			completeChan.FundingOutpoint)
		return
	case <-f.quit:
		fndgLog.Warnf("fundingManager shutting down, stopping funding "+
			"flow for ChannelPoint(%v)", completeChan.FundingOutpoint)
		return
	}

	if !ok {
		fndgLog.Warnf("ChainNotifier shutting down, cannot complete "+
			"funding flow for ChannelPoint(%v)",
			completeChan.FundingOutpoint)
		return
	}

	fundingPoint := completeChan.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(&fundingPoint)

	fndgLog.Infof("ChannelPoint(%v) is now active: ChannelID(%x)",
		fundingPoint, chanID[:])

	// With the block height and the transaction index known, we can
	// construct the compact chanID which is used on the network to unique
	// identify channels.
	shortChanID := lnwire.ShortChannelID{
		BlockHeight: confDetails.BlockHeight,
		TxIndex:     confDetails.TxIndex,
		TxPosition:  uint16(fundingPoint.Index),
	}

	// Now that the channel has been fully confirmed, we'll mark it as open
	// within the database.
	completeChan.IsPending = false
	err = f.cfg.Wallet.Cfg.Database.MarkChannelAsOpen(&fundingPoint, shortChanID)
	if err != nil {
		fndgLog.Errorf("error setting channel pending flag to false: "+
			"%v", err)
		return
	}

	// TODO(roasbeef): ideally persistent state update for chan above
	// should be abstracted

	// The funding transaction now being confirmed, we add this channel to
	// the fundingManager's internal persistant state machine that we use
	// to track the remaining process of the channel opening. This is useful
	// to resume the opening process in case of restarts.
	//
	// TODO(halseth): make the two db transactions (MarkChannelAsOpen and
	// saveChannelOpeningState) atomic by doing them in the same transaction.
	// Needed to be properly fault-tolerant.
	err = f.saveChannelOpeningState(&completeChan.FundingOutpoint, markedOpen,
		&shortChanID)
	if err != nil {
		fndgLog.Errorf("error setting channel state to markedOpen: %v",
			err)
		return
	}

	// Now that the funding transaction has the required number of
	// confirmations, we send the fundingLocked message to the peer.
	f.sendFundingLockedAndAnnounceChannel(completeChan, &shortChanID)
}

// sendFundingLockedAndAnnounceChannel creates and sends the fundingLocked
// message, and then the channel announcement. This should be called after the
// funding transaction has been confirmed, and the channelState is 'markedOpen'.
func (f *fundingManager) sendFundingLockedAndAnnounceChannel(
	completeChan *channeldb.OpenChannel, shortChanID *lnwire.ShortChannelID) {

	chanID := lnwire.NewChanIDFromOutPoint(&completeChan.FundingOutpoint)

	// With the channel marked open, we'll create the state-machine object
	// which wraps the database state.
	channel, err := lnwallet.NewLightningChannel(nil, nil,
		f.cfg.FeeEstimator, completeChan)
	if err != nil {
		fndgLog.Errorf("error creating new lightning channel: %v", err)
		return
	}
	defer channel.Stop()

	// Next, we'll send over the funding locked message which marks that we
	// consider the channel open by presenting the remote party with our
	// next revocation key. Without the revocation key, the remote party
	// will be unable to propose state transitions.
	nextRevocation, err := channel.NextRevocationKey()
	if err != nil {
		fndgLog.Errorf("unable to create next revocation: %v", err)
		return
	}
	fundingLockedMsg := lnwire.NewFundingLocked(chanID, nextRevocation)

	err = f.cfg.SendToPeer(completeChan.IdentityPub, fundingLockedMsg)
	if err != nil {
		fndgLog.Errorf("unable to send fundingLocked to peer: %v", err)
		return
	}

	// As the fundingLocked message is now sent to the peer, the channel is
	// moved to the next state of the state machine. It will be moved to the
	// last state (actually deleted from the database) after the channel is
	// finally announced.
	err = f.saveChannelOpeningState(&completeChan.FundingOutpoint, fundingLockedSent,
		shortChanID)
	if err != nil {
		fndgLog.Errorf("error setting channel state to "+
			"fundingLockedSent: %v", err)
		return
	}

	// TODO(roasbeef): wait 6 blocks before announcing

	f.sendChannelAnnouncement(completeChan, channel, shortChanID)
}

// sendChannelAnnouncement broadcast the necessary channel announcement
// messages to the network. Should be called after the fundingLocked message
// is sent (channelState is 'fundingLockedSent') and the channel is ready to
// be used.
func (f *fundingManager) sendChannelAnnouncement(completeChan *channeldb.OpenChannel,
	channel *lnwallet.LightningChannel, shortChanID *lnwire.ShortChannelID) {

	chanID := lnwire.NewChanIDFromOutPoint(&completeChan.FundingOutpoint)
	fundingPoint := completeChan.FundingOutpoint

	fndgLog.Infof("Announcing ChannelPoint(%v), short_chan_id=%v",
		&fundingPoint, spew.Sdump(shortChanID))

	// Register the new link with the L3 routing manager so this new
	// channel can be utilized during path finding.
	err := f.announceChannel(f.cfg.IDKey, completeChan.IdentityPub,
		channel.LocalFundingKey, channel.RemoteFundingKey,
		*shortChanID, chanID)
	if err != nil {
		fndgLog.Errorf("channel announcement failed: %v", err)
		return
	}

	// After the channel is successfully announced from the
	// fundingManager, we delete the channel from our internal database.
	// We can do this
	// because we assume the AuthenticatedGossiper queues the announcement
	// messages, and persists them in case of a daemon shutdown.
	err = f.deleteChannelOpeningState(&completeChan.FundingOutpoint)
	if err != nil {
		fndgLog.Errorf("error deleting channel state: %v", err)
		return
	}

	// Finally, as the local channel discovery has been fully processed,
	// we'll trigger the signal indicating that it's safe for any funding
	// locked messages related to this channel to be processed.
	f.localDiscoveryMtx.Lock()
	if discoverySignal, ok := f.localDiscoverySignals[chanID]; ok {
		close(discoverySignal)
	}
	f.localDiscoveryMtx.Unlock()

	return
}

// processFundingLocked sends a message to the fundingManager allowing it to
// finish the funding workflow.
func (f *fundingManager) processFundingLocked(msg *lnwire.FundingLocked,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingLockedMsg{msg, peerAddress}
}

// handleFundingLocked finalizes the channel funding process and enables the
// channel to enter normal operating mode.
func (f *fundingManager) handleFundingLocked(fmsg *fundingLockedMsg) {
	f.localDiscoveryMtx.Lock()
	localDiscoverySignal, ok := f.localDiscoverySignals[fmsg.msg.ChanID]
	f.localDiscoveryMtx.Unlock()

	if ok {
		// Before we proceed with processing the funding locked
		// message, we'll wait for the lcoal waitForFundingConfirmation
		// goroutine to signal that it has the necessary state in
		// place. Otherwise, we may be missing critical information
		// required to handle forwarded HTLC's.
		<-localDiscoverySignal

		// With the signal received, we can now safely delete the entry
		// from the map.
		f.localDiscoveryMtx.Lock()
		delete(f.localDiscoverySignals, fmsg.msg.ChanID)
		f.localDiscoveryMtx.Unlock()
	}

	// First, we'll attempt to locate the channel who's funding workflow is
	// being finalized by this message. We got to the database rather than
	// our reservation map as we may have restarted, mid funding flow.
	chanID := fmsg.msg.ChanID
	channel, err := f.cfg.FindChannel(chanID)
	if err != nil {
		fndgLog.Errorf("Unable to locate ChannelID(%v), cannot complete "+
			"funding", chanID)
		return
	}

	// TODO(roasbeef): done nothing if repeat message sent

	// The funding locked message contains the next commitment point we'll
	// need to create the next commitment state for the remote party. So
	// we'll insert that into the channel now before passing it along to
	// other sub-systems.
	err = channel.InitNextRevocation(fmsg.msg.NextPerCommitmentPoint)
	if err != nil {
		fndgLog.Errorf("unable to insert next commitment point: %v", err)
		return
	}

	// With the channel retrieved, we'll send the breach arbiter the new
	// channel so it can watch for attempts to breach the channel's
	// contract by the remote party.
	f.cfg.ArbiterChan <- channel

	// Launch a defer so we _ensure_ that the channel barrier is properly
	// closed even if the target peer is not longer online at this point.
	defer func() {
		// Close the active channel barrier signalling the readHandler
		// that commitment related modifications to this channel can
		// now proceed.
		f.barrierMtx.Lock()
		fndgLog.Tracef("Closing chan barrier for ChanID(%v)", chanID)
		close(f.newChanBarriers[chanID])
		delete(f.newChanBarriers, chanID)
		f.barrierMtx.Unlock()
	}()

	// Finally, we'll find the peer that sent us this message so we can
	// provide it with the fully initialized channel state.
	peer, err := f.cfg.FindPeer(fmsg.peerAddress.IdentityKey)
	if err != nil {
		fndgLog.Errorf("Unable to find peer: %v", err)
		return
	}
	newChanDone := make(chan struct{})
	newChanMsg := &newChannelMsg{
		channel: channel,
		done:    newChanDone,
	}
	peer.newChannels <- newChanMsg

	// We pause here to wait for the peer to recognize the new channel
	// before we close the channel barrier corresponding to the channel.
	select {
	case <-f.quit:
		return
	case <-newChanDone: // Fallthrough if we're not quitting.
	}
}

// channelProof is one half of the proof necessary to create an authenticated
// announcement on the network. The two signatures individually sign a
// statement of the existence of a channel.
type channelProof struct {
	nodeSig    *btcec.Signature
	bitcoinSig *btcec.Signature
}

// chanAnnouncement encapsulates the two authenticated announcements that we
// send out to the network after a new channel has been created locally.
type chanAnnouncement struct {
	chanAnn       *lnwire.ChannelAnnouncement
	chanUpdateAnn *lnwire.ChannelUpdate
	chanProof     *lnwire.AnnounceSignatures
}

// newChanAnnouncement creates the authenticated channel announcement messages
// required to broadcast a newly created channel to the network. The
// announcement is two part: the first part authenticates the existence of the
// channel and contains four signatures binding the funding pub keys and
// identity pub keys of both parties to the channel, and the second segment is
// authenticated only by us and contains our directional routing policy for the
// channel.
func (f *fundingManager) newChanAnnouncement(localPubKey, remotePubKey *btcec.PublicKey,
	localFundingKey, remoteFundingKey *btcec.PublicKey,
	shortChanID lnwire.ShortChannelID,
	chanID lnwire.ChannelID) (*chanAnnouncement, error) {

	chainHash := *f.cfg.Wallet.Cfg.NetParams.GenesisHash

	// The unconditional section of the announcement is the ShortChannelID
	// itself which compactly encodes the location of the funding output
	// within the blockchain.
	chanAnn := &lnwire.ChannelAnnouncement{
		ShortChannelID: shortChanID,
		Features:       lnwire.NewFeatureVector([]lnwire.Feature{}),
		ChainHash:      chainHash,
	}

	// The chanFlags field indicates which directed edge of the channel is
	// being updated within the ChannelUpdateAnnouncement announcement
	// below. A value of zero means it's the edge of the "first" node and 1
	// being the other node.
	var chanFlags uint16

	// The lexicographical ordering of the two identity public keys of the
	// nodes indicates which of the nodes is "first". If our serialized
	// identity key is lower than theirs then we're the "first" node and
	// second otherwise.
	selfBytes := localPubKey.SerializeCompressed()
	remoteBytes := remotePubKey.SerializeCompressed()
	if bytes.Compare(selfBytes, remoteBytes) == -1 {
		chanAnn.NodeID1 = localPubKey
		chanAnn.NodeID2 = remotePubKey
		chanAnn.BitcoinKey1 = localFundingKey
		chanAnn.BitcoinKey2 = remoteFundingKey

		// If we're the first node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 0
	} else {
		chanAnn.NodeID1 = remotePubKey
		chanAnn.NodeID2 = localPubKey
		chanAnn.BitcoinKey1 = remoteFundingKey
		chanAnn.BitcoinKey2 = localFundingKey

		// If we're the second node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 1
	}

	chanUpdateAnn := &lnwire.ChannelUpdate{
		ShortChannelID:  shortChanID,
		ChainHash:       chainHash,
		Timestamp:       uint32(time.Now().Unix()),
		Flags:           chanFlags,
		TimeLockDelta:   uint16(f.cfg.DefaultRoutingPolicy.TimeLockDelta),
		HtlcMinimumMsat: f.cfg.DefaultRoutingPolicy.MinHTLC,
		BaseFee:         uint32(f.cfg.DefaultRoutingPolicy.BaseFee),
		FeeRate:         uint32(f.cfg.DefaultRoutingPolicy.FeeRate),
	}

	// With the channel update announcement constructed, we'll generate a
	// signature that signs a double-sha digest of the announcement.
	// This'll serve to authenticate this announcement and any other future
	// updates we may send.
	chanUpdateMsg, err := chanUpdateAnn.DataToSign()
	if err != nil {
		return nil, err
	}
	chanUpdateAnn.Signature, err = f.cfg.SignMessage(f.cfg.IDKey, chanUpdateMsg)
	if err != nil {
		return nil, errors.Errorf("unable to generate channel "+
			"update announcement signature: %v", err)
	}

	// The channel existence proofs itself is currently announced in
	// distinct message. In order to properly authenticate this message, we
	// need two signatures: one under the identity public key used which
	// signs the message itself and another signature of the identity
	// public key under the funding key itself.
	//
	// TODO(roasbeef): use SignAnnouncement here instead?
	chanAnnMsg, err := chanAnn.DataToSign()
	if err != nil {
		return nil, err
	}
	nodeSig, err := f.cfg.SignMessage(f.cfg.IDKey, chanAnnMsg)
	if err != nil {
		return nil, errors.Errorf("unable to generate node "+
			"signature for channel announcement: %v", err)
	}
	bitcoinSig, err := f.cfg.SignMessage(localFundingKey, chanAnnMsg)
	if err != nil {
		return nil, errors.Errorf("unable to generate bitcoin "+
			"signature for node public key: %v", err)
	}

	// Finally, we'll generate the announcement proof which we'll use to
	// provide the other side with the necessary signatures required to
	// allow them to reconstruct the full channel announcement.
	proof := &lnwire.AnnounceSignatures{
		ChannelID:        chanID,
		ShortChannelID:   shortChanID,
		NodeSignature:    nodeSig,
		BitcoinSignature: bitcoinSig,
	}

	return &chanAnnouncement{
		chanAnn:       chanAnn,
		chanUpdateAnn: chanUpdateAnn,
		chanProof:     proof,
	}, nil
}

// announceChannel announces a newly created channel to the rest of the network
// by crafting the two authenticated announcements required for the peers on
// the network to recognize the legitimacy of the channel. The crafted
// announcements are then sent to the channel router to handle broadcasting to
// the network during its next trickle.
// This method is synchronous and will return when all the network requests
// finish, either successfully or with an error.
func (f *fundingManager) announceChannel(localIDKey, remoteIDKey, localFundingKey,
	remoteFundingKey *btcec.PublicKey, shortChanID lnwire.ShortChannelID,
	chanID lnwire.ChannelID) error {

	// First, we'll create the batch of announcements to be sent upon
	// initial channel creation. This includes the channel announcement
	// itself, the channel update announcement, and our half of the channel
	// proof needed to fully authenticate the channel.
	ann, err := f.newChanAnnouncement(localIDKey, remoteIDKey,
		localFundingKey, remoteFundingKey, shortChanID, chanID)
	if err != nil {
		fndgLog.Errorf("can't generate channel announcement: %v", err)
		return err
	}

	// With the announcements crafted, we'll now send the announcements to
	// the rest of the network.
	//
	// TODO(roasbeef): add flag that indicates if should be announced or
	// not

	// The announcement message consists of three distinct messages:
	// 1. channel announcement 2. channel update 3. channel proof
	// We must wait for them all to be successfully announced to the
	// network, and/ if either fails we consider the announcement
	// unsuccessful.
	if err = f.cfg.SendAnnouncement(ann.chanAnn); err != nil {
		return err
	}
	if err = f.cfg.SendAnnouncement(ann.chanUpdateAnn); err != nil {
		return err
	}
	if err = f.cfg.SendAnnouncement(ann.chanProof); err != nil {
		return err
	}

	// Now that the channel is announced to the network, we will also
	// obtain and send a node announcement. This is done since a node
	// announcement is only accepted after a channel is known for that
	// particular node, and this might be our first channel.
	nodeAnn, err := f.cfg.CurrentNodeAnnouncement()
	if err != nil {
		fndgLog.Errorf("can't generate node announcement: %v", err)
		return err
	}

	if err = f.cfg.SendAnnouncement(&nodeAnn); err != nil {
		return err
	}
	return nil
}

// initFundingWorkflow sends a message to the funding manager instructing it
// to initiate a single funder workflow with the source peer.
// TODO(roasbeef): re-visit blocking nature..
func (f *fundingManager) initFundingWorkflow(peerAddress *lnwire.NetAddress,
	req *openChanReq) {

	f.fundingRequests <- &initFundingMsg{
		peerAddress: peerAddress,
		openChanReq: req,
	}
}

// handleInitFundingMsg creates a channel reservation within the daemon's
// wallet, then sends a funding request to the remote peer kicking off the
// funding workflow.
func (f *fundingManager) handleInitFundingMsg(msg *initFundingMsg) {
	var (
		// TODO(roasbeef): add delay
		peerKey      = msg.peerAddress.IdentityKey
		localAmt     = msg.localFundingAmt
		remoteAmt    = msg.remoteFundingAmt
		capacity     = localAmt + remoteAmt
		ourDustLimit = lnwallet.DefaultDustLimit()
	)

	fndgLog.Infof("Initiating fundingRequest(localAmt=%v, remoteAmt=%v, "+
		"capacity=%v, chainhash=%v, addr=%v, dustLimit=%v)", localAmt,
		msg.pushAmt, capacity, msg.chainHash, msg.peerAddress.Address,
		ourDustLimit)

	// First, we'll query the fee estimator for a fee that should get the
	// commitment transaction into the next block (conf target of 1). We
	// target the next block here to ensure that we'll be able to execute a
	// timely unilateral channel closure if needed.
	//
	// TODO(roasbeef): shouldn't be targeting next block
	feePerWeight := btcutil.Amount(f.cfg.FeeEstimator.EstimateFeePerWeight(1))

	// The protocol currently operates on the basis of fee-per-kw, so we'll
	// multiply the computed sat/weight by 1000 to arrive at fee-per-kw.
	feePerKw := feePerWeight * 1000

	// Initialize a funding reservation with the local wallet. If the
	// wallet doesn't have enough funds to commit to this channel, then the
	// request will fail, and be aborted.
	reservation, err := f.cfg.Wallet.InitChannelReservation(capacity,
		localAmt, msg.pushAmt, feePerKw, peerKey,
		msg.peerAddress.Address, &msg.chainHash)
	if err != nil {
		msg.err <- err
		return
	}

	// Obtain a new pending channel ID which is used to track this
	// reservation throughout its lifetime.
	chanID := f.nextPendingChanID()

	fndgLog.Infof("Target sat/kw for pendingID(%x): %v", chanID,
		int64(feePerKw))

	// If a pending channel map for this peer isn't already created, then
	// we create one, ultimately allowing us to track this pending
	// reservation within the target peer.
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}

	f.activeReservations[peerIDKey][chanID] = &reservationWithCtx{
		chanAmt:     capacity,
		reservation: reservation,
		peerAddress: msg.peerAddress,
		updates:     msg.updates,
		err:         msg.err,
	}
	f.resMtx.Unlock()

	// Using the RequiredRemoteDelay closure, we'll compute the remote CSV
	// delay we require given the total amount of funds within the channel.
	remoteCsvDelay := f.cfg.RequiredRemoteDelay(capacity)

	// Once the reservation has been created, and indexed, queue a funding
	// request to the remote peer, kicking off the funding workflow.
	ourContribution := reservation.OurContribution()

	// Finally, we'll use the current value of the channels and our default
	// policy to determine of required commitment constraints for the
	// remote party.
	chanReserve, maxValue, maxHtlcs := reservation.RemoteChanConstraints()

	fndgLog.Infof("Starting funding workflow with %v for pendingID(%x)",
		msg.peerAddress.Address, chanID)

	fundingOpen := lnwire.OpenChannel{
		ChainHash:            *f.cfg.Wallet.Cfg.NetParams.GenesisHash,
		PendingChannelID:     chanID,
		FundingAmount:        capacity,
		PushAmount:           msg.pushAmt,
		DustLimit:            ourContribution.DustLimit,
		MaxValueInFlight:     maxValue,
		ChannelReserve:       chanReserve,
		HtlcMinimum:          ourContribution.MinHTLC,
		FeePerKiloWeight:     uint32(feePerKw),
		CsvDelay:             uint16(remoteCsvDelay),
		MaxAcceptedHTLCs:     maxHtlcs,
		FundingKey:           ourContribution.MultiSigKey,
		RevocationPoint:      ourContribution.RevocationBasePoint,
		PaymentPoint:         ourContribution.PaymentBasePoint,
		DelayedPaymentPoint:  ourContribution.DelayBasePoint,
		FirstCommitmentPoint: ourContribution.FirstCommitmentPoint,
	}
	if err := f.cfg.SendToPeer(peerKey, &fundingOpen); err != nil {
		fndgLog.Errorf("Unable to send funding request message: %v", err)
		msg.err <- err
		return
	}
}

// waitUntilChannelOpen is designed to prevent other lnd subsystems from
// sending new update messages to a channel before the channel is fully
// opened.
func (f *fundingManager) waitUntilChannelOpen(targetChan lnwire.ChannelID) {
	f.barrierMtx.RLock()
	barrier, ok := f.newChanBarriers[targetChan]
	f.barrierMtx.RUnlock()
	if ok {
		fndgLog.Tracef("waiting for chan barrier signal for ChanID(%v)",
			targetChan)

		select {
		case <-barrier:
		case <-f.quit: // TODO(roasbeef): add timer?
			break
		}

		fndgLog.Tracef("barrier for ChanID(%v) closed", targetChan)
	}
}

// processErrorGeneric sends a message to the fundingManager allowing it to
// process the occurred generic error.
func (f *fundingManager) processFundingError(err *lnwire.Error,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingErrorMsg{err, peerAddress}
}

// handleErrorGenericMsg process the error which was received from remote peer,
// depending on the type of error we should do different clean up steps and
// inform the user about it.
func (f *fundingManager) handleErrorMsg(fmsg *fundingErrorMsg) {
	protocolErr := fmsg.err

	peerKey := fmsg.peerAddress.IdentityKey
	chanID := fmsg.err.ChanID

	// First, we'll attempt to retrieve the funding workflow that this
	// error was tied to. If we're unable to do so, then we'll exit early
	// as this was an unwarranted error.
	resCtx, err := f.getReservationCtx(peerKey, chanID)
	if err != nil {
		fndgLog.Warnf("Received error for non-existent funding "+
			"flow: %v", spew.Sdump(protocolErr))
		return
	}

	// If we did indeed find the funding workflow, then we'll return the
	// error back to the caller (if any), and cancel the workflow itself.
	lnErr := lnwire.ErrorCode(protocolErr.Data[0])
	fndgLog.Errorf("Received funding error from %x: %v",
		peerKey.SerializeCompressed(), lnErr,
	)
	resCtx.err <- grpc.Errorf(lnErr.ToGrpcCode(), lnErr.String())

	if _, err := f.cancelReservationCtx(peerKey, chanID); err != nil {
		fndgLog.Warnf("unable to delete reservation: %v", err)
		return
	}
}

// cancelReservationCtx do all needed work in order to securely cancel the
// reservation.
func (f *fundingManager) cancelReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte) (*reservationWithCtx, error) {

	fndgLog.Infof("Cancelling funding reservation for node_key=%x, "+
		"chan_id=%x", peerKey.SerializeCompressed(), pendingChanID)

	ctx, err := f.getReservationCtx(peerKey, pendingChanID)
	if err != nil {
		return nil, errors.Errorf("unable to find reservation: %v",
			err)
	}

	if err := ctx.reservation.Cancel(); err != nil {
		return nil, errors.Errorf("unable to cancel reservation: %v",
			err)
	}

	f.deleteReservationCtx(peerKey, pendingChanID)
	return ctx, nil
}

// deleteReservationCtx deletes the reservation uniquely identified by the
// target public key of the peer, and the specified pending channel ID.
func (f *fundingManager) deleteReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte) {

	// TODO(roasbeef): possibly cancel funding barrier in peer's
	// channelManager?
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	delete(f.activeReservations[peerIDKey], pendingChanID)
	f.resMtx.Unlock()
}

// getReservationCtx returns the reservation context for a particular pending
// channel ID for a target peer.
func (f *fundingManager) getReservationCtx(peerKey *btcec.PublicKey,
	pendingChanID [32]byte) (*reservationWithCtx, error) {

	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.RLock()
	resCtx, ok := f.activeReservations[peerIDKey][pendingChanID]
	f.resMtx.RUnlock()

	if !ok {
		return nil, errors.Errorf("unknown channel (id: %x)",
			pendingChanID)
	}

	return resCtx, nil
}

func copyPubKey(pub *btcec.PublicKey) *btcec.PublicKey {
	return &btcec.PublicKey{
		Curve: btcec.S256(),
		X:     pub.X,
		Y:     pub.Y,
	}
}

// saveChannelOpeningState saves the channelOpeningState for the provided
// chanPoint to the channelOpeningStateBucket.
func (f *fundingManager) saveChannelOpeningState(chanPoint *wire.OutPoint,
	state channelOpeningState, shortChanID *lnwire.ShortChannelID) error {
	return f.cfg.Wallet.Cfg.Database.Update(func(tx *bolt.Tx) error {

		bucket, err := tx.CreateBucketIfNotExists(channelOpeningStateBucket)
		if err != nil {
			return err
		}

		var outpointBytes bytes.Buffer
		if err = writeOutpoint(&outpointBytes, chanPoint); err != nil {
			return err
		}

		// Save state and the uint64 representation of the shortChanID
		// for later use.
		scratch := make([]byte, 10)
		byteOrder.PutUint16(scratch[:2], uint16(state))
		byteOrder.PutUint64(scratch[2:], shortChanID.ToUint64())

		if err = bucket.Put(outpointBytes.Bytes(), scratch); err != nil {
			return err
		}
		return nil
	})
}

// getChannelOpeningState fetches the channelOpeningState for the provided
// chanPoint from the database, or returns ErrChannelNotFound if the channel
// is not found.
func (f *fundingManager) getChannelOpeningState(chanPoint *wire.OutPoint) (
	channelOpeningState, *lnwire.ShortChannelID, error) {

	var state channelOpeningState
	var shortChanID lnwire.ShortChannelID
	err := f.cfg.Wallet.Cfg.Database.View(func(tx *bolt.Tx) error {

		bucket := tx.Bucket(channelOpeningStateBucket)
		if bucket == nil {
			// If the bucket does not exist, it means we never added
			//  a channel to the db, so return ErrChannelNotFound.
			return ErrChannelNotFound
		}

		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, chanPoint); err != nil {
			return err
		}

		value := bucket.Get(outpointBytes.Bytes())
		if value == nil {
			return ErrChannelNotFound
		}

		state = channelOpeningState(byteOrder.Uint16(value[:2]))
		shortChanID = lnwire.NewShortChanIDFromInt(byteOrder.Uint64(value[2:]))
		return nil
	})
	if err != nil {
		return 0, nil, err
	}

	return state, &shortChanID, nil
}

// deleteChannelOpeningState removes any state for chanPoint from the database.
func (f *fundingManager) deleteChannelOpeningState(chanPoint *wire.OutPoint) error {
	return f.cfg.Wallet.Cfg.Database.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(channelOpeningStateBucket)
		if bucket == nil {
			return fmt.Errorf("Bucket not found")
		}

		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, chanPoint); err != nil {
			return err
		}

		if err := bucket.Delete(outpointBytes.Bytes()); err != nil {
			return err
		}
		return nil
	})
}
