package main

import (
	"bytes"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
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
	peer        *peer

	updates chan *lnrpc.OpenStatusUpdate
	err     chan error
}

// initFundingMsg is sent by an outside subsystem to the funding manager in
// order to kick off a funding workflow with a specified target peer. The
// original request which defines the parameters of the funding workflow are
// embedded within this message giving the funding manager full context w.r.t
// the workflow.
type initFundingMsg struct {
	peer *peer
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

// fundingErrorMsg couples an lnwire.ErrorGeneric message
// with the peer who sent the message. This allows the funding
// manager to properly process the error.
type fundingErrorMsg struct {
	err  *lnwire.ErrorGeneric
	peer *peer
}

// pendingChannels is a map instantiated per-peer which tracks all active
// pending single funded channels indexed by their pending channel identifier.
type pendingChannels map[uint64]*reservationWithCtx

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

	// channelReservations is a map which houses the state of all pending
	// funding workflows.
	resMtx             sync.RWMutex
	activeReservations map[int32]pendingChannels

	// wallet is the daemon's internal Lightning enabled wallet.
	wallet *lnwallet.LightningWallet

	breachAribter *breachArbiter

	// fundingMsgs is a channel which receives wrapped wire messages
	// related to funding workflow from outside peers.
	fundingMsgs chan interface{}

	// queries is a channel which receives requests to query the internal
	// state of the funding manager.
	queries chan interface{}

	// fundingRequests is a channel used to receive channel initiation
	// requests from a local subsystem within the daemon.
	fundingRequests chan *initFundingMsg

	fakeProof *channelProof

	quit chan struct{}
	wg   sync.WaitGroup
}

