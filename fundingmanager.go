package main

import (
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/BitfuryLightning/tools/rt"
	"github.com/BitfuryLightning/tools/rt/graph"
)

const (
	// TODO(roasbeef): tune
	msgBufferSize = 50
)

// reservationWithCtx encapsulates a pending channel reservation. This wrapper
// struct is used internally within the funding manager to track and progress
// the funding workflow initiated by incoming/outgoing meethods from the target
// peer. Additionally, this struct houses a response and error channel which is
// used to respond to the caller in the case a channel workflow is initiated
// via a local signal such as RPC.
// TODO(roasbeef): actually use the context package
//  * deadlines, etc.
type reservationWithCtx struct {
	reservation *lnwallet.ChannelReservation
	peer        *peer

	resp chan *wire.OutPoint
	err  chan error
}

// initFundingMsg is sent by an outside sub-system to the funding manager in
// order to kick-off a funding workflow with a specified target peer. The
// original request which defines the parameters of the funding workflow are
// embedded within this message giving the funding manager full context w.r.t
// the workflow.
type initFundingMsg struct {
	peer *peer
	err  chan error
	resp chan *wire.OutPoint

	*openChanReq
}

// fundingRequestMsg couples an lnwire.SingleFundingRequest message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingRequestMsg struct {
	msg  *lnwire.SingleFundingRequest
	peer *peer
}

// fundingResponseMsg couples an lnwire.SingleFundingResponse message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingResponseMsg struct {
	msg  *lnwire.SingleFundingResponse
	peer *peer
}

// fundingCompleteMsg couples an lnwire.SingleFundingComplete message with the
// peer who sent the message. This allows the funding manager to queue a
// response directly to the peer, progressing the funding workflow.
type fundingCompleteMsg struct {
	msg  *lnwire.SingleFundingComplete
	peer *peer
}

// fundingSignCompleteMsg couples an lnwire.SingleFundingSignComplete message
// with the peer who sent the message. This allows the funding manager to
// queue a response directly to the peer, progressing the funding workflow.
type fundingSignCompleteMsg struct {
	msg  *lnwire.SingleFundingSignComplete
	peer *peer
}

// fundingOpenMsg couples an lnwire.SingleFundingOpenProof message
// with the peer who sent the message. This allows the funding manager to
// queue a response directly to the peer, progressing the funding workflow.
type fundingOpenMsg struct {
	msg  *lnwire.SingleFundingOpenProof
	peer *peer
}

// pendingChannels is a map instantiated per-peer which tracks all active
// pending single funded channels indexed by their pending channel identifier.
type pendingChannels map[uint64]*reservationWithCtx

// fundingManager acts as an orchestrator/bridge between the wallet's
// 'ChannelReservation' workflow, and the wire protocl's funding initiation
// messages. Any requests to initaite the funding workflow for a channel, either
// kicked-off locally, or remotely is handled by the funding manager. Once a
// channels's funding workflow has been completed, any local callers, the local
// peer, and possibly the remote peer are notified of the completion of the
// channel workflow. Additionally, any temporary or permanent access controls
// between the wallet and remote peers are enforced via the funding manager.
type fundingManager struct {
	// MUST be used atomically.
	started int32
	stopped int32

	// channelReservations is a map which houses the state of all pending
	// funding workflows.
	resMtx             sync.RWMutex
	activeReservations map[int32]pendingChannels

	// wallet is the daemon's internal Lightning enabled wallet.
	wallet *lnwallet.LightningWallet

	// fundingMsgs is a channel which receives wrapped wire messages
	// related to funding workflow from outside peers.
	fundingMsgs chan interface{}

	// queries is a channel which receives requests to query the internal
	// state of the funding manager.
	queries chan interface{}

	// fundingRequests is a channel used to recieve channel initiation
	// requests from a local sub-system within the daemon.
	fundingRequests chan *initFundingMsg

	quit chan struct{}
	wg   sync.WaitGroup
}

