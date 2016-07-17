package main

import (
	"container/list"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

var (
	numNodes int32
)

const (
	// pingInterval is the interval at which ping messages are sent.
	pingInterval = 30 * time.Second

	// outgoingQueueLen is the buffer size of the channel which houses
	// messages to be sent across the wire, requested by objects outside
	// this struct.
	outgoingQueueLen = 50
)

// outgoinMsg packages an lnwire.Message to be sent out on the wire, along with
// a buffered channel which will be sent upon once the write is complete. This
// buffered channel acts as a semaphore to be used for synchronization purposes.
type outgoinMsg struct {
	msg      lnwire.Message
	sentChan chan struct{} // MUST be buffered.
}

// chanSnapshotReq is a message sent by outside sub-systems to a peer in order
// to gain a snapshot of the peer's currently active channels.
type chanSnapshotReq struct {
	resp chan []*channeldb.ChannelSnapshot
}

// peer is an active peer on the Lightning Network. This struct is responsible
// for managing any channel state related to this peer. To do so, it has several
// helper goroutines to handle events such as HTLC timeouts, new funding
// workflow, and detecting an uncooperative closure of any active channels.
type peer struct {
	// MUST be used atomically.
	started    int32
	connected  int32
	disconnect int32

	conn net.Conn

	lightningAddr *lndc.LNAdr
	lightningID   wire.ShaHash

	inbound bool
	id      int32

	// For purposes of detecting retransmits, etc.
	lastNMessages map[lnwire.Message]struct{}

	// This mutex protects all the stats below it.
	sync.RWMutex
	timeConnected time.Time
	lastSend      time.Time
	lastRecv      time.Time

	// The following fields are only meant to be used *atomically*
	bytesReceived    uint64
	bytesSent        uint64
	satoshisSent     uint64
	satoshisReceived uint64

	// chainNet is the Bitcoin network to which this peer is anchored to.
	chainNet wire.BitcoinNet

	// sendQueue is the channel which is used to queue outgoing to be
	// written onto the wire. Note that this channel is unbuffered.
	sendQueue chan outgoinMsg

	// outgoingQueue is a buffered channel which allows second/third party
	// objects to queue messages to be sent out on the wire.
	outgoingQueue chan outgoinMsg

	// sendQueueSync is used as a semaphore to synchronize writes between
	// the writeHandler and the queueHandler.
	sendQueueSync chan struct{}

	// activeChannels is a map which stores the state machines of all
	// active channels. Channels are indexed into the map by the txid of
	// the funding transaction which opened the channel.
	activeChannels   map[wire.OutPoint]*lnwallet.LightningChannel
	chanSnapshotReqs chan *chanSnapshotReq

	htlcManagers map[wire.OutPoint]chan lnwire.Message

	// newChanBarriers is a map from a channel point to a 'barrier' which
	// will be signalled once the channel is fully open. This barrier acts
	// as a synchronization point for any incoming/outgoing HTLCs before
	// the channel has been fully opened.
	barrierMtx      sync.RWMutex
	newChanBarriers map[wire.OutPoint]chan struct{}
	barrierInits    chan wire.OutPoint

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newChannels chan *lnwallet.LightningChannel

	// localCloseChanReqs is a channel in which any local requests to
	// close a particular channel are sent over.
	localCloseChanReqs chan *closeLinkReq

	// remoteCloseChanReqs is a channel in which any remote requests
	// (initiated by the remote peer) close a particular channel are sent
	// over.
	remoteCloseChanReqs chan *lnwire.CloseRequest

	// nextPendingChannelID is an integer which represents the id of the
	// next pending channel. Pending channels are tracked by this id
	// throughout their lifetime until they become active channels, or are
	// cancelled. Channels id's initiated by an outbound node start from 0,
	// while channels inititaed by an inbound node start from 2^63. In
	// either case, this value is always monotonically increasing.
	nextPendingChannelID uint64
	pendingChannelMtx    sync.RWMutex

	server *server

	queueQuit chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
}

// newPeer creates a new peer from an establish connection object, and a
// pointer to the main server.
func newPeer(conn net.Conn, server *server, btcNet wire.BitcoinNet, inbound bool) (*peer, error) {
	lndcConn := conn.(*lndc.LNDConn)
	nodePub := lndcConn.RemotePub

	p := &peer{
		conn:        conn,
		lightningID: wire.ShaHash(fastsha256.Sum256(nodePub.SerializeCompressed())),
		id:          atomic.AddInt32(&numNodes, 1),
		chainNet:    btcNet,
		inbound:     inbound,

		server: server,

		lastNMessages: make(map[lnwire.Message]struct{}),

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		barrierInits:     make(chan wire.OutPoint),
		newChanBarriers:  make(map[wire.OutPoint]chan struct{}),
		activeChannels:   make(map[wire.OutPoint]*lnwallet.LightningChannel),
		htlcManagers:     make(map[wire.OutPoint]chan lnwire.Message),
		chanSnapshotReqs: make(chan *chanSnapshotReq),
		newChannels:      make(chan *lnwallet.LightningChannel, 1),

		localCloseChanReqs:  make(chan *closeLinkReq),
		remoteCloseChanReqs: make(chan *lnwire.CloseRequest),

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
	}

	// TODO(roasbeef): re-write after lnaddr revamp, shouldn't need to use
	// type assertions
	var err error
	tcpAddr := lndcConn.Conn.(*net.TCPConn).RemoteAddr().(*net.TCPAddr)
	p.lightningAddr, err = lndc.NewLnAdr(tcpAddr, nodePub, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Initiate the pending channel identifier properly depending on if this
	// node is inbound or outbound. This value will be used in an increasing
	// manner to track pending channels.
	if inbound {
		p.nextPendingChannelID = 1 << 63
	} else {
		p.nextPendingChannelID = 0
	}

	// Fetch and then load all the active channels we have with this
	// remote peer from the database.
	activeChans, err := server.chanDB.FetchOpenChannels(&p.lightningID)
	if err != nil {
		peerLog.Errorf("unable to fetch active chans "+
			"for peer %v: %v", p, err)
		return nil, err
	}
	peerLog.Debugf("Loaded %v active channels from database with peerID(%v)",
		len(activeChans), p.id)
	if err := p.loadActiveChannels(activeChans); err != nil {
		return nil, err
	}

	return p, nil
}

// loadActiveChannels creates indexes within the peer for tracking all active
// channels returned by the database.
func (p *peer) loadActiveChannels(chans []*channeldb.OpenChannel) error {
	for _, dbChan := range chans {
		chanID := dbChan.ChanID
		lnChan, err := lnwallet.NewLightningChannel(p.server.lnwallet,
			p.server.lnwallet.ChainNotifier, p.server.chanDB, dbChan)
		if err != nil {
			return err
		}

		chanPoint := wire.OutPoint{
			Hash:  chanID.Hash,
			Index: chanID.Index,
		}
		p.activeChannels[chanPoint] = lnChan
		peerLog.Infof("peerID(%v) loaded ChannelPoint(%v)", p.id, chanPoint)

		// Register this new channel link with the HTLC Switch. This is
		// necessary to properly route multi-hop payments, and forward
		// new payments triggered by RPC clients.
		downstreamLink := make(chan *htlcPacket)
		plexChan := p.server.htlcSwitch.RegisterLink(p,
			dbChan.Snapshot(), downstreamLink)

		// TODO(roasbeef): buffer?
		upstreamLink := make(chan lnwire.Message)
		p.htlcManagers[chanPoint] = upstreamLink
		p.wg.Add(1)
		go p.htlcManager(lnChan, plexChan, downstreamLink, upstreamLink)
	}

	return nil
}

// Start starts all helper goroutines the peer needs for normal operations.
// In the case this peer has already beeen started, then this function is a
// noop.
func (p *peer) Start() error {
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	peerLog.Tracef("peer %v starting", p)

	p.wg.Add(4)
	go p.readHandler()
	go p.queueHandler()
	go p.writeHandler()
	go p.channelManager()

	return nil
}

// Stop signals the peer for a graceful shutdown. All active goroutines will be
// signaled to wrap up any final actions. This function will also block until
// all goroutines have exited.
func (p *peer) Stop() error {
	// If we're already disconnecting, just exit.
	if atomic.AddInt32(&p.disconnect, 1) != 1 {
		return nil
	}

	// Otherwise, close the connection if we're currently connected.
	if atomic.LoadInt32(&p.connected) != 0 {
		p.conn.Close()
	}

	// Signal all worker goroutines to gracefully exit.
	close(p.quit)
	p.wg.Wait()

	return nil
}

// Disconnect terminates the connection with the remote peer. Additionally, a
// signal is sent to the server and htlcSwitch indicating the resources
// allocated to the peer can now be cleaned up.
func (p *peer) Disconnect() {
	if !atomic.CompareAndSwapInt32(&p.disconnect, 0, 1) {
		return
	}

	peerLog.Tracef("Disconnecting %s", p)
	if atomic.LoadInt32(&p.connected) != 0 {
		p.conn.Close()
	}

	close(p.quit)

	// Launch a goroutine to clean up the remaining resources.
	go func() {
		// Tell the switch to unregister all links associated with this
		// peer. Passing nil as the target link indicates that all links
		// associated with this interface should be closed.
		p.server.htlcSwitch.UnregisterLink(p.lightningID, nil)

		p.server.donePeers <- p
	}()
}

// String returns the string representation of this peer.
func (p *peer) String() string {
	return p.conn.RemoteAddr().String()
}

// readNextMessage reads, and returns the next message on the wire along with
// any additional raw payload.
func (p *peer) readNextMessage() (lnwire.Message, []byte, error) {
	// TODO(roasbeef): use our own net magic?
	n, nextMsg, rawPayload, err := lnwire.ReadMessage(p.conn, 0, p.chainNet)
	atomic.AddUint64(&p.bytesReceived, uint64(n))
	if err != nil {
		return nil, nil, err
	}

	// TODO(roasbeef): add message summaries
	peerLog.Tracef("readMessage from %v: %v", p, newLogClosure(func() string {
		return spew.Sdump(nextMsg)
	}))

	return nextMsg, rawPayload, nil
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper sub-system.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) readHandler() {
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, _, err := p.readNextMessage()
		if err != nil {
			peerLog.Infof("unable to read message: %v", err)
			break out
		}

		var isChanUpate bool
		var targetChan *wire.OutPoint

		switch msg := nextMsg.(type) {
		// TODO(roasbeef): consolidate into predicate (single vs dual)
		case *lnwire.SingleFundingRequest:
			p.server.fundingMgr.processFundingRequest(msg, p)
		case *lnwire.SingleFundingResponse:
			p.server.fundingMgr.processFundingResponse(msg, p)
		case *lnwire.SingleFundingComplete:
			p.server.fundingMgr.processFundingComplete(msg, p)
		case *lnwire.SingleFundingSignComplete:
			p.server.fundingMgr.processFundingSignComplete(msg, p)
		case *lnwire.SingleFundingOpenProof:
			p.server.fundingMgr.processFundingOpenProof(msg, p)
		case *lnwire.CloseRequest:
			p.remoteCloseChanReqs <- msg
		// TODO(roasbeef): interface for htlc update msgs
		//  * .(CommitmentUpdater)
		case *lnwire.HTLCAddRequest:
			isChanUpate = true
			targetChan = msg.ChannelPoint
		case *lnwire.HTLCSettleRequest:
			isChanUpate = true
			targetChan = msg.ChannelPoint
		case *lnwire.CommitRevocation:
			isChanUpate = true
			targetChan = msg.ChannelPoint
		case *lnwire.CommitSignature:
			isChanUpate = true
			targetChan = msg.ChannelPoint
		}

		if isChanUpate {
			// We might be receiving an update to a newly funded
			// channel in which we were the responder. Therefore
			// we need to possibly block until the new channel has
			// propagated internally through the system.
			p.barrierMtx.RLock()
			barrier, ok := p.newChanBarriers[*targetChan]
			p.barrierMtx.RUnlock()
			if ok {
				peerLog.Tracef("waiting for chan barrier "+
					"signal for ChannelPoint(%v)", targetChan)
				select {
				case <-barrier:
				case <-p.quit: // TODO(roasbeef): add timer?
					break out
				}
				peerLog.Tracef("barrier for ChannelPoint(%v) "+
					"closed", targetChan)
			}

			// Dispatch the commitment update message to the proper
			// active goroutine dedicated to this channel.
			targetChan, ok := p.htlcManagers[*targetChan]
			if !ok {
				peerLog.Errorf("recv'd update for unknown channel %v",
					targetChan)
				continue
			}
			targetChan <- nextMsg
		}
	}

	p.Disconnect()

	p.wg.Done()
	peerLog.Tracef("readHandler for peer %v done", p)
}

// writeMessage writes the target lnwire.Message to the remote peer.
func (p *peer) writeMessage(msg lnwire.Message) error {
	// Simply exit if we're shutting down.
	if atomic.LoadInt32(&p.disconnect) != 0 {
		return nil
	}

	// TODO(roasbeef): add message summaries
	peerLog.Tracef("writeMessage to %v: %v", p, newLogClosure(func() string {
		return spew.Sdump(msg)
	}))

	n, err := lnwire.WriteMessage(p.conn, msg, 0, p.chainNet)
	atomic.AddUint64(&p.bytesSent, uint64(n))

	return err
}

// writeHandler is a goroutine dedicated to reading messages off of an incoming
// queue, and writing them out to the wire. This goroutine coordinates with the
// queueHandler in order to ensure the incoming message queue is quickly drained.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) writeHandler() {
	// pingTicker is used to periodically send pings to the remote peer.
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

out:
	for {
		select {
		case outMsg := <-p.sendQueue:
			switch m := outMsg.msg.(type) {
			// TODO(roasbeef): handle special write cases
			}

			if err := p.writeMessage(outMsg.msg); err != nil {
				peerLog.Errorf("unable to write message: %v", err)
				p.Disconnect()
				break out
			}

			// Synchronize with the writeHandler.
			p.sendQueueSync <- struct{}{}
		case <-pingTicker.C:
			// TODO(roasbeef): move ping to time.AfterFunc
		case <-p.quit:
			break out
		}
	}

	// Wait for the queueHandler to finish so we can empty out all pending
	// messages avoiding a possible deadlock somewhere.
	<-p.queueQuit

	// Drain any lingering messages that we're meant to be sent. But since
	// we're shutting down, just ignore them.
fin:
	for {
		select {
		case msg := <-p.sendQueue:
			if msg.sentChan != nil {
				msg.sentChan <- struct{}{}
			}
		default:
			break fin
		}
	}
	p.wg.Done()
	peerLog.Tracef("writeHandler for peer %v done", p)
}

