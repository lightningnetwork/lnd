package main

import (
	"bytes"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"google.golang.org/grpc"
)

const (
	// TODO(roasbeef): tune
	msgBufferSize = 50
)

// reservationWithCtx encapsulates a pending channel reservation. This wrapper
// struct is used internally within the funding manager to track and progress
// the funding workflow initiated by incoming/outgoing methods from the target
// peer. Additionally, this struct houses a response and error channel which is
// used to respond to the caller in the case a channel workflow is initiated
// via a local signal such as RPC.
// TODO(roasbeef): actually use the context package
//  * deadlines, etc.
type reservationWithCtx struct {
	reservation *lnwallet.ChannelReservation
	peerAddress *lnwire.NetAddress

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

// fundingRequestMsg couples an lnwire.SingleFundingRequest message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingRequestMsg struct {
	msg         *lnwire.SingleFundingRequest
	peerAddress *lnwire.NetAddress
}

// fundingResponseMsg couples an lnwire.SingleFundingResponse message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingResponseMsg struct {
	msg         *lnwire.SingleFundingResponse
	peerAddress *lnwire.NetAddress
}

// fundingCompleteMsg couples an lnwire.SingleFundingComplete message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingCompleteMsg struct {
	msg         *lnwire.SingleFundingComplete
	peerAddress *lnwire.NetAddress
}

// fundingSignCompleteMsg couples an lnwire.SingleFundingSignComplete message
// with the peer who sent the message. This allows the funding manager to
// queue a response directly to the peer, progressing the funding workflow.
type fundingSignCompleteMsg struct {
	msg         *lnwire.SingleFundingSignComplete
	peerAddress *lnwire.NetAddress
}

// fundingLockedMsg couples an lnwire.FundingLocked message with the peer who
// sent the message. This allows the funding manager to finalize the funding
// process and announce the existence of the new channel.
type fundingLockedMsg struct {
	msg         *lnwire.FundingLocked
	peerAddress *lnwire.NetAddress
}

// fundingErrorMsg couples an lnwire.ErrorGeneric message
// with the peer who sent the message. This allows the funding
// manager to properly process the error.
type fundingErrorMsg struct {
	err         *lnwire.ErrorGeneric
	peerAddress *lnwire.NetAddress
}

// pendingChannels is a map instantiated per-peer which tracks all active
// pending single funded channels indexed by their pending channel identifier.
type pendingChannels map[uint64]*reservationWithCtx

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

	// ArbiterChan allows the FundingManager to notify the BreachArbiter
	// that a new channel has been created that should be observed to
	// ensure that the channel counterparty hasn't broadcasted an invalid
	// commitment transaction.
	ArbiterChan chan<- *lnwallet.LightningChannel

	// Notifier is used by the FundingManager to determine when the
	// channel's funding transaction has been confirmed on the blockchain
	// so that the channel creation process can be completed.
	Notifier chainntnfs.ChainNotifier

	// SendToRouter is used by the FundingManager to announce newly created
	// channels to the rest of the Lightning Network.
	SendToRouter func(msg lnwire.Message)

	// SendToPeer allows the FundingManager to send messages to the peer
	// node during the multiple steps involved in the creation of the
	// channel's funding transaction and initial commitment transaction.
	SendToPeer func(target *btcec.PublicKey, msgs ...lnwire.Message) error

	// FindPeer searches the list of peers connected to the node so that
	// the FundingManager can notify other daemon subsystems as necessary
	// during the funding process.
	FindPeer func(peerKey *btcec.PublicKey) (*peer, error)

	// FindChannel queries the database for the channel with the given
	// funding transaction outpoint.
	FindChannel func(chanPoint wire.OutPoint) (*lnwallet.LightningChannel, error)
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

	// channelReservations is a map which houses the state of all pending
	// funding workflows.
	resMtx             sync.RWMutex
	activeReservations map[serializedPubKey]pendingChannels

	// fundingMsgs is a channel which receives wrapped wire messages
	// related to funding workflow from outside peers.
	fundingMsgs chan interface{}

	// queries is a channel which receives requests to query the internal
	// state of the funding manager.
	queries chan interface{}

	// fundingRequests is a channel used to receive channel initiation
	// requests from a local subsystem within the daemon.
	fundingRequests chan *initFundingMsg

	// newChanBarriers is a map from a channel point to a 'barrier' which
	// will be signalled once the channel is fully open. This barrier acts
	// as a synchronization point for any incoming/outgoing HTLCs before
	// the channel has been fully opened.
	barrierMtx      sync.RWMutex
	newChanBarriers map[wire.OutPoint]chan struct{}

	fakeProof *channelProof

	quit chan struct{}
	wg   sync.WaitGroup
}