// newFundingManager creates and initializes a new instance of the
// fundingManager.
func newFundingManager(w *lnwallet.LightningWallet, b *breachArbiter) *fundingManager {
	// TODO(roasbeef): remove once we actually sign the funding_locked
	// stuffs
	s := "30450221008ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa" +
		"1bf0314f882d70220299105481d63e0f4bc2a88121167221b6700d72a0e" +
		"ad154c03be696a292d24ae"
	fakeSigHex, _ := hex.DecodeString(s)
	fakeSig, _ := btcec.ParseSignature(fakeSigHex, btcec.S256())

	return &fundingManager{
		wallet:        w,
		breachAribter: b,

		fakeProof: &channelProof{
			nodeSig:    fakeSig,
			bitcoinSig: fakeSig,
		},

		activeReservations: make(map[int32]pendingChannels),
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

	fndgLog.Tracef("Funding manager running")

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
	identityPub   *btcec.PublicKey
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
			case *fundingOpenMsg:
				f.handleFundingOpen(fmsg)
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
				identityPub:   peer.addr.IdentityKey,
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
// initiate the new funding workflow with the source peer.
func (f *fundingManager) processFundingRequest(msg *lnwire.SingleFundingRequest, peer *peer) {
	f.fundingMsgs <- &fundingRequestMsg{msg, peer}
}

// handleFundingRequest creates an initial 'ChannelReservation' within
// the wallet, then responds to the source peer with a single funder response
// message progressing the funding workflow.
// TODO(roasbeef): add error chan to all, let channelManager handle
// error+propagate
func (f *fundingManager) handleFundingRequest(fmsg *fundingRequestMsg) {
	// Check number of pending channels to be smaller than maximum allowed
	// number and send ErrorGeneric to remote peer if condition is violated.
	if len(f.activeReservations[fmsg.peer.id]) >= cfg.MaxPendingChannels {
		errMsg := &lnwire.ErrorGeneric{
			ChannelPoint: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 0,
			},
			Problem:          "Number of pending channels exceed maximum",
			Code:             lnwire.ErrMaxPendingChannels,
			PendingChannelID: fmsg.msg.ChannelID,
		}
		fmsg.peer.queueMsg(errMsg, nil)
		return
	}

	// We'll also reject any requests to create channels until we're fully
	// synced to the network as we won't be able to properly validate the
	// confirmation of the funding transaction.
	isSynced, err := f.wallet.IsSynced()
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
		fmsg.peer.queueMsg(errMsg, nil)
		return
	}

	msg := fmsg.msg
	amt := msg.FundingAmount
	delay := msg.CsvDelay

	// TODO(roasbeef): error if funding flow already ongoing
	fndgLog.Infof("Recv'd fundingRequest(amt=%v, push=%v, delay=%v, pendingId=%v) "+
		"from peerID(%v)", amt, msg.PushSatoshis, delay, msg.ChannelID,
		fmsg.peer.id)

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
	reservation, err := f.wallet.InitChannelReservation(amt, 0,
		fmsg.peer.addr.IdentityKey, fmsg.peer.addr.Address, 1, delay,
		ourDustLimit, msg.PushSatoshis)
	if err != nil {
		// TODO(roasbeef): push ErrorGeneric message
		fndgLog.Errorf("Unable to initialize reservation: %v", err)
		fmsg.peer.Disconnect()
		return
	}

	reservation.SetTheirDustLimit(theirDustlimit)

	// Once the reservation has been created successfully, we add it to this
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

	// With our portion of the reservation initialized, process the
	// initiators contribution to the channel.
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(msg.DeliveryPkScript, activeNetParams.Params)
	if err != nil {
		fndgLog.Errorf("Unable to extract addresses from script: %v", err)
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
		fmsg.peer.Disconnect()
		return
	}

	fndgLog.Infof("Sending fundingResp for pendingID(%v)", msg.ChannelID)

	// With the initiator's contribution recorded, respond with our
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
		deliveryScript, ourDustLimit)

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
	peerID := fmsg.peer.id
	chanID := fmsg.msg.ChannelID
	sourcePeer := fmsg.peer

	resCtx, err := f.getReservationCtx(peerID, chanID)
	if err != nil {
		fndgLog.Warnf("Can't find reservation (peerID:%v, chanID:%v)",
			peerID, chanID)
		return
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
			sourcePeer, err)
		fmsg.peer.Disconnect()
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
		resCtx.err <- err
		return
	}

	// Register a new barrier for this channel to properly synchronize with
	// the peer's readHandler once the channel is open.
	fmsg.peer.barrierInits <- *outPoint

	fndgLog.Infof("Generated ChannelPoint(%v) for pendingID(%v)", outPoint,
		chanID)

	revocationKey := resCtx.reservation.OurContribution().RevocationKey
	obsfucator := resCtx.reservation.StateNumObfuscator()

	fundingComplete := lnwire.NewSingleFundingComplete(chanID, *outPoint,
		commitSig, revocationKey, obsfucator)
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
	resCtx, err := f.getReservationCtx(fmsg.peer.id, fmsg.msg.ChannelID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			fmsg.peer.id, fmsg.msg.ChannelID)
		return
	}

	// The channel initiator has responded with the funding outpoint of the
	// final funding transaction, as well as a signature for our version of
	// the commitment transaction. So at this point, we can validate the
	// inititator's commitment transaction, then send our own if it's valid.
	// TODO(roasbeef): make case (p vs P) consistent throughout
	fundingOut := fmsg.msg.FundingOutPoint
	chanID := fmsg.msg.ChannelID
	fndgLog.Infof("completing pendingID(%v) with ChannelPoint(%v)",
		chanID, fundingOut,
	)

	revokeKey := copyPubKey(fmsg.msg.RevocationKey)
	obsfucator := fmsg.msg.StateHintObsfucator
	commitSig := fmsg.msg.CommitSignature.Serialize()

	// With all the necessary data available, attempt to advance the
	// funding workflow to the next stage. If this succeeds then the
	// funding transaction will broadcast after our next message.
	err = resCtx.reservation.CompleteReservationSingle(revokeKey, &fundingOut,
		commitSig, obsfucator)
	if err != nil {
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
	fmsg.peer.barrierInits <- fundingOut

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
func newChanAnnouncement(localIdentity *btcec.PublicKey,
	channel *lnwallet.LightningChannel, chanID lnwire.ChannelID,
	localProof, remoteProof *channelProof) *chanAnnouncement {

	// First obtain the remote party's identity public key, this will be
	// used to determine the order of the keys and signatures in the
	// channel announcement.
	chanInfo := channel.StateSnapshot()
	remotePub := chanInfo.RemoteIdentity
	localPub := localIdentity

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
		chanAnn.FirstNodeID = localPub
		chanAnn.SecondNodeID = &remotePub
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
		chanAnn.FirstNodeID = &remotePub
		chanAnn.SecondNodeID = localPub
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

// handleFundingSignComplete processes the final message received in a single
// funder workflow. Once this message is processed, the funding transaction is
// broadcast. Once the funding transaction reaches a sufficient number of
// confirmations, a message is sent to the responding peer along with a compact
// encoding of the location of the channel within the blockchain.
func (f *fundingManager) handleFundingSignComplete(fmsg *fundingSignCompleteMsg) {
	chanID := fmsg.msg.ChannelID
	peerID := fmsg.peer.id

	resCtx, err := f.getReservationCtx(peerID, chanID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			peerID, chanID)
		return
	}

	// The remote peer has responded with a signature for our commitment
	// transaction. We'll verify the signature for validity, then commit
	// the state to disk as we can now open the channel.
	commitSig := fmsg.msg.CommitSignature.Serialize()
	if err := resCtx.reservation.CompleteReservation(nil, commitSig); err != nil {
		fndgLog.Errorf("unable to complete reservation sign complete: %v", err)
		fmsg.peer.Disconnect()
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
				Txid: fundingPoint.Hash[:],
			},
		},
	}

	// Spawn a goroutine which will send the newly open channel to the
	// source peer once the channel is open. A channel is considered "open"
	// once it reaches a sufficient number of confirmations.
	// TODO(roasbeef): semaphore to limit active chan open goroutines
	go func() {
		// TODO(roasbeef): need to persist pending broadcast channels,
		// send chan open proof during scan of blocks mined while down.
		openChanDetails, err := resCtx.reservation.DispatchChan()
		if err != nil {
			fndgLog.Errorf("Unable to dispatch "+
				"ChannelPoint(%v): %v", fundingPoint, err)
			return
		}

		// This reservation is no longer pending as the funding
		// transaction has been fully confirmed.
		f.deleteReservationCtx(peerID, chanID)

		fndgLog.Infof("ChannelPoint(%v) with peerID(%v) is now active",
			fundingPoint, peerID)

		// Now that the channel is open, we need to notify a number of
		// parties of this event.

		// First we send the newly opened channel to the source server
		// peer.
		fmsg.peer.newChannels <- openChanDetails.Channel

		// Afterwards we send the breach arbiter the new channel so it
		// can watch for attempts to breach the channel's contract by
		// the remote party.
		f.breachAribter.newContracts <- openChanDetails.Channel

		// With the block height and the transaction index known, we
		// can construct the compact chainID which is used on the
		// network to unique identify channels.
		chainID := lnwire.ChannelID{
			BlockHeight: openChanDetails.ConfirmationHeight,
			TxIndex:     openChanDetails.TransactionIndex,
			TxPosition:  uint16(fundingPoint.Index),
		}

		// Next, we queue a message to notify the remote peer that the
		// channel is open. We additionally provide the compact
		// channelID so they can advertise the channel.
		fundingOpen := lnwire.NewSingleFundingOpenProof(chanID, chainID)
		fmsg.peer.queueMsg(fundingOpen, nil)

		// Register the new link with the L3 routing manager so this
		// new channel can be utilized during path
		// finding.
		// TODO(roasbeef): should include sigs from funding
		// locked
		//  * should be moved to after funding locked is recv'd
		f.announceChannel(fmsg.peer.server, openChanDetails.Channel,
			chainID, f.fakeProof, f.fakeProof)

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
		return
	}()
}