// queueHandler is responsible for accepting messages from outside sub-systems
// to be eventually sent out on the wire by the writeHandler.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) queueHandler() {
	waitOnSync := false
	pendingMsgs := list.New()
out:
	for {
		select {
		case msg := <-p.outgoingQueue:
			if !waitOnSync {
				p.sendQueue <- msg
			} else {
				pendingMsgs.PushBack(msg)
			}
			waitOnSync = true
		case <-p.sendQueueSync:
			// If there aren't any more remaining messages in the
			// queue, then we're no longer waiting to synchronize
			// with the writeHandler.
			next := pendingMsgs.Front()
			if next == nil {
				waitOnSync = false
				continue
			}

			// Notify the writeHandler about the next item to
			// asynchronously send.
			val := pendingMsgs.Remove(next)
			p.sendQueue <- val.(outgoinMsg)
			// TODO(roasbeef): other sync stuffs
		case <-p.quit:
			break out
		}
	}

	close(p.queueQuit)
	p.wg.Done()
}

// queueMsg queues a new lnwire.Message to be eventually sent out on the
// wire.
func (p *peer) queueMsg(msg lnwire.Message, doneChan chan struct{}) {
	p.outgoingQueue <- outgoinMsg{msg, doneChan}
}

// ChannelSnapshots returns a slice of channel snapshots detaling all currently
// active channels maintained with the remote peer.
func (p *peer) ChannelSnapshots() []*channeldb.ChannelSnapshot {
	resp := make(chan []*channeldb.ChannelSnapshot, 1)
	p.chanSnapshotReqs <- &chanSnapshotReq{resp}
	return <-resp
}