// newFundingManager creates and initializes a new instance of the
// fundingManager.
func newFundingManager(cfg fundingConfig) (*fundingManager, error) {
	// TODO(roasbeef): remove once we actually sign the funding_locked
	// stuffs
	s := "30450221008ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa" +
		"1bf0314f882d70220299105481d63e0f4bc2a88121167221b6700d72a0e" +
		"ad154c03be696a292d24ae"
	fakeSigHex, _ := hex.DecodeString(s)
	fakeSig, _ := btcec.ParseSignature(fakeSigHex, btcec.S256())

	return &fundingManager{
		cfg: &cfg,

		fakeProof: &channelProof{
			nodeSig:    fakeSig,
			bitcoinSig: fakeSig,
		},

		activeReservations: make(map[serializedPubKey]pendingChannels),
		newChanBarriers:    make(map[wire.OutPoint]chan struct{}),
		fundingMsgs:        make(chan interface{}, msgBufferSize),
		fundingRequests:    make(chan *initFundingMsg, msgBufferSize),
		queries:            make(chan interface{}, 1),
		quit:               make(chan struct{}),
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
	pendingChannels, err := f.cfg.Wallet.ChannelDB.FetchPendingChannels()
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
			"barrier", *channel.FundingOutpoint)
		f.newChanBarriers[*channel.FundingOutpoint] = make(chan struct{})
		f.barrierMtx.Unlock()

		doneChan := make(chan struct{})
		go f.waitForFundingConfirmation(channel, doneChan)
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

type numPendingReq struct {
	resp chan uint32
	err  chan error
}

// NumPendingChannels returns the number of pending channels currently
// progressing through the reservation workflow.
func (f *fundingManager) NumPendingChannels() (uint32, error) {
	respChan := make(chan uint32, 1)
	errChan := make(chan error)

	req := &numPendingReq{
		resp: respChan,
		err:  errChan,
	}
	f.queries <- req

	return <-respChan, <-errChan
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
			case *fundingRequestMsg:
				f.handleFundingRequest(fmsg)
			case *fundingResponseMsg:
				f.handleFundingResponse(fmsg)
			case *fundingCompleteMsg:
				f.handleFundingComplete(fmsg)
			case *fundingSignCompleteMsg:
				f.handleFundingSignComplete(fmsg)
			case *fundingLockedMsg:
				f.handleFundingLocked(fmsg)
			case *fundingErrorMsg:
				f.handleErrorGenericMsg(fmsg)
			}
		case req := <-f.fundingRequests:
			f.handleInitFundingMsg(req)
		case req := <-f.queries:
			switch msg := req.(type) {
			case *numPendingReq:
				f.handleNumPending(msg)
			case *pendingChansReq:
				f.handlePendingChannels(msg)
			}
		case <-f.quit:
			return
		}
	}
}

// handleNumPending handles a request for the total number of pending channels.
func (f *fundingManager) handleNumPending(msg *numPendingReq) {
	var numPending uint32
	for _, peerChannels := range f.activeReservations {
		numPending += uint32(len(peerChannels))
	}

	dbPendingChannels, err := f.cfg.Wallet.ChannelDB.FetchPendingChannels()
	if err != nil {
		close(msg.resp)
		msg.err <- err
		return
	}

	numPending = numPending + uint32(len(dbPendingChannels))

	msg.resp <- numPending
	msg.err <- nil
}

// handlePendingChannels responds to a request for details concerning all
// currently pending channels waiting for the final phase of the funding
// workflow (funding txn confirmation).
func (f *fundingManager) handlePendingChannels(msg *pendingChansReq) {
	var pendingChannels []*pendingChannel

	dbPendingChannels, err := f.cfg.Wallet.ChannelDB.FetchPendingChannels()
	if err != nil {
		msg.resp <- nil
		msg.err <- err
		return
	}

	for _, dbPendingChan := range dbPendingChannels {
		pendingChan := &pendingChannel{
			identityPub:   dbPendingChan.IdentityPub,
			channelPoint:  dbPendingChan.ChanID,
			capacity:      dbPendingChan.Capacity,
			localBalance:  dbPendingChan.OurBalance,
			remoteBalance: dbPendingChan.TheirBalance,
		}

		pendingChannels = append(pendingChannels, pendingChan)
	}

	msg.resp <- pendingChannels
	msg.err <- nil
}

// processFundingRequest sends a message to the fundingManager allowing it to
// initiate the new funding workflow with the source peer.
func (f *fundingManager) processFundingRequest(msg *lnwire.SingleFundingRequest,
	peerAddress *lnwire.NetAddress) {
	f.fundingMsgs <- &fundingRequestMsg{msg, peerAddress}
}