// announceChannel announces a newly created channel to the rest of the network
// by crafting the two authenticated announcements required for the peers on the
// network to recognize the legitimacy of the channel. The crafted
// announcements are then send to the channel router to handle broadcasting to
// the network during its next trickle.
func (f *fundingManager) announceChannel(s *server,
	channel *lnwallet.LightningChannel, chanID lnwire.ChannelID,
	localProof, remoteProof *channelProof) {

	// TODO(roasbeef): need a Signer.SignMessage method to finalize
	// advertisements
	localIdentity := s.identityPriv.PubKey()
	chanAnnouncement := newChanAnnouncement(localIdentity, channel,
		chanID, localProof, remoteProof)

	s.chanRouter.ProcessRoutingMessage(chanAnnouncement.chanAnn, localIdentity)
	s.chanRouter.ProcessRoutingMessage(chanAnnouncement.edgeUpdate, localIdentity)
}

// processFundingOpenProof sends a message to the fundingManager allowing it
// to process the final message received when the daemon is on the responding
// side of a single funder channel workflow.
func (f *fundingManager) processFundingOpenProof(msg *lnwire.SingleFundingOpenProof, peer *peer) {
	f.fundingMsgs <- &fundingOpenMsg{msg, peer}
}

// handleFundingOpen processes the final message when the daemon is the
// responder to a single funder channel workflow.
func (f *fundingManager) handleFundingOpen(fmsg *fundingOpenMsg) {
	chanID := fmsg.msg.ChannelID
	peerID := fmsg.peer.id

	resCtx, err := f.getReservationCtx(peerID, chanID)
	if err != nil {
		fndgLog.Warnf("can't find reservation (peerID:%v, chanID:%v)",
			peerID, chanID)
		return
	}

	// The channel initiator has claimed the channel is now open, so we'll
	// verify the contained SPV proof for validity.
	// TODO(roasbeef): send off to the spv proof verifier, in the routing
	// submodule.

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
	f.deleteReservationCtx(peerID, chanID)

	fndgLog.Infof("FundingOpen: ChannelPoint(%v) with peerID(%v) is now open",
		resCtx.reservation.FundingOutpoint(), peerID)

	// Notify the L3 routing manager of the newly active channel link.
	// TODO(roasbeef): should have sigs, only after funding_locked is
	// recv'd
	//  * also ensure fault tolerance, scan opened chan on start up check
	//  for graph existence
	f.announceChannel(fmsg.peer.server, openChan, fmsg.msg.ChanChainID,
		f.fakeProof, f.fakeProof)

	// Send the newly opened channel to the breach arbiter to it can watch
	// for uncooperative channel breaches, potentially punishing the
	// counterparty for attempting to cheat us.
	f.breachAribter.newContracts <- openChan

	// Finally, notify the target peer of the newly opened channel.
	fmsg.peer.newChannels <- openChan
}