// newFundingManager creates and initializes a new instance of the
// fundingManager.
func newFundingManager(w *lnwallet.LightningWallet) *fundingManager {
	return &fundingManager{
		activeReservations: make(map[int32]pendingChannels),
		wallet:             w,
		fundingMsgs:        make(chan interface{}, msgBufferSize),
		fundingRequests:    make(chan *initFundingMsg, msgBufferSize),
		queries:            make(chan interface{}, 1),
		quit:               make(chan struct{}),
	}
}

// Start launches all helper goroutines required for handling requests sent
// to the funding manager.
func (f *fundingManager) Start() error {
	if atomic.AddInt32(&f.started, 1) != 1 { // TODO(roasbeef): CAS instead
		return nil
	}

	fndgLog.Infof("funding manager running")

	f.wg.Add(1) // TODO(roasbeef): tune
	go f.reservationCoordinator()

	return nil
}

// Start signals all helper goroutines to execute a graceful shutdown. This
// method will block until all goroutines have exited.
func (f *fundingManager) Stop() error {
	if atomic.AddInt32(&f.stopped, 1) != 1 {
		return nil
	}

	fndgLog.Infof("funding manager shutting down")

	close(f.quit)

	f.wg.Wait()

	return nil
}

type numPendingReq struct {
	resp chan uint32
}

// NumPendingChannels returns the number of pending channels currently
// progressing through the reservation workflow.
func (f *fundingManager) NumPendingChannels() uint32 {
	resp := make(chan uint32, 1)

	req := &numPendingReq{resp}
	f.queries <- req

	return <-resp
}

type pendingChannel struct {
	peerId        int32
	lightningID   [32]byte
	channelPoint  *wire.OutPoint
	capacity      btcutil.Amount
	localBalance  btcutil.Amount
	remoteBalance btcutil.Amount
}

type pendingChansReq struct {
	resp chan []*pendingChannel
}

// PendingChannels returns a slice describing all the channels which are
// currently pending at the last state of the funding workflow.
func (f *fundingManager) PendingChannels() []*pendingChannel {
	resp := make(chan []*pendingChannel, 1)

	req := &pendingChansReq{resp}
	f.queries <- req

	return <-resp
}

// reservationCoordinator is the primary goroutine tasked with progressing the
// funding workflow between the wallet, and any outside peers or local callers.
//
// NOTE: This MUST be run as a goroutine.
func (f *fundingManager) reservationCoordinator() {
out:
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
			case *fundingOpenMsg:
				f.handleFundingOpen(fmsg)
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
			break out
		}
	}

	f.wg.Done()
}

// handleNumPending handles a request for the total number of pending channels.
func (f *fundingManager) handleNumPending(msg *numPendingReq) {
	var numPending uint32
	for _, peerChannels := range f.activeReservations {
		numPending += uint32(len(peerChannels))
	}
	msg.resp <- numPending
}

// handlePendingChannels responds to a request for details concerning all
// currently pending channels waiting for the final phase of the funding
// workflow (funding txn confirmation).
func (f *fundingManager) handlePendingChannels(msg *pendingChansReq) {
	var pendingChannels []*pendingChannel
	for peerID, peerChannels := range f.activeReservations {
		for _, pendingChan := range peerChannels {
			peer := pendingChan.peer
			res := pendingChan.reservation
			localFund := res.OurContribution().FundingAmount
			remoteFund := res.TheirContribution().FundingAmount

			pendingChan := &pendingChannel{
				peerId:        peerID,
				lightningID:   peer.lightningID,
				channelPoint:  res.FundingOutpoint(),
				capacity:      localFund + remoteFund,
				localBalance:  localFund,
				remoteBalance: remoteFund,
			}
			pendingChannels = append(pendingChannels, pendingChan)
		}
	}
	msg.resp <- pendingChannels
}

// processFundingRequest sends a message to the fundingManager allowing it to
// intiate the new funding workflow with the source peer.
func (f *fundingManager) processFundingRequest(msg *lnwire.SingleFundingRequest, peer *peer) {
	f.fundingMsgs <- &fundingRequestMsg{msg, peer}
}