// handleFundingRequest creates an initial 'ChannelReservation' within
// the wallet, then responds to the source peer with a single funder response
// message progressing the funding workflow.
// TODO(roasbeef): add error chan to all, let channelManager handle
// error+propagate
func (f *fundingManager) handleFundingRequest(fmsg *fundingRequestMsg) {
	// Check number of pending channels to be smaller than maximum allowed
	// number and send ErrorGeneric to remote peer if condition is violated.
	peerIDKey := newSerializedKey(fmsg.peerAddress.IdentityKey)

	if len(f.activeReservations[peerIDKey]) >= cfg.MaxPendingChannels {
		errMsg := &lnwire.ErrorGeneric{
			ChannelPoint: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 0,
			},
			Problem:          "Number of pending channels exceed maximum",
			Code:             lnwire.ErrMaxPendingChannels,
			PendingChannelID: fmsg.msg.ChannelID,
		}
		if err := f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, errMsg); err != nil {
			fndgLog.Errorf("unable to send max pending channels "+
				"message to peer: %v", err)
			return
		}
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
		errMsg := &lnwire.ErrorGeneric{
			ChannelPoint: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 0,
			},
			Problem:          "Synchronizing blockchain",
			Code:             lnwire.ErrSynchronizingChain,
			PendingChannelID: fmsg.msg.ChannelID,
		}
		if err := f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, errMsg); err != nil {
			fndgLog.Errorf("unable to send error message to peer %v", err)
			return
		}
		return
	}

	msg := fmsg.msg
	amt := msg.FundingAmount
	delay := msg.CsvDelay

	// TODO(roasbeef): error if funding flow already ongoing
	fndgLog.Infof("Recv'd fundingRequest(amt=%v, delay=%v, pendingId=%v) "+
		"from peer(%x)", amt, msg.PushSatoshis, delay, msg.ChannelID,
		fmsg.peerAddress.IdentityKey.SerializeCompressed())

	ourDustLimit := lnwallet.DefaultDustLimit()
	theirDustlimit := msg.DustLimit

	// Attempt to initialize a reservation within the wallet. If the wallet
	// has insufficient resources to create the channel, then the reservation
	// attempt may be rejected. Note that since we're on the responding
	// side of a single funder workflow, we don't commit any funds to the
	// channel ourselves.
	// TODO(roasbeef): passing num confs 1 is irrelevant here, make signed?
	// TODO(roasbeef): assuming this was an inbound connection, replace
	// port with default advertised port
	reservation, err := f.cfg.Wallet.InitChannelReservation(amt, 0,
		fmsg.peerAddress.IdentityKey, fmsg.peerAddress.Address,
		uint16(fmsg.msg.ConfirmationDepth), delay, ourDustLimit,
		msg.PushSatoshis)
	if err != nil {
		// TODO(roasbeef): push ErrorGeneric message
		fndgLog.Errorf("Unable to initialize reservation: %v", err)
		return
	}

	reservation.SetTheirDustLimit(theirDustlimit)

	// Once the reservation has been created successfully, we add it to this
	// peers map of pending reservations to track this particular reservation
	// until either abort or completion.
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}
	f.activeReservations[peerIDKey][msg.ChannelID] = &reservationWithCtx{
		reservation: reservation,
		err:         make(chan error, 1),
		peerAddress: fmsg.peerAddress,
	}
	f.resMtx.Unlock()

	cancelReservation := func() {
		_, err := f.cancelReservationCtx(fmsg.peerAddress.IdentityKey, msg.ChannelID)
		if err != nil {
			fndgLog.Errorf("unable to cancel reservation: %v", err)
		}
	}

	// With our portion of the reservation initialized, process the
	// initiators contribution to the channel.
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(msg.DeliveryPkScript, activeNetParams.Params)
	if err != nil {
		fndgLog.Errorf("Unable to extract addresses from script: %v", err)
		cancelReservation()
		return
	}
	contribution := &lnwallet.ChannelContribution{
		FundingAmount:   amt,
		MultiSigKey:     copyPubKey(msg.ChannelDerivationPoint),
		CommitKey:       copyPubKey(msg.CommitmentKey),
		DeliveryAddress: addrs[0],
		CsvDelay:        delay,
	}
	if err := reservation.ProcessSingleContribution(contribution); err != nil {
		fndgLog.Errorf("unable to add contribution reservation: %v", err)
		cancelReservation()
		return
	}

	fndgLog.Infof("Sending fundingResp for pendingID(%v)", msg.ChannelID)

	// With the initiator's contribution recorded, respond with our
	// contribution in the next message of the workflow.
	ourContribution := reservation.OurContribution()
	deliveryScript, err := txscript.PayToAddrScript(ourContribution.DeliveryAddress)
	if err != nil {
		fndgLog.Errorf("unable to convert address to pkscript: %v", err)
		cancelReservation()
		return
	}
	fundingResp := lnwire.NewSingleFundingResponse(msg.ChannelID,
		ourContribution.RevocationKey, ourContribution.CommitKey,
		ourContribution.MultiSigKey, ourContribution.CsvDelay,
		deliveryScript, ourDustLimit, msg.ConfirmationDepth)

	if err := f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, fundingResp); err != nil {
		fndgLog.Errorf("unable to send funding response to peer: %v", err)
		cancelReservation()
		return
	}
}