// channelManager is goroutine dedicated to handling all requests/signals
// pertaining to the opening, cooperative closing, and force closing of all
// channels maintained with the remote peer.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) channelManager() {
out:
	for {
		select {
		case req := <-p.chanSnapshotReqs:
			snapshots := make([]*channeldb.ChannelSnapshot, 0, len(p.activeChannels))
			for _, activeChan := range p.activeChannels {
				snapshot := activeChan.StateSnapshot()
				snapshots = append(snapshots, snapshot)
			}
			req.resp <- snapshots

		case pendingChanPoint := <-p.barrierInits:
			p.barrierMtx.Lock()
			peerLog.Tracef("Creating chan barrier for "+
				"ChannelPoint(%v)", pendingChanPoint)
			p.newChanBarriers[pendingChanPoint] = make(chan struct{})
			p.barrierMtx.Unlock()

		case newChan := <-p.newChannels:
			chanPoint := *newChan.ChannelPoint()
			p.activeChannels[chanPoint] = newChan

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			// Now that the channel is open, notify the Htlc
			// Switch of a new active link.
			chanSnapShot := newChan.StateSnapshot()
			downstreamLink := make(chan *htlcPacket)
			plexChan := p.server.htlcSwitch.RegisterLink(p,
				chanSnapShot, downstreamLink)

			// With the channel registered to the HtlcSwitch spawn
			// a goroutine to handle commitment updates for this
			// new channel.
			upstreamLink := make(chan lnwire.Message)
			p.htlcManagers[chanPoint] = upstreamLink
			p.wg.Add(1)
			go p.htlcManager(newChan, plexChan, downstreamLink, upstreamLink)

			// Close the active channel barrier signalling the
			// readHandler that commitment related modifications to
			// this channel can now proceed.
			p.barrierMtx.Lock()
			peerLog.Tracef("Closing chan barrier for ChannelPoint(%v)", chanPoint)
			close(p.newChanBarriers[chanPoint])
			delete(p.newChanBarriers, chanPoint)
			p.barrierMtx.Unlock()

		case req := <-p.localCloseChanReqs:
			p.handleLocalClose(req)

		case req := <-p.remoteCloseChanReqs:
			p.handleRemoteClose(req)

		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
}

// handleLocalClose kicks-off the workflow to execute a cooperative closure of
// the channel initiated by a local sub-system.
func (p *peer) handleLocalClose(req *closeLinkReq) {
	chanPoint := req.chanPoint
	key := wire.OutPoint{
		Hash:  chanPoint.Hash,
		Index: chanPoint.Index,
	}
	channel := p.activeChannels[key]

	// Shift the channel state machine into a 'closing' state. This
	// generates a signature for the closing tx, as well as a txid of the
	// closing tx itself, allowing us to watch the network to determine
	// when the remote node broadcasts the fully signed closing transaction.
	sig, txid, err := channel.InitCooperativeClose()
	if err != nil {
		req.resp <- nil
		req.err <- err
		return
	}
	peerLog.Infof("Executing cooperative closure of "+
		"ChanPoint(%v) with peerID(%v), txid=%v", key, p.id,
		txid)

	// With our signature for the close tx generated, send the signature
	// to the remote peer instructing it to close this particular channel
	// point.
	// TODO(roasbeef): remove encoding redundancy
	closeSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		req.resp <- nil
		req.err <- err
		return
	}
	closeReq := lnwire.NewCloseRequest(chanPoint, closeSig)
	p.queueMsg(closeReq, nil)

	// Finally, launch a goroutine which will request to be notified by the
	// ChainNotifier once the closure transaction obtains a single
	// confirmation.
	go func() {
		// TODO(roasbeef): add param for num needed confs
		notifier := p.server.lnwallet.ChainNotifier
		confNtfn, _ := notifier.RegisterConfirmationsNtfn(txid, 1)

		var success bool
		select {
		case height, ok := <-confNtfn.Confirmed:
			// In the case that the ChainNotifier is shutting
			// down, all subscriber notification channels will be
			// closed, generating a nil receive.
			if !ok {
				// TODO(roasbeef): check for nil elsewhere
				return
			}

			// The channel has been closed, remove it from any
			// active indexes, and the database state.
			peerLog.Infof("ChannelPoint(%v) is now "+
				"closed at height %v", key, height)
			wipeChannel(p, channel)

			success = true
		case <-p.quit:
			return
		}

		// Respond to the local sub-system which requested the channel
		// closure.
		req.resp <- &closeLinkResp{txid, success}
		req.err <- nil
	}()
}