// handleSingleFundingRequest creates an initial 'ChannelReservation' within
// the wallet, then responds to the source peer with a single funder response
// message progressing the funding workflow.
// TODO(roasbeef): add error chan to all, let channelManager handle
// error+propagate
func (f *fundingManager) handleFundingRequest(fmsg *fundingRequestMsg) {
	msg := fmsg.msg
	amt := msg.FundingAmount
	delay := msg.CsvDelay

	// TODO(roasbeef): error if funding flow already ongoing
	fndgLog.Infof("Recv'd fundingRequest(amt=%v, delay=%v, pendingId=%v) "+
		"from peerID(%v)", amt, delay, msg.ChannelID, fmsg.peer.id)

	// Attempt to initialize a reservation within the wallet. If the wallet
	// has insufficient resources to create the channel, then the reservation
	// attempt may be rejected. Note that since we're on the responding
	// side of a single funder workflow, we don't commit any funds to the
	// channel ourselves.
	// TODO(roasbeef): passing num confs 1 is irrelevant here, make signed?
	reservation, err := f.wallet.InitChannelReservation(amt, 0, fmsg.peer.lightningID, 1, delay)
	if err != nil {
		// TODO(roasbeef): push ErrorGeneric message
		fndgLog.Errorf("Unable to initialize reservation: %v", err)
		fmsg.peer.Disconnect()
		return
	}

	// Once the reservation has been created succesfully, we add it to this
	// peers map of pending reservations to track this particular reservation
	// until either abort or completion.
	f.resMtx.Lock()
	if _, ok := f.activeReservations[fmsg.peer.id]; !ok {
		f.activeReservations[fmsg.peer.id] = make(pendingChannels)
	}
	f.activeReservations[fmsg.peer.id][msg.ChannelID] = &reservationWithCtx{
		reservation: reservation,
		peer:        fmsg.peer,
	}
	f.resMtx.Unlock()

	// With our portion of the reservation initialied, process the
	// initiators contribution to the channel.
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(msg.DeliveryPkScript, activeNetParams.Params)
	if err != nil {
		fndgLog.Errorf("Unable to extract addresses from script: %v", err)
		return
	}
	contribution := &lnwallet.ChannelContribution{
		FundingAmount:   amt,
		MultiSigKey:     msg.ChannelDerivationPoint,
		CommitKey:       msg.CommitmentKey,
		DeliveryAddress: addrs[0],
		CsvDelay:        delay,
	}
	if err := reservation.ProcessSingleContribution(contribution); err != nil {
		fndgLog.Errorf("unable to add contribution reservation: %v", err)
		fmsg.peer.Disconnect()
		return
	}

	fndgLog.Infof("Sending fundingResp for pendingID(%v)", msg.ChannelID)

	// With the initiator's contribution recorded, response with our
	// contribution in the next message of the workflow.
	ourContribution := reservation.OurContribution()
	deliveryScript, err := txscript.PayToAddrScript(ourContribution.DeliveryAddress)
	if err != nil {
		fndgLog.Errorf("unable to convert address to pkscript: %v", err)
		return
	}
	fundingResp := lnwire.NewSingleFundingResponse(msg.ChannelID,
		ourContribution.RevocationKey, ourContribution.CommitKey,
		ourContribution.MultiSigKey, ourContribution.CsvDelay,
		deliveryScript)

	fmsg.peer.queueMsg(fundingResp, nil)
}

// processFundingRequest sends a message to the fundingManager allowing it to
// continue the second phase of a funding workflow with the target peer.
func (f *fundingManager) processFundingResponse(msg *lnwire.SingleFundingResponse, peer *peer) {
	f.fundingMsgs <- &fundingResponseMsg{msg, peer}
}