// processFundingRequest sends a message to the fundingManager allowing it to
// continue the second phase of a funding workflow with the target peer.
func (f *fundingManager) processFundingResponse(msg *lnwire.SingleFundingResponse,
	peerAddress *lnwire.NetAddress) {
	f.fundingMsgs <- &fundingResponseMsg{msg, peerAddress}
}

// handleFundingResponse processes a response to the workflow initiation sent
// by the remote peer. This message then queues a message with the funding
// outpoint, and a commitment signature to the remote peer.
func (f *fundingManager) handleFundingResponse(fmsg *fundingResponseMsg) {
	msg := fmsg.msg
	chanID := fmsg.msg.ChannelID
	peerKey := fmsg.peerAddress.IdentityKey

	resCtx, err := f.getReservationCtx(peerKey, chanID)
	if err != nil {
		fndgLog.Warnf("Can't find reservation (peerKey:%v, chanID:%v)",
			peerKey, chanID)
		return
	}

	cancelReservation := func() {
		if _, err := f.cancelReservationCtx(peerKey, chanID); err != nil {
			fndgLog.Errorf("unable to cancel reservation: %v", err)
		}
	}

	fndgLog.Infof("Recv'd fundingResponse for pendingID(%v)", msg.ChannelID)

	resCtx.reservation.SetTheirDustLimit(msg.DustLimit)

	// The remote node has responded with their portion of the channel
	// contribution. At this point, we can process their contribution which
	// allows us to construct and sign both the commitment transaction, and
	// the funding transaction.
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(msg.DeliveryPkScript,
		activeNetParams.Params)
	if err != nil {
		fndgLog.Errorf("Unable to extract addresses from script: %v", err)
		cancelReservation()
		resCtx.err <- err
		return
	}
	contribution := &lnwallet.ChannelContribution{
		FundingAmount:   0,
		MultiSigKey:     copyPubKey(msg.ChannelDerivationPoint),
		CommitKey:       copyPubKey(msg.CommitmentKey),
		DeliveryAddress: addrs[0],
		RevocationKey:   copyPubKey(msg.RevocationKey),
		CsvDelay:        msg.CsvDelay,
	}
	if err := resCtx.reservation.ProcessContribution(contribution); err != nil {
		fndgLog.Errorf("Unable to process contribution from %v: %v",
			fmsg.peerAddress.IdentityKey, err)
		cancelReservation()
		resCtx.err <- err
		return
	}

	// Now that we have their contribution, we can extract, then send over
	// both the funding out point and our signature for their version of
	// the commitment transaction to the remote peer.
	outPoint := resCtx.reservation.FundingOutpoint()
	_, sig := resCtx.reservation.OurSignatures()
	commitSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		fndgLog.Errorf("Unable to parse signature: %v", err)
		cancelReservation()
		resCtx.err <- err
		return
	}

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	fndgLog.Debugf("Creating chan barrier for "+
		"ChannelPoint(%v)", outPoint)
	f.newChanBarriers[*outPoint] = make(chan struct{})
	f.barrierMtx.Unlock()

	fndgLog.Infof("Generated ChannelPoint(%v) for pendingID(%v)", outPoint,
		chanID)

	revocationKey := resCtx.reservation.OurContribution().RevocationKey
	obsfucator := resCtx.reservation.StateNumObfuscator()

	fundingComplete := lnwire.NewSingleFundingComplete(chanID, *outPoint,
		commitSig, revocationKey, obsfucator)

	if err := f.cfg.SendToPeer(fmsg.peerAddress.IdentityKey, fundingComplete); err != nil {
		fndgLog.Errorf("Unable to send funding complete message: %v", err)
		cancelReservation()
		resCtx.err <- err
		return
	}
}

// processFundingComplete queues a funding complete message coupled with the
// source peer to the fundingManager.
func (f *fundingManager) processFundingComplete(msg *lnwire.SingleFundingComplete,
	peerAddress *lnwire.NetAddress) {
	f.fundingMsgs <- &fundingCompleteMsg{msg, peerAddress}
}