// handleRemoteClose completes a request for cooperative channel closure
// initiated by the remote node.
func (p *peer) handleRemoteClose(req *lnwire.CloseRequest) {
	chanPoint := req.ChannelPoint
	key := wire.OutPoint{
		Hash:  chanPoint.Hash,
		Index: chanPoint.Index,
	}
	channel := p.activeChannels[key]

	// Now that we have their signature for the closure transaction, we
	// can assemble the final closure transaction, complete with our
	// signature.
	sig := req.RequesterCloseSig
	closeSig := append(sig.Serialize(), byte(txscript.SigHashAll))
	closeTx, err := channel.CompleteCooperativeClose(closeSig)
	if err != nil {
		peerLog.Errorf("unable to complete cooperative "+
			"close for ChannelPoint(%v): %v",
			chanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		return
	}

	// Finally, broadcast the closure transaction, to the network.
	peerLog.Infof("Broadcasting cooperative close tx: %v", newLogClosure(func() string {
		return spew.Sdump(closeTx)
	}))
	if err := p.server.lnwallet.PublishTransaction(closeTx); err != nil {
		peerLog.Errorf("channel close tx from "+
			"ChannelPoint(%v) rejected: %v",
			chanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		return
	}

	// TODO(roasbeef): also wait for confs before removing state
	peerLog.Infof("ChannelPoint(%v) is now "+
		"closed", key)
	wipeChannel(p, channel)
}

// wipeChannel removes the passed channel from all indexes associated with the
// peer, and deletes the channel from the database.
func wipeChannel(p *peer, channel *lnwallet.LightningChannel) {
	chanID := channel.ChannelPoint()

	delete(p.activeChannels, *chanID)

	// Instruct the Htlc Switch to close this link as the channel is no
	// longer active.
	p.server.htlcSwitch.UnregisterLink(p.lightningID, chanID)
	htlcWireLink := p.htlcManagers[*chanID]
	delete(p.htlcManagers, *chanID)
	close(htlcWireLink)

	if err := channel.DeleteState(); err != nil {
		peerLog.Errorf("Unable to delete ChannelPoint(%v) "+
			"from db %v", chanID, err)
	}
}

// pendingPayment represents a pending HTLC which has yet to be settled by the
// upstream peer. A pending payment encapsulates the initial HTLC add request
// additionally coupling the index of the HTLC within the log, and an error
// channel to signal the payment requester once the payment has been fully
// fufilled.
type pendingPayment struct {
	htlc  *lnwire.HTLCAddRequest
	index uint32

	err chan error
}

// commitmentState is the volatile+persistent state of an active channel's
// commitment update state-machine. This struct is used by htlcManager's to
// save meta-state required for proper functioning.
type commitmentState struct {
	// TODO(roasbeef): use once trickle+batch logic is in
	pendingLogLen uint32

	// htlcsToSettle is a list of preimages which allow us to settle one or
	// many of the pending HTLC's we've received from the upstream peer.
	// TODO(roasbeef): should send sig to settle once preimage is known.
	htlcsToSettle [][32]byte

	// sigPending is a bool which indicates if we're currently awaiting a
	// signature response to a commitment update we've initiated.
	sigPending bool

	// clearedHTCLs is a map of outgoing HTLC's we've committed to in our
	// chain which have not yet been settled by the upstream peer.
	clearedHTCLs map[uint32]*pendingPayment

	channel   *lnwallet.LightningChannel
	chanPoint *wire.OutPoint
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// The htlcManager reads messages from the upstream (remote) peer, and also
// from several possible downstream channels managed by the htlcSwitch. In the
// event that an htlc needs to be forwarded, then send-only htlcPlex chan is
// used which sends htlc packets to the switch for forwarding. Additionally,
// the htlcManager handles acting upon all timeouts for any active HTLC's,
// manages the channel's revocation window, and also the htlc trickle
// queue+timer for this active channels.
func (p *peer) htlcManager(channel *lnwallet.LightningChannel,
	htlcPlex chan<- *htlcPacket, downstreamLink <-chan *htlcPacket,
	upstreamLink <-chan lnwire.Message) {

	chanStats := channel.StateSnapshot()
	peerLog.Tracef("HTLC manager for ChannelPoint(%v) started, "+
		"our_balance=%v, their_balance=%v, chain_height=%v",
		channel.ChannelPoint(), chanStats.LocalBalance,
		chanStats.RemoteBalance, chanStats.NumUpdates)

	// A new session for this active channel has just started, therefore we
	// need to send our initial revocation window to the remote peer.
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		rev, err := channel.ExtendRevocationWindow()
		if err != nil {
			peerLog.Errorf("unable to expand revocation window: %v", err)
			continue
		}
		p.queueMsg(rev, nil)
	}

	state := &commitmentState{
		channel:      channel,
		chanPoint:    channel.ChannelPoint(),
		clearedHTCLs: make(map[uint32]*pendingPayment),
	}
out:
	for {
		select {
		case pkt := <-downstreamLink:
			p.handleDownStreamPkt(state, pkt)
		case msg, ok := <-upstreamLink:
			// If the upstream message link is closed, this signals
			// that the channel itself is being closed, therefore
			// we exit.
			if !ok {
				break out
			}

			p.handleUpstreamMsg(state, msg)
		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
	peerLog.Tracef("htlcManager for peer %v done", p)
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLC's, timeout previously cleared HTLC's, and finally to settle currently
// cleared HTLC's with the upstream peer.
func (p *peer) handleDownStreamPkt(state *commitmentState, pkt *htlcPacket) {
	switch htlc := pkt.msg.(type) {
	case *lnwire.HTLCAddRequest:
		// A new payment has been initiated via the
		// downstream channel, so we add the new HTLC
		// to our local log, then update the commitment
		// chains.
		index := state.channel.AddHTLC(htlc, false)
		p.queueMsg(htlc, nil)

		// TODO(roasbeef): batch trickle timer + cap
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update "+
				"commitment: %v", err)
		}

		state.sigPending = true
		state.clearedHTCLs[index] = &pendingPayment{
			htlc:  htlc,
			index: index,
			err:   pkt.err,
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (p *peer) handleUpstreamMsg(state *commitmentState, msg lnwire.Message) {
	switch htlcPkt := msg.(type) {
	// TODO(roasbeef): timeouts
	case *lnwire.HTLCAddRequest:
		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the pre-image
		state.channel.AddHTLC(htlcPkt, true)

		rHash := htlcPkt.RedemptionHashes[0]
		if invoice, found := p.server.invoices.lookupInvoice(rHash); found {
			// TODO(roasbeef): check value
			//  * onion layer strip should also be before invoice lookup
			//  * also can immediately send the settle msg
			pre := invoice.paymentPreimage
			state.htlcsToSettle = append(state.htlcsToSettle, pre)
		}
	case *lnwire.HTLCSettleRequest:
		// TODO(roasbeef): this assumes no "multi-sig"
		pre := htlcPkt.RedemptionProofs[0]
		if _, err := state.channel.SettleHTLC(pre, true); err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for outgoing HTLC rejected: %v", err)
			p.Disconnect()
			return
		}
	case *lnwire.CommitSignature:
		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		// TODO(roasbeef): use uint64 for indexes?
		logIndex := uint32(htlcPkt.LogIndex)
		sig := htlcPkt.CommitSig.Serialize()
		if err := state.channel.ReceiveNewCommitment(sig, logIndex); err != nil {
			peerLog.Errorf("unable to accept new commitment: %v", err)
			p.Disconnect()
			return
		}

		// If we didn't initiate this state transition, then we'll
		// update the remote commitment chain with a new commitment.
		// Otherwise, we can reset the pending bit as we received the
		// signature we were expecting.
		// TODO(roasbeef): move sig updates to own trigger
		//  * can remove sigPending if so
		if !state.sigPending {
			if err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				return
			}
		} else {
			state.sigPending = false
		}

		// Finally, since we just accepted a new state, send the remote
		// peer a revocation for our prior state.
		nextRevocation, err := state.channel.RevokeCurrentCommitment()
		if err != nil {
			peerLog.Errorf("unable to revoke current commitment: %v", err)
			return
		}
		p.queueMsg(nextRevocation, nil)
	case *lnwire.CommitRevocation:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		htlcsToForward, err := state.channel.ReceiveRevocation(htlcPkt)
		if err != nil {
			peerLog.Errorf("unable to accept revocation: %v", err)
			p.Disconnect()
			return
		}
		// TODO(roasbeef): send the locked-in HTLC's over the plex chan
		// to the switch.
		peerLog.Debugf("htlcs ready to forward: %v",
			spew.Sdump(htlcsToForward))

		// If any of the htlc's eligible for forwarding are pending
		// settling or timeing out previous outgoing payments, then we
		// can them from the pending set, and signal the requster (if
		// existing) that the payment has been fully fulfilled.
		for _, htlc := range htlcsToForward {
			if p, ok := state.clearedHTCLs[htlc.ParentIndex]; ok {
				peerLog.Debugf("local htlc %v cleared",
					spew.Sdump(p.htlc))
				p.err <- nil
				delete(state.clearedHTCLs, htlc.ParentIndex)
			}
		}

		// A full state transition has been completed, if we don't need
		// to settle any HTLC's, then we're done.
		if len(state.htlcsToSettle) == 0 {
			return
		}

		// Otherwise, we have some pending HTLC's which we can pull
		// funds from, thereby settling.
		peerLog.Tracef("settling %v HTLC's", len(state.htlcsToSettle))
		for _, pre := range state.htlcsToSettle {
			// Add each HTLC settle update to the channel's state
			// update log, also sending the log update to the
			// remote party.
			logIndex, err := state.channel.SettleHTLC(pre, false)
			if err != nil {
				peerLog.Errorf("unable to settle htlc: %v", err)
				continue
			}
			settleMsg := &lnwire.HTLCSettleRequest{
				ChannelPoint:     state.chanPoint,
				HTLCKey:          lnwire.HTLCKey(logIndex),
				RedemptionProofs: [][32]byte{pre},
			}
			p.queueMsg(settleMsg, nil)
		}

		// With all the settle updates added to the local and remote
		// HTLC logs, initiate a state transition by updating the
		// remote commitment chain.
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update "+
				"commitment: %v", err)
			return
		}
		state.sigPending = true
		state.htlcsToSettle = nil
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (p *peer) updateCommitTx(state *commitmentState) error {
	sigTheirs, logIndexTheirs, err := state.channel.SignNextCommitment()
	if err != nil {
		return fmt.Errorf("unable to sign next commitment: %v", err)
	}

	parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
	if err != nil {
		return fmt.Errorf("unable to parse sig: %v", err)
	}

	commitSig := &lnwire.CommitSignature{
		ChannelPoint: state.chanPoint,
		CommitSig:    parsedSig,
		LogIndex:     uint64(logIndexTheirs),
	}
	p.queueMsg(commitSig, nil)

	return nil
}

// TODO(roasbeef): make all start/stop mutexes a CAS