// handleFundingResponse processes a response to the workflow initiation sent
// by the remote peer. This message then queues a message with the funding
// outpoint, and a commitment signature to the remote peer.
func (f *fundingManager) handleFundingResponse(fmsg *fundingResponseMsg) {
	msg := fmsg.msg
	sourcePeer := fmsg.peer

	f.resMtx.RLock()
	resCtx := f.activeReservations[fmsg.peer.id][msg.ChannelID]
	f.resMtx.RUnlock()

	fndgLog.Infof("Recv'd fundingResponse for pendingID(%v)", msg.ChannelID)

	// The remote node has responded with their portion of the channel
	// contribution. At this point, we can process their contribution which
	// allows us to construct and sign both the commitment transaction, and
	// the funding transaction.
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(msg.DeliveryPkScript, activeNetParams.Params)
	if err != nil {
		fndgLog.Errorf("Unable to extract addresses from script: %v", err)
		return
	}
	contribution := &lnwallet.ChannelContribution{
		FundingAmount:   0,
		MultiSigKey:     msg.ChannelDerivationPoint,
		CommitKey:       msg.CommitmentKey,
		DeliveryAddress: addrs[0],
		RevocationKey:   msg.RevocationKey,
		CsvDelay:        msg.CsvDelay,
	}
	if err := resCtx.reservation.ProcessContribution(contribution); err != nil {
		fndgLog.Errorf("Unable to process contribution from %v: %v",
			sourcePeer, err)
		fmsg.peer.Disconnect()
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
		return
	}

	// Register a new barrier for this channel to properly synchronize with
	// the peer's readHandler once the channel is open.
	fmsg.peer.barrierInits <- *outPoint

	fndgLog.Infof("Generated ChannelPoint(%v) for pendingID(%v)",
		outPoint, msg.ChannelID)

	revocationKey := resCtx.reservation.OurContribution().RevocationKey
	fundingComplete := lnwire.NewSingleFundingComplete(msg.ChannelID,
		outPoint, commitSig, revocationKey)
	sourcePeer.queueMsg(fundingComplete, nil)
}

// processFundingComplete queues a funding complete message coupled with the
// source peer to the fundingManager.
func (f *fundingManager) processFundingComplete(msg *lnwire.SingleFundingComplete, peer *peer) {
	f.fundingMsgs <- &fundingCompleteMsg{msg, peer}
}

// handleFundingComplete progresses the funding workflow when the daemon is on
// the responding side of a single funder workflow. Once this message has been
// processed, a signature is sent to the remote peer allowing it to broadcast
// the funding transaction, progressing the workflow into the final stage.
func (f *fundingManager) handleFundingComplete(fmsg *fundingCompleteMsg) {
	f.resMtx.RLock()
	resCtx := f.activeReservations[fmsg.peer.id][fmsg.msg.ChannelID]
	f.resMtx.RUnlock()

	// The channel initiator has responded with the funding outpoint of the
	// final funding transaction, as well as a signature for our version of
	// the commitment transaction. So at this point, we can validate the
	// inititator's commitment transaction, then send our own if it's valid.
	// TODO(roasbeef): make case (p vs P) consistent throughout
	fundingOut := fmsg.msg.FundingOutPoint
	chanID := fmsg.msg.ChannelID
	commitSig := fmsg.msg.CommitSignature.Serialize()
	fndgLog.Infof("completing pendingID(%v) with ChannelPoint(%v)",
		fmsg.msg.ChannelID, fundingOut,
	)

	// Append a sighash type of SigHashAll to the signature as it's the
	// sighash type used implicitly within this type of channel for
	// commitment transactions.
	commitSig = append(commitSig, byte(txscript.SigHashAll))
	revokeKey := fmsg.msg.RevocationKey
	if err := resCtx.reservation.CompleteReservationSingle(revokeKey, fundingOut, commitSig); err != nil {
		// TODO(roasbeef): better error logging: peerID, channelID, etc.
		fndgLog.Errorf("unable to complete single reservation: %v", err)
		fmsg.peer.Disconnect()
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
		return
	}

	// Register a new barrier for this channel to properly synchronize with
	// the peer's readHandler once the channel is open.
	fmsg.peer.barrierInits <- *fundingOut

	fndgLog.Infof("sending signComplete for pendingID(%v) over ChannelPoint(%v)",
		fmsg.msg.ChannelID, fundingOut)

	signComplete := lnwire.NewSingleFundingSignComplete(chanID, ourCommitSig)
	fmsg.peer.queueMsg(signComplete, nil)
}

// processFundingSignComplete sends a single funding sign complete message
// along with the source peer to the funding manager.
func (f *fundingManager) processFundingSignComplete(msg *lnwire.SingleFundingSignComplete, peer *peer) {
	f.fundingMsgs <- &fundingSignCompleteMsg{msg, peer}
}