// handleFundingComplete progresses the funding workflow when the daemon is on
// the responding side of a single funder workflow. Once this message has been
// processed, a signature is sent to the remote peer allowing it to broadcast
// the funding transaction, progressing the workflow into the final stage.
func (f *fundingManager) handleFundingComplete(fmsg *fundingCompleteMsg) {
	peerKey := fmsg.peerAddress.IdentityKey
	chanID := fmsg.msg.ChannelID

	resCtx, err := f.getReservationCtx(peerKey, chanID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			peerKey, chanID)
		return
	}

	cancelReservation := func() {
		if _, err := f.cancelReservationCtx(peerKey, chanID); err != nil {
			fndgLog.Errorf("unable to cancel reservation: %v", err)
		}
	}

	// The channel initiator has responded with the funding outpoint of the
	// final funding transaction, as well as a signature for our version of
	// the commitment transaction. So at this point, we can validate the
	// inititator's commitment transaction, then send our own if it's valid.
	// TODO(roasbeef): make case (p vs P) consistent throughout
	fundingOut := fmsg.msg.FundingOutPoint
	fndgLog.Infof("completing pendingID(%v) with ChannelPoint(%v)",
		chanID, fundingOut,
	)

	revokeKey := copyPubKey(fmsg.msg.RevocationKey)
	obsfucator := fmsg.msg.StateHintObsfucator
	commitSig := fmsg.msg.CommitSignature.Serialize()

	// With all the necessary data available, attempt to advance the
	// funding workflow to the next stage. If this succeeds then the
	// funding transaction will broadcast after our next message.
	completeChan, err := resCtx.reservation.CompleteReservationSingle(
		revokeKey, &fundingOut, commitSig, obsfucator)
	if err != nil {
		// TODO(roasbeef): better error logging: peerID, channelID, etc.
		fndgLog.Errorf("unable to complete single reservation: %v", err)
		cancelReservation()
		return
	}

	// With their signature for our version of the commitment transaction
	// verified, we can now send over our signature to the remote peer.
	// TODO(roasbeef): just have raw bytes in wire msg? avoids decoding
	// then decoding shortly afterwards.
	_, sig := resCtx.reservation.OurSignatures()
	ourCommitSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		fndgLog.Errorf("unable to parse signature: %v", err)
		cancelReservation()
		return
	}

	// A new channel has almost finished the funding process. In order to
	// properly synchronize with the writeHandler goroutine, we add a new
	// channel to the barriers map which will be closed once the channel is
	// fully open.
	f.barrierMtx.Lock()
	fndgLog.Debugf("Creating chan barrier for "+
		"ChannelPoint(%v)", fundingOut)
	f.newChanBarriers[fundingOut] = make(chan struct{})
	f.barrierMtx.Unlock()

	fndgLog.Infof("sending signComplete for pendingID(%v) over ChannelPoint(%v)",
		chanID, fundingOut)

	signComplete := lnwire.NewSingleFundingSignComplete(chanID, ourCommitSig)
	if err := f.cfg.SendToPeer(peerKey, signComplete); err != nil {
		fndgLog.Errorf("unable to send signComplete message: %v", err)
		cancelReservation()
		return
	}

	go func() {
		doneChan := make(chan struct{})
		go f.waitForFundingConfirmation(completeChan, doneChan)

		<-doneChan
		f.deleteReservationCtx(peerKey, fmsg.msg.ChannelID)
	}()
}

// processFundingSignComplete sends a single funding sign complete message
// along with the source peer to the funding manager.
func (f *fundingManager) processFundingSignComplete(msg *lnwire.SingleFundingSignComplete,
	peerAddress *lnwire.NetAddress) {
	f.fundingMsgs <- &fundingSignCompleteMsg{msg, peerAddress}
}

// handleFundingSignComplete processes the final message received in a single
// funder workflow. Once this message is processed, the funding transaction is
// broadcast. Once the funding transaction reaches a sufficient number of
// confirmations, a message is sent to the responding peer along with a compact
// encoding of the location of the channel within the blockchain.
func (f *fundingManager) handleFundingSignComplete(fmsg *fundingSignCompleteMsg) {
	chanID := fmsg.msg.ChannelID
	peerKey := fmsg.peerAddress.IdentityKey

	resCtx, err := f.getReservationCtx(peerKey, chanID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			peerKey, chanID)
		return
	}

	// The remote peer has responded with a signature for our commitment
	// transaction. We'll verify the signature for validity, then commit
	// the state to disk as we can now open the channel.
	commitSig := fmsg.msg.CommitSignature.Serialize()
	completeChan, err := resCtx.reservation.CompleteReservation(nil, commitSig)
	if err != nil {
		fndgLog.Errorf("unable to complete reservation sign complete: %v", err)
		resCtx.err <- err
		return
	}

	fundingPoint := resCtx.reservation.FundingOutpoint()
	fndgLog.Infof("Finalizing pendingID(%v) over ChannelPoint(%v), "+
		"waiting for channel open on-chain", chanID, fundingPoint)

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
		go f.waitForFundingConfirmation(completeChan, doneChan)

		<-doneChan

		// Finally give the caller a final update notifying them that
		// the channel is now open.
		// TODO(roasbeef): helper funcs for proto construction
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

		f.deleteReservationCtx(peerKey, fmsg.msg.ChannelID)
	}()
}

