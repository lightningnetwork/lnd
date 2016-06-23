package main

import (
	"container/list"
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

	inbound         bool
	protocolVersion uint32
	id              int32

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
	activeChannels map[wire.OutPoint]*lnwallet.LightningChannel

	// newChanBarriers is a map from a channel point to a 'barrier' which
	// will be signalled once the channel is fully open. This barrier acts
	// as a synchronization point for any incoming/outgoing HTLCs before
	// the channel has been fully opened.
	// TODO(roasbeef): barrier to sync chan open and handling of first htlc
	// message.
	newChanBarriers map[wire.OutPoint]chan struct{}

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	// TODO(roasbeef): barrier to block until chan open before update
	newChannels chan *lnwallet.LightningChannel

	// localCloseChanReqs is a channel in which any local requests to
	// close a particular channel are sent over.
	localCloseChanReqs chan *closeChanReq

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
func newPeer(conn net.Conn, server *server, net wire.BitcoinNet, inbound bool) (*peer, error) {
	nodePub := conn.(*lndc.LNDConn).RemotePub

	p := &peer{
		conn:        conn,
		lightningID: wire.ShaHash(fastsha256.Sum256(nodePub.SerializeCompressed())),
		id:          atomic.AddInt32(&numNodes, 1),
		chainNet:    net,
		inbound:     inbound,

		server: server,

		lastNMessages: make(map[lnwire.Message]struct{}),

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		newChanBarriers: make(map[wire.OutPoint]chan struct{}),
		activeChannels:  make(map[wire.OutPoint]*lnwallet.LightningChannel),
		newChannels:     make(chan *lnwallet.LightningChannel, 1),

		localCloseChanReqs:  make(chan *closeChanReq),
		remoteCloseChanReqs: make(chan *lnwire.CloseRequest),

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
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

		// Update the server's global channel index.
		p.server.chanIndexMtx.Lock()
		p.server.chanIndex[chanPoint] = p
		p.server.chanIndexMtx.Unlock()
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

	p.wg.Add(5)
	go p.readHandler()
	go p.queueHandler()
	go p.writeHandler()
	go p.channelManager()
	go p.htlcManager()

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
	// TODO(roasbeef): set timeout for initial channel request or version
	// exchange.

out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, _, err := p.readNextMessage()
		if err != nil {
			peerLog.Infof("unable to read message: %v", err)
			break out
		}

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
		}
	}

	p.wg.Done()
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
				// TODO(roasbeef): disconnect
				peerLog.Errorf("unable to write message: %v", err)
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

// channelManager is goroutine dedicated to handling all requests/signals
// pertaining to the opening, cooperative closing, and force closing of all
// channels maintained with the remote peer.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) channelManager() {
out:
	for {
		select {
		case newChan := <-p.newChannels:
			chanPoint := *newChan.ChannelPoint()
			p.activeChannels[chanPoint] = newChan

			// TODO(roasbeef): signal channel barrier
			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			// Now that the channel is open, update the server's
			// map of channels to the peers we have a particular
			// channel open to.
			// TODO(roasbeef): should server have this knowledge?
			p.server.chanIndexMtx.Lock()
			p.server.chanIndex[chanPoint] = p
			p.server.chanIndexMtx.Unlock()
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
func (p *peer) handleLocalClose(req *closeChanReq) {
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
		}

		// Respond to the local sub-system which requested the channel
		// closure.
		req.resp <- &closeChanResp{success}
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
	chanID := *channel.ChannelPoint()

	delete(p.activeChannels, chanID)

	p.server.chanIndexMtx.Lock()
	delete(p.server.chanIndex, chanID)
	p.server.chanIndexMtx.Unlock()

	if err := channel.DeleteState(); err != nil {
		peerLog.Errorf("Unable to delete ChannelPoint(%v) "+
			"from db %v", chanID, err)
	}
}

// htlcManager...
//  * communicates with the htlc switch over several channels
//  * in handler sends to this goroutine after getting final revocation
//  * has timeouts etc, to send back on queue handler in case of timeout
func (p *peer) htlcManager() {
out:
	for {
		select {
		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
}

// TODO(roasbeef): make all start/stop mutexes a CAS