// handleFundingSignComplete processes the final message recieved in a single
// funder workflow. Once this message is processed, the funding transaction is
// broadcast. Once the funding transaction reaches a sufficient number of
// confirmations, a message is sent to the responding peer along with an SPV
// proofs of transaction inclusion.
func (f *fundingManager) handleFundingSignComplete(fmsg *fundingSignCompleteMsg) {
	chanID := fmsg.msg.ChannelID

	f.resMtx.RLock()
	resCtx := f.activeReservations[fmsg.peer.id][chanID]
	f.resMtx.RUnlock()

	// The remote peer has responded with a signature for our commitment
	// transaction. We'll verify the signature for validity, then commit
	// the state to disk as we can now open the channel.
	commitSig := append(fmsg.msg.CommitSignature.Serialize(), byte(txscript.SigHashAll))
	if err := resCtx.reservation.CompleteReservation(nil, commitSig); err != nil {
		fndgLog.Errorf("unable to complete reservation sign complete: %v", err)
		fmsg.peer.Disconnect()
		return
	}

	fundingPoint := resCtx.reservation.FundingOutpoint()
	fndgLog.Infof("Finalizing pendingID(%v) over ChannelPoint(%v), "+
		"waiting for channel open on-chain", chanID, fundingPoint)

	// Spawn a goroutine which will send the newly open channel to the
	// source peer once the channel is open. A channel is considered "open"
	// once it reaches a sufficient number of confirmations.
	go func() {
		// TODO(roasbeef): semaphore to limit active chan open goroutines
		select {
		// TODO(roasbeef): need to persist pending broadcast channels,
		// send chan open proof during scan of blocks mined while down.
		case openChan := <-resCtx.reservation.DispatchChan():

			// This reservation is no longer pending as the funding
			// transaction has been fully confirmed.
			f.resMtx.Lock()
			delete(f.activeReservations[fmsg.peer.id], chanID)
			f.resMtx.Unlock()

			fndgLog.Infof("ChannelPoint(%v) with peerID(%v) is now active",
				fundingPoint, fmsg.peer.id)

			// Now that the channel is open, we need to notifiy a
			// number of parties of this event.

			// First we send the newly opened channel to the source
			// server peer.
			fmsg.peer.newChannels <- openChan

			// Next, we queue a message to notify the remote peer
			// that the channel is open. We additionally provide an
			// SPV proof allowing them to verify the transaction
			// inclusion.
			// TODO(roasbeef): obtain SPV proof from sub-system.
			//  * ChainNotifier constructs proof also?
			spvProof := []byte("fake proof")
			fundingOpen := lnwire.NewSingleFundingOpenProof(chanID, spvProof)
			fmsg.peer.queueMsg(fundingOpen, nil)

			// Finally, respond to the original caller (if any).
			resCtx.err <- nil
			resCtx.resp <- resCtx.reservation.FundingOutpoint()
			return
		case <-f.quit:
			return
		}
	}()
}

// processFundingOpenProof sends a message to the fundingManager allowing it
// to process the final message recieved when the daemon is on the responding
// side of a single funder channel workflow.
func (f *fundingManager) processFundingOpenProof(msg *lnwire.SingleFundingOpenProof, peer *peer) {
	f.fundingMsgs <- &fundingOpenMsg{msg, peer}
}