// waitForFundingConfirmation handles the final stages of the channel funding
// process once the funding transaction has been broadcast. The primary
// function of waitForFundingConfirmation is to wait for blockchain
// confirmation, and then to notify the other systems that must be notified
// when a channel has become active for lightning transactions.
func (f *fundingManager) waitForFundingConfirmation(
	completeChan *channeldb.OpenChannel, doneChan chan struct{}) {

	// Register with the ChainNotifier for a notification once the funding
	// transaction reaches `numConfs` confirmations.
	txid := completeChan.FundingOutpoint.Hash
	numConfs := uint32(completeChan.NumConfsRequired)
	confNtfn, _ := f.cfg.Notifier.RegisterConfirmationsNtfn(&txid, numConfs)

	fndgLog.Infof("Waiting for funding tx (%v) to reach %v confirmations",
		txid, numConfs)

	// Wait until the specified number of confirmations has been reached,
	// or the wallet signals a shutdown.
	confDetails := <-confNtfn.Confirmed

	fundingPoint := *completeChan.FundingOutpoint
	fndgLog.Infof("ChannelPoint(%v) is now active",
		fundingPoint)

	completeChan.IsPending = false
	err := f.cfg.Wallet.ChannelDB.MarkChannelAsOpen(&fundingPoint)
	if err != nil {
		fndgLog.Errorf("error setting channel pending flag to false: "+
			"%v", err)
		return
	}

	// Finally, create and officially open the payment channel!
	// TODO(roasbeef): CreationTime once tx is 'open'
	channel, err := lnwallet.NewLightningChannel(f.cfg.Wallet.Signer,
		f.cfg.Notifier, completeChan)
	if err != nil {
		fndgLog.Errorf("error creating new lightning channel: %v", err)
		return
	}

	// Now that the channel is open, we need to notify a number of
	// parties of this event.

	// First we send the newly opened channel to the source server
	peer, err := f.cfg.FindPeer(completeChan.IdentityPub)
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

	<-newChanDone

	// Close the active channel barrier signalling the
	// readHandler that commitment related modifications to
	// this channel can now proceed.
	f.barrierMtx.Lock()
	fndgLog.Tracef("Closing chan barrier for ChannelPoint(%v)", fundingPoint)
	close(f.newChanBarriers[fundingPoint])
	delete(f.newChanBarriers, fundingPoint)
	f.barrierMtx.Unlock()

	// Afterwards we send the breach arbiter the new channel so it
	// can watch for attempts to breach the channel's contract by
	// the remote party.
	f.cfg.ArbiterChan <- channel

	// With the block height and the transaction index known, we
	// can construct the compact chainID which is used on the
	// network to unique identify channels.
	chanID := lnwire.ChannelID{
		BlockHeight: confDetails.BlockHeight,
		TxIndex:     confDetails.TxIndex,
		TxPosition:  uint16(fundingPoint.Index),
	}

	// When the funding transaction has been confirmed, the FundingLocked
	// message is sent to the remote peer so that the existence of the
	// channel can be announced to the network.
	fundingLockedMsg := lnwire.NewFundingLocked(fundingPoint, chanID,
		f.cfg.IDKey)
	f.cfg.SendToPeer(completeChan.IdentityPub, fundingLockedMsg)

	close(doneChan)
	return
}

// processFundingLocked sends a message to the fundingManager allowing it to finish
// the funding workflow.
func (f *fundingManager) processFundingLocked(msg *lnwire.FundingLocked,
	peerAddress *lnwire.NetAddress) {
	f.fundingMsgs <- &fundingLockedMsg{msg, peerAddress}
}

// handleFundingLocked finalizes the channel funding process and enables the channel
// to enter normal operating mode.
func (f *fundingManager) handleFundingLocked(fmsg *fundingLockedMsg) {
	fundingPoint := fmsg.msg.ChannelOutpoint
	channel, err := f.cfg.FindChannel(fundingPoint)
	if err != nil {
		fndgLog.Errorf("error looking up channel for outpoint: %v", fundingPoint)
		return
	}

	// Register the new link with the L3 routing manager so this
	// new channel can be utilized during path
	// finding.
	go f.announceChannel(f.cfg.IDKey, fmsg.peerAddress.IdentityKey, channel,
		fmsg.msg.ChannelID, f.fakeProof, f.fakeProof)
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
	chanAnn    *lnwire.ChannelAnnouncement
	edgeUpdate *lnwire.ChannelUpdateAnnouncement
}