// initFundingWorkflow sends a message to the funding manager instructing it
// to initiate a single funder workflow with the source peer.
// TODO(roasbeef): re-visit blocking nature..
func (f *fundingManager) initFundingWorkflow(targetPeer *peer, req *openChanReq) {
	f.fundingRequests <- &initFundingMsg{
		peer:        targetPeer,
		openChanReq: req,
	}
}

// handleInitFundingMsg creates a channel reservation within the daemon's
// wallet, then sends a funding request to the remote peer kicking off the
// funding workflow.
func (f *fundingManager) handleInitFundingMsg(msg *initFundingMsg) {
	var (
		// TODO(roasbeef): add delay
		nodeID       = msg.peer.addr.IdentityKey
		localAmt     = msg.localFundingAmt
		remoteAmt    = msg.remoteFundingAmt
		capacity     = localAmt + remoteAmt
		numConfs     = msg.numConfs
		ourDustLimit = lnwallet.DefaultDustLimit()
	)

	fndgLog.Infof("Initiating fundingRequest(localAmt=%v, remoteAmt=%v, "+
		"capacity=%v, numConfs=%v, addr=%v, dustLimit=%v)", localAmt,
		msg.pushAmt, capacity, numConfs, msg.peer.addr.Address,
		ourDustLimit)

	// Initialize a funding reservation with the local wallet. If the
	// wallet doesn't have enough funds to commit to this channel, then
	// the request will fail, and be aborted.
	reservation, err := f.wallet.InitChannelReservation(capacity, localAmt,
		nodeID, msg.peer.addr.Address, uint16(numConfs), 4,
		ourDustLimit, msg.pushAmt)
	if err != nil {
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
	)
	msg.peer.queueMsg(fundingReq, nil)
}

// processErrorGeneric sends a message to the fundingManager allowing it to
// process the occurred generic error.
func (f *fundingManager) processErrorGeneric(err *lnwire.ErrorGeneric,
	peer *peer) {

	f.fundingMsgs <- &fundingErrorMsg{err, peer}
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
		peerID := fmsg.peer.id
		chanID := fmsg.err.PendingChannelID

		resCtx, err := f.cancelReservationCtx(peerID, chanID)
		if err != nil {
			fndgLog.Warnf("unable to delete reservation: %v", err)
			return
		}

		fndgLog.Errorf("Received funding error from %v: %v", fmsg.peer,
			newLogClosure(func() string {
				return spew.Sdump(e)
			}),
		)

		resCtx.err <- grpc.Errorf(e.Code.ToGrpcCode(), e.Problem)
		return

	default:
		fndgLog.Warnf("unknown funding error (%v:%v)", e.Code, e.Problem)
	}
}

// cancelReservationCtx do all needed work in order to securely cancel the
// reservation.
func (f *fundingManager) cancelReservationCtx(peerID int32,
	chanID uint64) (*reservationWithCtx, error) {

	ctx, err := f.getReservationCtx(peerID, chanID)
	if err != nil {
		return nil, errors.Errorf("can't find reservation: %v",
			err)
	}

	if err := ctx.reservation.Cancel(); err != nil {
		ctx.err <- err
		return nil, errors.Errorf("can't cancel reservation: %v",
			err)
	}

	f.deleteReservationCtx(peerID, chanID)
	return ctx, nil
}

// deleteReservationCtx is needed in order to securely delete the reservation.
func (f *fundingManager) deleteReservationCtx(peerID int32, chanID uint64) {
	// TODO(roasbeef): possibly cancel funding barrier in peer's
	// channelManager?
	f.resMtx.Lock()
	delete(f.activeReservations[peerID], chanID)
	f.resMtx.Unlock()
}

// getReservationCtx returns the reservation context by peer id and channel id.
func (f *fundingManager) getReservationCtx(peerID int32,
	chanID uint64) (*reservationWithCtx, error) {

	f.resMtx.RLock()
	resCtx, ok := f.activeReservations[peerID][chanID]
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