// handleFundingOpen processes the final message when the daemon is the
// responder to a single funder channel workflow. The SPV proofs supplied by
// the initiating node is verified, which if correct, marks the channel as open
// to the source peer.
func (f *fundingManager) handleFundingOpen(fmsg *fundingOpenMsg) {
	f.resMtx.RLock()
	resCtx := f.activeReservations[fmsg.peer.id][fmsg.msg.ChannelID]
	f.resMtx.RUnlock()

	// The channel initiator has claimed the channel is now open, so we'll
	// verify the contained SPV proof for validity.
	// TODO(roasbeef): send off to the spv proof verifier, in the routing
	// sub-module.

	// Now that we've verified the initiator's proof, we'll commit the
	// channel state to disk, and notify the source peer of a newly opened
	// channel.
	openChan, err := resCtx.reservation.FinalizeReservation()
	if err != nil {
		fndgLog.Errorf("unable to finalize reservation: %v", err)
		fmsg.peer.Disconnect()
		return
	}

	// The reservation has been completed, therefore we can stop tracking
	// it within our active reservations map.
	f.resMtx.Lock()
	delete(f.activeReservations[fmsg.peer.id], fmsg.msg.ChannelID)
	f.resMtx.Unlock()

	fndgLog.Infof("FundingOpen: ChannelPoint(%v) with peerID(%v) is now open",
		resCtx.reservation.FundingOutpoint, fmsg.peer.id)

	capacity := float64(resCtx.reservation.OurContribution().FundingAmount +
		resCtx.reservation.TheirContribution().FundingAmount)
	fmsg.peer.server.routingMgr.AddChannel(
		graph.NewID(fmsg.peer.server.lightningID),
		graph.NewID([32]byte(fmsg.peer.lightningID)),
		graph.NewEdgeID(resCtx.reservation.FundingOutpoint().String()),
		&rt.ChannelInfo{
			Cpt: capacity,
		},
	)
	fmsg.peer.newChannels <- openChan
}

// initFundingWorkflow sends a message to the funding manager instructing it
// to initiate a single funder workflow with the source peer.
// TODO(roasbeef): re-visit blocking nature..
func (f *fundingManager) initFundingWorkflow(targetPeer *peer, req *openChanReq) (*wire.OutPoint, error) {
	errChan := make(chan error, 1)
	respChan := make(chan *wire.OutPoint, 1)
	f.fundingRequests <- &initFundingMsg{
		peer:        targetPeer,
		resp:        respChan,
		err:         errChan,
		openChanReq: req,
	}

	return <-respChan, <-errChan
}

// handleInitFundingMsg creates a channel reservation within the daemon's
// wallet, then sends a funding request to the remote peer kicking off the
// funding workflow.
func (f *fundingManager) handleInitFundingMsg(msg *initFundingMsg) {
	nodeID := msg.peer.lightningID

	localAmt := msg.localFundingAmt
	remoteAmt := msg.remoteFundingAmt
	capacity := localAmt + remoteAmt
	numConfs := msg.numConfs
	// TODO(roasbeef): add delay

	fndgLog.Infof("Initiating fundingRequest(localAmt=%v, remoteAmt=%v, "+
		"capacity=%v, numConfs=%v)", localAmt, remoteAmt, capacity, numConfs)

	// Initialize a funding reservation with the local wallet. If the
	// wallet doesn't have enough funds to commit to this channel, then
	// the request will fail, and be aborted.
	reservation, err := f.wallet.InitChannelReservation(capacity, localAmt,
		nodeID, uint16(numConfs), 4)
	if err != nil {
		msg.resp <- nil
		msg.err <- err
		return
	}

	// Obtain a new pending channel ID which is used to track this
	// reservation throughout its lifetime.
	msg.peer.pendingChannelMtx.Lock()
	chanID := msg.peer.nextPendingChannelID
	msg.peer.nextPendingChannelID++
	msg.peer.pendingChannelMtx.Unlock()

	// If a pending channel map for this peer isn't already created, then
	// we create one, ultimately allowing us to track this pending
	// reservation within the target peer.
	f.resMtx.Lock()
	if _, ok := f.activeReservations[msg.peer.id]; !ok {
		f.activeReservations[msg.peer.id] = make(pendingChannels)
	}
	f.activeReservations[msg.peer.id][chanID] = &reservationWithCtx{
		reservation: reservation,
		peer:        msg.peer,
		err:         msg.err,
		resp:        msg.resp,
	}
	f.resMtx.Unlock()

	// Once the reservation has been created, and indexed, queue a funding
	// request to the remote peer, kicking off the funding workflow.
	contribution := reservation.OurContribution()
	deliveryScript, err := txscript.PayToAddrScript(contribution.DeliveryAddress)
	if err != nil {
		fndgLog.Errorf("Unable to convert address to pkscript: %v", err)
		msg.resp <- nil
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
		contribution.FundingAmount,
		contribution.CsvDelay,
		contribution.CommitKey,
		contribution.MultiSigKey,
		deliveryScript,
	)
	msg.peer.queueMsg(fundingReq, nil)
}