// newChanAnnouncement creates the authenticated channel announcement messages
// required to broadcast a newly created channel to the network. The
// announcement is two part: the first part authenticates the existence of the
// channel and contains four signatures binding the funding pub keys and
// identity pub keys of both parties to the channel, and the second segment is
// authenticated only by us and contains our directional routing policy for the
// channel.
func newChanAnnouncement(localIdentity, remotePub *btcec.PublicKey,
	channel *lnwallet.LightningChannel, chanID lnwire.ChannelID,
	localProof, remoteProof *channelProof) *chanAnnouncement {

	// The unconditional section of the announcement is the ChannelID
	// itself which compactly encodes the location of the funding output
	// within the blockchain.
	chanAnn := &lnwire.ChannelAnnouncement{
		ChannelID: chanID,
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
	selfBytes := localIdentity.SerializeCompressed()
	remoteBytes := remotePub.SerializeCompressed()
	if bytes.Compare(selfBytes, remoteBytes) == -1 {
		chanAnn.FirstNodeID = localIdentity
		chanAnn.SecondNodeID = remotePub
		chanAnn.FirstNodeSig = localProof.nodeSig
		chanAnn.SecondNodeSig = remoteProof.nodeSig
		chanAnn.FirstBitcoinSig = localProof.nodeSig
		chanAnn.SecondBitcoinSig = remoteProof.nodeSig
		chanAnn.FirstBitcoinKey = channel.LocalFundingKey
		chanAnn.SecondBitcoinKey = channel.RemoteFundingKey

		// If we're the first node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 0
	} else {
		chanAnn.FirstNodeID = remotePub
		chanAnn.SecondNodeID = localIdentity
		chanAnn.FirstNodeSig = remoteProof.nodeSig
		chanAnn.SecondNodeSig = localProof.nodeSig
		chanAnn.FirstBitcoinSig = remoteProof.nodeSig
		chanAnn.SecondBitcoinSig = localProof.nodeSig
		chanAnn.FirstBitcoinKey = channel.RemoteFundingKey
		chanAnn.SecondBitcoinKey = channel.LocalFundingKey

		// If we're the second node then update the chanFlags to
		// indicate the "direction" of the update.
		chanFlags = 1
	}

	// TODO(roasbeef): add real sig, populate proper FeeSchema
	chanUpdateAnn := &lnwire.ChannelUpdateAnnouncement{
		Signature:                 localProof.nodeSig,
		ChannelID:                 chanID,
		Timestamp:                 uint32(time.Now().Unix()),
		Flags:                     chanFlags,
		Expiry:                    1,
		HtlcMinimumMstat:          0,
		FeeBaseMstat:              0,
		FeeProportionalMillionths: 0,
	}

	return &chanAnnouncement{
		chanAnn:    chanAnn,
		edgeUpdate: chanUpdateAnn,
	}
}

// announceChannel announces a newly created channel to the rest of the network
// by crafting the two authenticated announcements required for the peers on the
// network to recognize the legitimacy of the channel. The crafted
// announcements are then send to the channel router to handle broadcasting to
// the network during its next trickle.
func (f *fundingManager) announceChannel(idKey, remoteIdKey *btcec.PublicKey,
	channel *lnwallet.LightningChannel, chanID lnwire.ChannelID, localProof,
	remoteProof *channelProof) {

	// TODO(roasbeef): need a Signer.SignMessage method to finalize
	// advertisements
	chanAnnouncement := newChanAnnouncement(idKey, remoteIdKey, channel, chanID,
		localProof, remoteProof)

	f.cfg.SendToRouter(chanAnnouncement.chanAnn)
	f.cfg.SendToRouter(chanAnnouncement.edgeUpdate)
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
		numConfs     = msg.numConfs
		ourDustLimit = lnwallet.DefaultDustLimit()
	)

	fndgLog.Infof("Initiating fundingRequest(localAmt=%v, remoteAmt=%v, "+
		"capacity=%v, numConfs=%v, addr=%v, dustLimit=%v)", localAmt,
		msg.pushAmt, capacity, numConfs, msg.peerAddress.Address,
		ourDustLimit)

	// Initialize a funding reservation with the local wallet. If the
	// wallet doesn't have enough funds to commit to this channel, then
	// the request will fail, and be aborted.
	reservation, err := f.cfg.Wallet.InitChannelReservation(capacity,
		localAmt, peerKey, msg.peerAddress.Address, uint16(numConfs), 4,
		ourDustLimit, msg.pushAmt)
	if err != nil {
		msg.err <- err
		return
	}

	// Obtain a new pending channel ID which is used to track this
	// reservation throughout its lifetime.
	peer, err := f.cfg.FindPeer(peerKey)
	if err != nil {
		msg.err <- err
		return
	}

	chanID := peer.fetchNextPendingChanID()

	// If a pending channel map for this peer isn't already created, then
	// we create one, ultimately allowing us to track this pending
	// reservation within the target peer.
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	if _, ok := f.activeReservations[peerIDKey]; !ok {
		f.activeReservations[peerIDKey] = make(pendingChannels)
	}

	f.activeReservations[peerIDKey][chanID] = &reservationWithCtx{
		reservation: reservation,
		peerAddress: msg.peerAddress,
		updates:     msg.updates,
		err:         msg.err,
	}
	f.resMtx.Unlock()

	// Once the reservation has been created, and indexed, queue a funding
	// request to the remote peer, kicking off the funding workflow.
	contribution := reservation.OurContribution()
	deliveryScript, err := txscript.PayToAddrScript(contribution.DeliveryAddress)
	if err != nil {
		fndgLog.Errorf("Unable to convert address to pkscript: %v", err)
		msg.err <- err
		return
	}

	fndgLog.Infof("Starting funding workflow with for pendingID(%v)", chanID)

	// TODO(roasbeef): add FundingRequestFromContribution func
	// TODO(roasbeef): need to set fee/kb
	fundingReq := lnwire.NewSingleFundingRequest(
		chanID,
		msg.channelType,
		msg.coinType,
		0, // TODO(roasbeef): grab from fee estimation model
		capacity,
		contribution.CsvDelay,
		contribution.CommitKey,
		contribution.MultiSigKey,
		deliveryScript,
		ourDustLimit,
		msg.pushAmt,
		numConfs,
	)
	if err := f.cfg.SendToPeer(peerKey, fundingReq); err != nil {
		fndgLog.Errorf("Unable to send funding request message: %v", err)
		msg.err <- err
		return
	}
}

