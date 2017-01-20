package main

import (
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"
	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/connmgr"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

var (
	numNodes int32
)

const (
	// pingInterval is the interval at which ping messages are sent.
	pingInterval = 1 * time.Minute

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

// chanSnapshotReq is a message sent by outside subsystems to a peer in order
// to gain a snapshot of the peer's currently active channels.
type chanSnapshotReq struct {
	resp chan []*channeldb.ChannelSnapshot
}

// peer is an active peer on the Lightning Network. This struct is responsible
// for managing any channel state related to this peer. To do so, it has
// several helper goroutines to handle events such as HTLC timeouts, new
// funding workflow, and detecting an uncooperative closure of any active
// channels.
// TODO(roasbeef): proper reconnection logic
type peer struct {
	// MUST be used atomically.
	started    int32
	connected  int32
	disconnect int32

	connReq *connmgr.ConnReq
	conn    net.Conn

	addr        *lnwire.NetAddress
	lightningID chainhash.Hash

	inbound bool
	id      int32

	// pingTime is a rough estimate of the RTT (round-trip-time) between us
	// and the connected peer. This time is expressed in micro seconds.
	// TODO(roasbeef): also use a WMA or EMA?
	pingTime int64

	// pingLastSend is the Unix time expressed in nanoseconds when we sent
	// our last ping message.
	pingLastSend int64

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
	activeChanMtx    sync.RWMutex
	activeChannels   map[wire.OutPoint]*lnwallet.LightningChannel
	chanSnapshotReqs chan *chanSnapshotReq

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

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *closeChanReq

	// remoteCloseChanReqs is a channel in which any remote requests
	// (initiated by the remote peer) close a particular channel are sent
	// over.
	remoteCloseChanReqs chan *lnwire.CloseRequest

	// nextPendingChannelID is an integer which represents the id of the
	// next pending channel. Pending channels are tracked by this id
	// throughout their lifetime until they become active channels, or are
	// cancelled. Channels id's initiated by an outbound node start from 0,
	// while channels initiated by an inbound node start from 2^63. In
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
func newPeer(conn net.Conn, server *server, addr *lnwire.NetAddress,
	inbound bool) (*peer, error) {

	nodePub := addr.IdentityKey

	p := &peer{
		conn:        conn,
		lightningID: chainhash.Hash(fastsha256.Sum256(nodePub.SerializeCompressed())),
		addr:        addr,

		id:      atomic.AddInt32(&numNodes, 1),
		inbound: inbound,

		server: server,

		lastNMessages: make(map[lnwire.Message]struct{}),

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		barrierInits:     make(chan wire.OutPoint),
		newChanBarriers:  make(map[wire.OutPoint]chan struct{}),
		activeChannels:   make(map[wire.OutPoint]*lnwallet.LightningChannel),
		chanSnapshotReqs: make(chan *chanSnapshotReq),
		newChannels:      make(chan *lnwallet.LightningChannel, 1),

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
	activeChans, err := server.chanDB.FetchOpenChannels(p.addr.IdentityKey)
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
		lnChan, err := lnwallet.NewLightningChannel(p.server.lnwallet.Signer,
			p.server.bio, p.server.chainNotifier, dbChan)
		if err != nil {
			return err
		}

		chanPoint := wire.OutPoint{
			Hash:  chanID.Hash,
			Index: chanID.Index,
		}
		p.activeChanMtx.Lock()
		p.activeChannels[chanPoint] = lnChan
		p.activeChanMtx.Unlock()

		peerLog.Infof("peerID(%v) loaded ChannelPoint(%v)", p.id, chanPoint)

		p.server.breachArbiter.newContracts <- lnChan

		sphinxDecoder := routing.NewSphinxDecoder(p.server.sphinx)
		htlcManager := htlcswitch.NewHTLCManager(
			&htlcswitch.HTLCManagerConfig{
				Peer:          p,
				DecodeOnion:   sphinxDecoder.Decode,
				SettledContracts: p.server.breachArbiter.settledContracts,
				DebugHTLC:     cfg.DebugHTLC,
				Registry:      p.server.invoices,
				Forward:       p.server.htlcSwitch.Forward,
			}, lnChan)

		if err := p.server.htlcSwitch.Add(htlcManager); err != nil {
			return err
		}
	}

	return nil
}

// Start starts all helper goroutines the peer needs for normal operations.
// In the case this peer has already been started, then this function is a
// noop.
func (p *peer) Start() error {
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	atomic.AddInt32(&p.connected, 1)

	peerLog.Tracef("peer %v starting", p)

	p.wg.Add(5)
	go p.readHandler()
	go p.queueHandler()
	go p.writeHandler()
	go p.channelManager()
	go p.pingHandler()

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

	if p.connReq != nil {
		p.server.connMgr.Disconnect(p.connReq.ID())
	}

	// Launch a goroutine to clean up the remaining resources.
	go func() {
		// Tell the switch to unregister all links associated with this
		// peer. Passing nil as the target link indicates that all links
		// associated with this interface should be closed.
		p.server.htlcSwitch.RemoveById(p.HopID())

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
	n, nextMsg, rawPayload, err := lnwire.ReadMessage(p.conn, 0,
		p.addr.ChainNet)
	atomic.AddUint64(&p.bytesReceived, uint64(n))
	if err != nil {
		return nil, nil, err
	}

	// TODO(roasbeef): add message summaries
	peerLog.Tracef("readMessage to %v: %v", p, lnwire.MessageToStringClosure(nextMsg))

	return nextMsg, rawPayload, nil
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper subsystem.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) readHandler() {
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, _, err := p.readNextMessage()
		if err != nil {
			peerLog.Infof("unable to read message from %v: %v",
				p, err)

			switch err.(type) {
			// If this is just a message we don't yet recognize,
			// we'll continue processing as normal as this allows
			// us to introduce new messages in a forwards
			// compatible manner.
			case *lnwire.UnknownMessage:
				continue

			// If the error we encountered wasn't just a message we
			// didn't recognize, then we'll stop all processing s
			// this is a fatal error.
			default:
				break out
			}
		}

		var isChanUpdate bool
		var targetChan *wire.OutPoint

		switch msg := nextMsg.(type) {
		case *lnwire.Pong:
			// When we receive a Pong message in response to our
			// last ping message, we'll use the time in which we
			// sent the ping message to measure a rough estimate of
			// round trip time.
			pingSendTime := atomic.LoadInt64(&p.pingLastSend)
			delay := (time.Now().UnixNano() - pingSendTime) / 1000
			atomic.StoreInt64(&p.pingTime, delay)

		case *lnwire.Ping:
			p.queueMsg(lnwire.NewPong(msg.Nonce), nil)

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

		case *lnwire.ErrorGeneric:
			p.server.fundingMgr.processErrorGeneric(msg, p)

		// TODO(roasbeef): create ChanUpdater interface for the below
		case *lnwire.HTLCAddRequest:
			isChanUpdate = true
			targetChan = msg.ChannelPoint
		case *lnwire.HTLCSettleRequest:
			isChanUpdate = true
			targetChan = msg.ChannelPoint
		case *lnwire.CancelHTLC:
			isChanUpdate = true
			targetChan = msg.ChannelPoint
		case *lnwire.CommitRevocation:
			isChanUpdate = true
			targetChan = msg.ChannelPoint
		case *lnwire.CommitSignature:
			isChanUpdate = true
			targetChan = msg.ChannelPoint

		case *lnwire.NodeAnnouncement,
			*lnwire.ChannelAnnouncement,
			*lnwire.ChannelUpdateAnnouncement:

			p.server.chanRouter.ProcessRoutingMessage(msg,
				p.addr.IdentityKey)
		}

		if isChanUpdate {
			// We might be receiving an update to a newly funded
			// channel in which we were the responder. Therefore
			// we need to possibly block until the new channel has
			// propagated internally through the system.
			// TODO(roasbeef): replace with atomic load from/into
			// map?
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
			manager, err := p.server.htlcSwitch.Get(targetChan)
			if err != nil {
				peerLog.Warn(err)
				continue
			}

			peerLog.Tracef("htlc manager handle message: %v", nextMsg)
			if err := manager.HandleMessage(nextMsg); err != nil {
				peerLog.Errorf("htlc manager can't handle the "+
					"message: %v", err)
				continue
			}
			peerLog.Trace("forward handle message done")
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
	peerLog.Tracef("writeMessage to %v: %v", p, lnwire.MessageToStringClosure(msg))

	n, err := lnwire.WriteMessage(p.conn, msg, 0, p.addr.ChainNet)
	atomic.AddUint64(&p.bytesSent, uint64(n))

	return err
}

// writeHandler is a goroutine dedicated to reading messages off of an incoming
// queue, and writing them out to the wire. This goroutine coordinates with the
// queueHandler in order to ensure the incoming message queue is quickly drained.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) writeHandler() {
out:
	for {
		select {
		case outMsg := <-p.sendQueue:
			if err := p.writeMessage(outMsg.msg); err != nil {
				peerLog.Errorf("unable to write message: %v",
					err)
				p.Disconnect()
				break out
			}

			switch outMsg.msg.(type) {
			case *lnwire.Ping:
				// TODO(roasbeef): do this before the write?
				// possibly account for processing within func?
				now := time.Now().UnixNano()
				atomic.StoreInt64(&p.pingLastSend, now)
			}

			// Synchronize with the writeHandler.
			p.sendQueueSync <- struct{}{}
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

// queueHandler is responsible for accepting messages from outside subsystems
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

// pingHandler is responsible for periodically sending ping messages to the
// remote peer in order to keep the connection alive and/or determine if the
// connection is still active.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) pingHandler() {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	var pingBuf [8]byte

out:
	for {
		select {
		case <-pingTicker.C:
			// Fill the ping buffer with fresh randomness. If we're
			// unable to read enough bytes, then we simply defer
			// sending the ping to the next interval.
			if _, err := rand.Read(pingBuf[:]); err != nil {
				peerLog.Errorf("unable to send ping to %v: %v", p,
					err)
				continue
			}

			// Convert the bytes read into a uint64, and queue the
			// message for sending.
			nonce := binary.BigEndian.Uint64(pingBuf[:])
			p.queueMsg(lnwire.NewPing(nonce), nil)
		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
}

// PingTime returns the estimated ping time to the peer in microseconds.
func (p *peer) PingTime() int64 {
	return atomic.LoadInt64(&p.pingTime)
}

// queueMsg queues a new lnwire.Message to be eventually sent out on the
// wire.
func (p *peer) queueMsg(msg lnwire.Message, doneChan chan struct{}) {
	select {
	case p.outgoingQueue <- outgoinMsg{msg, doneChan}:
	case <-p.quit:
		return
	}
}

// ChannelSnapshots returns a slice of channel snapshots detailing all
// currently active channels maintained with the remote peer.
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
			p.activeChanMtx.RLock()
			snapshots := make([]*channeldb.ChannelSnapshot, 0, len(p.activeChannels))
			for _, activeChan := range p.activeChannels {
				snapshot := activeChan.StateSnapshot()
				snapshots = append(snapshots, snapshot)
			}
			p.activeChanMtx.RUnlock()
			req.resp <- snapshots

		case pendingChanPoint := <-p.barrierInits:
			// A new channel has almost finished the funding
			// process. In order to properly synchronize with the
			// writeHandler goroutine, we add a new channel to the
			// barriers map which will be closed once the channel
			// is fully open.
			p.barrierMtx.Lock()
			peerLog.Tracef("Creating chan barrier for "+
				"ChannelPoint(%v)", pendingChanPoint)
			p.newChanBarriers[pendingChanPoint] = make(chan struct{})
			p.barrierMtx.Unlock()

		case newChan := <-p.newChannels:
			chanPoint := *newChan.ChannelPoint()

			p.activeChanMtx.Lock()
			p.activeChannels[chanPoint] = newChan
			p.activeChanMtx.Unlock()

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			decoder := routing.NewSphinxDecoder(p.server.sphinx)
			manager := htlcswitch.NewHTLCManager(
				&htlcswitch.HTLCManagerConfig{
					Peer:          p,
					DecodeOnion:   decoder.Decode,
					SettledContracts: p.server.breachArbiter.settledContracts,
					DebugHTLC:     cfg.DebugHTLC,
					Registry:      p.server.invoices,
					Forward:       p.server.htlcSwitch.Forward,
				}, newChan)

			err := p.server.htlcSwitch.Add(manager)
			if err != nil {
				peerLog.Errorf("can't register new htlc "+
					"manager(%v) with peerId(%v)", chanPoint, p.id)
			}

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

// executeCooperativeClose executes the initial phase of a user-executed
// cooperative channel close. The channel state machine is transitioned to the
// closing phase, then our half of the closing witness is sent over to the
// remote peer.
func (p *peer) executeCooperativeClose(channel *lnwallet.LightningChannel) (*chainhash.Hash, error) {
	// Shift the channel state machine into a 'closing' state. This
	// generates a signature for the closing tx, as well as a txid of the
	// closing tx itself, allowing us to watch the network to determine
	// when the remote node broadcasts the fully signed closing
	// transaction.
	sig, txid, err := channel.InitCooperativeClose()
	if err != nil {
		return nil, err
	}

	chanPoint := channel.ChannelPoint()
	peerLog.Infof("Executing cooperative closure of "+
		"ChanPoint(%v) with peerID(%v), txid=%v", chanPoint, p.id, txid)

	// With our signature for the close tx generated, send the signature to
	// the remote peer instructing it to close this particular channel
	// point.
	// TODO(roasbeef): remove encoding redundancy
	closeSig, err := btcec.ParseSignature(sig, btcec.S256())
	if err != nil {
		return nil, err
	}
	closeReq := lnwire.NewCloseRequest(chanPoint, closeSig)
	p.queueMsg(closeReq, nil)

	return txid, nil
}

// handleLocalClose kicks-off the workflow to execute a cooperative or forced
// unilateral closure of the channel initiated by a local subsystem.
// TODO(roasbeef): if no more active channels with peer call Remove on connMgr
// with peerID
func (p *peer) handleLocalClose(req *closeChanReq) {
	var (
		err         error
		closingTxid *chainhash.Hash
	)

	p.activeChanMtx.RLock()
	channel := p.activeChannels[*req.chanPoint]
	p.activeChanMtx.RUnlock()

	switch req.closeType {
	// A type of CloseRegular indicates that the user has opted to close
	// out this channel on-chian, so we execute the cooperative channel
	// closre workflow.
	case closeRegular:
		closingTxid, err = p.executeCooperativeClose(channel)
		peerLog.Infof("Attempting cooperative close of "+
			"ChannelPoint(%v) with txid: %v", req.chanPoint,
			closingTxid)

	// A type of CloseBreach indicates that the counter-party has breached
	// the channel therefore we need to clean up our local state.
	case closeBreach:
		peerLog.Infof("ChannelPoint(%v) has been breached, wiping "+
			"channel", req.chanPoint)
		if err := p.WipeChannel(channel); err != nil {
			peerLog.Infof("Unable to wipe channel after detected "+
				"breach: %v", err)
			req.err <- err
			return
		}
		return
	}
	if err != nil {
		req.err <- err
		return
	}

	// Update the caller with a new event detailing the current pending
	// state of this request.
	req.updates <- &lnrpc.CloseStatusUpdate{
		Update: &lnrpc.CloseStatusUpdate_ClosePending{
			ClosePending: &lnrpc.PendingUpdate{
				Txid: closingTxid[:],
			},
		},
	}

	// Finally, launch a goroutine which will request to be notified by the
	// ChainNotifier once the closure transaction obtains a single
	// confirmation.
	go func() {
		// TODO(roasbeef): add param for num needed confs
		notifier := p.server.chainNotifier
		confNtfn, err := notifier.RegisterConfirmationsNtfn(closingTxid, 1)
		if err != nil {
			req.err <- err
			return
		}

		select {
		case height, ok := <-confNtfn.Confirmed:
			// In the case that the ChainNotifier is shutting down,
			// all subscriber notification channels will be closed,
			// generating a nil receive.
			if !ok {
				return
			}

			// The channel has been closed, remove it from any
			// active indexes, and the database state.
			peerLog.Infof("ChannelPoint(%v) is now "+
				"closed at height %v", req.chanPoint, height)
			if err := p.WipeChannel(channel); err != nil {
				req.err <- err
				return
			}
		case <-p.quit:
			return
		}

		// Respond to the local subsystem which requested the channel
		// closure.
		req.updates <- &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ChanClose{
				ChanClose: &lnrpc.ChannelCloseUpdate{
					ClosingTxid: closingTxid[:],
					Success:     true,
				},
			},
		}

		p.server.breachArbiter.settledContracts <- req.chanPoint
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

	p.activeChanMtx.RLock()
	channel := p.activeChannels[key]
	p.activeChanMtx.RUnlock()

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

	peerLog.Infof("Broadcasting cooperative close tx: %v",
		newLogClosure(func() string {
			return spew.Sdump(closeTx)
		}))

	// Finally, broadcast the closure transaction, to the network.
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
	if err := p.WipeChannel(channel); err != nil {
		peerLog.Errorf("unable to wipe channel: %v", err)
	}

	p.server.breachArbiter.settledContracts <- req.ChannelPoint
}

// WipeChannel removes the passed channel from all indexes associated with the
// peer, and deletes the channel from the database.
func (p *peer) WipeChannel(channel *lnwallet.LightningChannel) error {
	chanPoint := channel.ChannelPoint()

	p.activeChanMtx.Lock()
	delete(p.activeChannels, *chanPoint)
	p.activeChanMtx.Unlock()

	// Instruct the Htlc Switch to close this link as the channel is no
	// longer active.
	if err := p.server.htlcSwitch.RemoveByChan(chanPoint); err != nil {
		if err == htlcswitch.ErrHTLCManagerNotFound {
			peerLog.Warnf("Unable remove htlc manager with "+
				"ChannelPoint(%v): %v", chanPoint, err)
			return nil
		}
		return err
	}

	// Finally, we purge the channel's state from the database, leaving a
	// small summary for historical records.
	if err := channel.DeleteState(); err != nil {
		peerLog.Errorf("Unable to delete ChannelPoint(%v) "+
			"from db: %v", chanPoint, err)
		return err
	}

	return nil
}

func (p *peer) SendMessage(msg lnwire.Message) error {
	p.queueMsg(msg, nil)
	return nil
}

func (p *peer) ID() [sha256.Size]byte {
	data := p.addr.IdentityKey.SerializeCompressed()
	return fastsha256.Sum256(data)
}

func (p *peer) HopID() *routing.HopID {
	pubKey := p.addr.IdentityKey.SerializeCompressed()
	return routing.NewHopID(pubKey)
}

// TODO(roasbeef): make all start/stop mutexes a CAS