// waitUntilChannelOpen is designed to prevent other lnd subsystems from
// sending new update messages to a channel before the channel is fully
// opened.
func (f *fundingManager) waitUntilChannelOpen(targetChan wire.OutPoint) {
	f.barrierMtx.RLock()
	barrier, ok := f.newChanBarriers[targetChan]
	f.barrierMtx.RUnlock()
	if ok {
		fndgLog.Tracef("waiting for chan barrier signal for "+
			"ChannelPoint(%v)", targetChan)
		select {
		case <-barrier:
		case <-f.quit: // TODO(roasbeef): add timer?
			break
		}
		fndgLog.Tracef("barrier for ChannelPoint(%v) closed",
			targetChan)
	}
}

// processErrorGeneric sends a message to the fundingManager allowing it to
// process the occurred generic error.
func (f *fundingManager) processErrorGeneric(err *lnwire.ErrorGeneric,
	peerAddress *lnwire.NetAddress) {

	f.fundingMsgs <- &fundingErrorMsg{err, peerAddress}
}

// handleErrorGenericMsg process the error which was received from remote peer,
// depending on the type of error we should do different clean up steps and
// inform the user about it.
func (f *fundingManager) handleErrorGenericMsg(fmsg *fundingErrorMsg) {
	e := fmsg.err

	switch e.Code {
	case lnwire.ErrMaxPendingChannels:
		fallthrough
	case lnwire.ErrSynchronizingChain:
		peerKey := fmsg.peerAddress.IdentityKey
		chanID := fmsg.err.PendingChannelID

		ctx, err := f.cancelReservationCtx(peerKey, chanID)
		if err != nil {
			fndgLog.Warnf("unable to delete reservation: %v", err)
			ctx.err <- err
			return
		}

		fndgLog.Errorf("Received funding error from %v: %v",
			peerKey.SerializeCompressed(), newLogClosure(func() string {
				return spew.Sdump(e)
			}),
		)

		ctx.err <- grpc.Errorf(e.Code.ToGrpcCode(), e.Problem)
		return

	default:
		fndgLog.Warnf("unknown funding error (%v:%v)", e.Code, e.Problem)
	}
}

// cancelReservationCtx do all needed work in order to securely cancel the
// reservation.
func (f *fundingManager) cancelReservationCtx(peerKey *btcec.PublicKey,
	chanID uint64) (*reservationWithCtx, error) {

	fndgLog.Infof("Cancelling funding reservation for node_key=%x, "+
		"chan_id=%v", peerKey.SerializeCompressed(), chanID)

	ctx, err := f.getReservationCtx(peerKey, chanID)
	if err != nil {
		return nil, errors.Errorf("can't find reservation: %v",
			err)
	}

	if err := ctx.reservation.Cancel(); err != nil {
		return nil, errors.Errorf("can't cancel reservation: %v",
			err)
	}

	f.deleteReservationCtx(peerKey, chanID)
	return ctx, nil
}

// deleteReservationCtx is needed in order to securely delete the reservation.
func (f *fundingManager) deleteReservationCtx(peerKey *btcec.PublicKey, chanID uint64) {
	// TODO(roasbeef): possibly cancel funding barrier in peer's
	// channelManager?
	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.Lock()
	delete(f.activeReservations[peerIDKey], chanID)
	f.resMtx.Unlock()
}

// getReservationCtx returns the reservation context by peer id and channel id.
func (f *fundingManager) getReservationCtx(peerKey *btcec.PublicKey,
	chanID uint64) (*reservationWithCtx, error) {

	peerIDKey := newSerializedKey(peerKey)
	f.resMtx.RLock()
	resCtx, ok := f.activeReservations[peerIDKey][chanID]
	f.resMtx.RUnlock()

	if !ok {
		return nil, errors.Errorf("unknown channel (id: %v)", chanID)
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
