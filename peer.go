package main

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/connmgr"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
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
	// The following fields are only meant to be used *atomically*
	bytesReceived uint64
	bytesSent     uint64

	// pingTime is a rough estimate of the RTT (round-trip-time) between us
	// and the connected peer. This time is expressed in micro seconds.
	// TODO(roasbeef): also use a WMA or EMA?
	pingTime int64

	// pingLastSend is the Unix time expressed in nanoseconds when we sent
	// our last ping message.
	pingLastSend int64

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

	// For purposes of detecting retransmits, etc.
	lastNMessages map[lnwire.Message]struct{}

	// This mutex protects all the stats below it.
	sync.RWMutex
	timeConnected time.Time
	lastSend      time.Time
	lastRecv      time.Time

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

	htlcManMtx   sync.RWMutex
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

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *closeLinkReq

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
		htlcManagers:     make(map[wire.OutPoint]chan lnwire.Message),
		chanSnapshotReqs: make(chan *chanSnapshotReq),
		newChannels:      make(chan *lnwallet.LightningChannel, 1),

		localCloseChanReqs:  make(chan *closeLinkReq),
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

		// Register this new channel link with the HTLC Switch. This is
		// necessary to properly route multi-hop payments, and forward
		// new payments triggered by RPC clients.
		downstreamLink := make(chan *htlcPacket, 10)
		plexChan := p.server.htlcSwitch.RegisterLink(p,
			dbChan.Snapshot(), downstreamLink)

		upstreamLink := make(chan lnwire.Message, 10)
		p.htlcManMtx.Lock()
		p.htlcManagers[chanPoint] = upstreamLink
		p.htlcManMtx.Unlock()

		p.wg.Add(1)
		go p.htlcManager(lnChan, plexChan, downstreamLink, upstreamLink)
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
		// peer. Passing nil as the target link indicates that all
		// links associated with this interface should be closed.
		p.server.htlcSwitch.UnregisterLink(p.addr.IdentityKey, nil)

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
	p.logWireMessage(nextMsg, true)

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
			p.htlcManMtx.Lock()
			channel, ok := p.htlcManagers[*targetChan]
			p.htlcManMtx.Unlock()
			if !ok {
				peerLog.Errorf("recv'd update for unknown "+
					"channel %v from %v", targetChan, p)
				continue
			}
			channel <- nextMsg
		}
	}

	p.Disconnect()

	p.wg.Done()
	peerLog.Tracef("readHandler for peer %v done", p)
}

// logWireMessage logs the receipt or sending of particular wire message. This
// function is used rather than just logging the message in order to produce
// less spammy log messages in trace mode by setting the 'Curve" parameter to
// nil. Doing this avoids printing out each of the field elements in the curve
// parameters for secp256k1.
func (p *peer) logWireMessage(msg lnwire.Message, read bool) {
	switch m := msg.(type) {
	case *lnwire.CommitRevocation:
		m.NextRevocationKey.Curve = nil
	case *lnwire.NodeAnnouncement:
		m.NodeID.Curve = nil
	case *lnwire.ChannelAnnouncement:
		m.FirstNodeID.Curve = nil
		m.SecondNodeID.Curve = nil
		m.FirstBitcoinKey.Curve = nil
		m.SecondBitcoinKey.Curve = nil
	case *lnwire.SingleFundingComplete:
		m.RevocationKey.Curve = nil
	case *lnwire.SingleFundingRequest:
		m.CommitmentKey.Curve = nil
		m.ChannelDerivationPoint.Curve = nil
	case *lnwire.SingleFundingResponse:
		m.ChannelDerivationPoint.Curve = nil
		m.CommitmentKey.Curve = nil
		m.RevocationKey.Curve = nil
	}

	prefix := "readMessage from"
	if !read {
		prefix = "writeMessage to"
	}

	peerLog.Tracef(prefix+" %v: %v", p, newLogClosure(func() string {
		return spew.Sdump(msg)
	}))
}

// writeMessage writes the target lnwire.Message to the remote peer.
func (p *peer) writeMessage(msg lnwire.Message) error {
	// Simply exit if we're shutting down.
	if atomic.LoadInt32(&p.disconnect) != 0 {
		return nil
	}

	// TODO(roasbeef): add message summaries
	p.logWireMessage(msg, false)

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
	defer func() {
		p.wg.Done()
		peerLog.Tracef("writeHandler for peer %v done", p)
	}()

	for {
		select {
		case outMsg := <-p.sendQueue:
			switch outMsg.msg.(type) {
			// If we're about to send a ping message, then log the
			// exact time in which we send the message so we can
			// use the delay as a rough estimate of latency to the
			// remote peer.
			case *lnwire.Ping:
				// TODO(roasbeef): do this before the write?
				// possibly account for processing within func?
				now := time.Now().UnixNano()
				atomic.StoreInt64(&p.pingLastSend, now)
			}

			// Write out the message to the socket, closing the
			// 'sentChan' if it's non-nil, The 'sentChan' allows
			// callers to optionally synchronize sends with the
			// writeHandler.
			err := p.writeMessage(outMsg.msg)
			if outMsg.sentChan != nil {
				close(outMsg.sentChan)
			}

			if err != nil {
				peerLog.Errorf("unable to write message: %v",
					err)
				p.Disconnect()
				return
			}

		case <-p.quit:
			return
		}
	}
}

// queueHandler is responsible for accepting messages from outside subsystems
// to be eventually sent out on the wire by the writeHandler.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) queueHandler() {
	defer p.wg.Done()

	pendingMsgs := list.New()
	for {
		// Before add a queue'd message our pending message queue,
		// we'll first try to aggressively empty out our pending list of
		// messaging.
		for {
			// Examine the front of the queue. If this message is
			// nil, then we've emptied out the queue and can accept
			// new messages from outside sub-systems.
			elem := pendingMsgs.Front()
			if elem == nil {
				break
			}

			select {
			case p.sendQueue <- elem.Value.(outgoinMsg):
				pendingMsgs.Remove(elem)
			case <-p.quit:
				return
			default:
				break
			}
		}

		// If there weren't any messages to send, or the writehandler
		// is still blocked, then we'll accept a new message into the
		// queue from outside sub-systems.
		select {
		case <-p.quit:
			return
		case msg := <-p.outgoingQueue:
			pendingMsgs.PushBack(msg)
		}

	}
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

			// Now that the channel is open, notify the Htlc
			// Switch of a new active link.
			chanSnapShot := newChan.StateSnapshot()
			downstreamLink := make(chan *htlcPacket, 10)
			plexChan := p.server.htlcSwitch.RegisterLink(p,
				chanSnapShot, downstreamLink)

			// With the channel registered to the HtlcSwitch spawn
			// a goroutine to handle commitment updates for this
			// new channel.
			upstreamLink := make(chan lnwire.Message, 10)
			p.htlcManMtx.Lock()
			p.htlcManagers[chanPoint] = upstreamLink
			p.htlcManMtx.Unlock()

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
func (p *peer) handleLocalClose(req *closeLinkReq) {
	var (
		err         error
		closingTxid *chainhash.Hash
	)

	p.activeChanMtx.RLock()
	channel := p.activeChannels[*req.chanPoint]
	p.activeChanMtx.RUnlock()

	switch req.CloseType {
	// A type of CloseRegular indicates that the user has opted to close
	// out this channel on-chian, so we execute the cooperative channel
	// closre workflow.
	case CloseRegular:
		closingTxid, err = p.executeCooperativeClose(channel)
		peerLog.Infof("Attempting cooperative close of "+
			"ChannelPoint(%v) with txid: %v", req.chanPoint,
			closingTxid)

	// A type of CloseBreach indicates that the counterparty has breached
	// the cahnnel therefore we need to clean up our local state.
	case CloseBreach:
		peerLog.Infof("ChannelPoint(%v) has been breached, wiping "+
			"channel", req.chanPoint)
		if err := wipeChannel(p, channel); err != nil {
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
			if err := wipeChannel(p, channel); err != nil {
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
	if err := wipeChannel(p, channel); err != nil {
		peerLog.Errorf("unable to wipe channel: %v", err)
	}

	p.server.breachArbiter.settledContracts <- req.ChannelPoint
}

// wipeChannel removes the passed channel from all indexes associated with the
// peer, and deletes the channel from the database.
func wipeChannel(p *peer, channel *lnwallet.LightningChannel) error {
	chanID := channel.ChannelPoint()

	p.activeChanMtx.Lock()
	delete(p.activeChannels, *chanID)
	p.activeChanMtx.Unlock()

	// Instruct the Htlc Switch to close this link as the channel is no
	// longer active.
	p.server.htlcSwitch.UnregisterLink(p.addr.IdentityKey, chanID)

	// Additionally, close up "down stream" link for the htlcManager which
	// has been assigned to this channel. This servers the link between the
	// htlcManager and the switch, signalling that the channel is no longer
	// active.
	p.htlcManMtx.RLock()

	// If the channel can't be found in the map, then this channel has
	// already been wiped.
	htlcWireLink, ok := p.htlcManagers[*chanID]
	if !ok {
		p.htlcManMtx.RUnlock()
		return nil
	}

	close(htlcWireLink)

	p.htlcManMtx.RUnlock()

	// Next, we remove the htlcManager from our internal map as the
	// goroutine should have exited gracefully due to the channel closure
	// above.
	p.htlcManMtx.RLock()
	delete(p.htlcManagers, *chanID)
	p.htlcManMtx.RUnlock()

	// Finally, we purge the channel's state from the database, leaving a
	// small summary for historical records.
	if err := channel.DeleteState(); err != nil {
		peerLog.Errorf("Unable to delete ChannelPoint(%v) "+
			"from db: %v", chanID, err)
		return err
	}

	return nil
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
	// htlcsToSettle is a list of preimages which allow us to settle one or
	// many of the pending HTLCs we've received from the upstream peer.
	htlcsToSettle map[uint32]*channeldb.Invoice

	// htlcsToCancel is a set of HTLCs identified by their log index which
	// are to be cancelled upon the next state transition.
	htlcsToCancel map[uint32]lnwire.CancelReason

	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	cancelReasons map[uint32]lnwire.CancelReason

	// TODO(roasbeef): use once trickle+batch logic is in
	pendingBatch []*pendingPayment

	// clearedHTCLs is a map of outgoing HTLCs we've committed to in our
	// chain which have not yet been settled by the upstream peer.
	clearedHTCLs map[uint32]*pendingPayment

	// numUnAcked is a counter tracking the number of unacked changes we've
	// sent. A change is acked once we receive a new update to our local
	// chain from the remote peer.
	numUnAcked uint32

	// logCommitTimer is a timer which is sent upon if we go an interval
	// without receiving/sending a commitment update. It's role is to
	// ensure both chains converge to identical state in a timely manner.
	// TODO(roasbeef): timer should be >> then RTT
	logCommitTimer <-chan time.Time

	// switchChan is a channel used to send packets to the htlc switch for
	// forwarding.
	switchChan chan<- *htlcPacket

	// sphinx is an instance of the Sphinx onion Router for this node. The
	// router will be used to process all incoming Sphinx packets embedded
	// within HTLC add messages.
	sphinx *sphinx.Router

	// pendingCircuits tracks the remote log index of the incoming HTLCs,
	// mapped to the processed Sphinx packet contained within the HTLC.
	// This map is used as a staging area between when an HTLC is added to
	// the log, and when it's locked into the commitment state of both
	// chains. Once locked in, the processed packet is sent to the switch
	// along with the HTLC to forward the packet to the next hop.
	pendingCircuits map[uint32]*sphinx.ProcessedPacket

	channel   *lnwallet.LightningChannel
	chanPoint *wire.OutPoint
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// The htlcManager reads messages from the upstream (remote) peer, and also
// from several possible downstream channels managed by the htlcSwitch. In the
// event that an htlc needs to be forwarded, then send-only htlcPlex chan is
// used which sends htlc packets to the switch for forwarding. Additionally,
// the htlcManager handles acting upon all timeouts for any active HTLCs,
// manages the channel's revocation window, and also the htlc trickle
// queue+timer for this active channels.
func (p *peer) htlcManager(channel *lnwallet.LightningChannel,
	htlcPlex chan<- *htlcPacket, downstreamLink <-chan *htlcPacket,
	upstreamLink <-chan lnwire.Message) {

	chanStats := channel.StateSnapshot()
	peerLog.Infof("HTLC manager for ChannelPoint(%v) started, "+
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
		channel:         channel,
		chanPoint:       channel.ChannelPoint(),
		clearedHTCLs:    make(map[uint32]*pendingPayment),
		htlcsToSettle:   make(map[uint32]*channeldb.Invoice),
		htlcsToCancel:   make(map[uint32]lnwire.CancelReason),
		cancelReasons:   make(map[uint32]lnwire.CancelReason),
		pendingCircuits: make(map[uint32]*sphinx.ProcessedPacket),
		sphinx:          p.server.sphinx,
		switchChan:      htlcPlex,
	}

	// TODO(roasbeef): check to see if able to settle any currently pending
	// HTLCs
	//   * also need signals when new invoices are added by the invoiceRegistry

	batchTimer := time.Tick(10 * time.Millisecond)
out:
	for {
		select {
		case <-channel.UnilateralCloseSignal:
			// TODO(roasbeef): need to send HTLC outputs to nursery
			peerLog.Warnf("Remote peer has closed ChannelPoint(%v) on-chain",
				state.chanPoint)
			if err := wipeChannel(p, channel); err != nil {
				peerLog.Errorf("unable to wipe channel %v", err)
			}

			p.server.breachArbiter.settledContracts <- state.chanPoint

			break out
		case <-channel.ForceCloseSignal:
			peerLog.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peerID(%x)",
				state.chanPoint, p.id)
			break out
		// TODO(roasbeef): prevent leaking ticker?
		case <-state.logCommitTimer:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit. If so, then send
			// an update incrementing the unacked counter is
			// successfully.
			if !state.channel.PendingUpdates() &&
				len(state.htlcsToSettle) == 0 {
				continue
			}

			if sent, err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				p.Disconnect()
				break out
			} else if sent {
				state.numUnAcked += 1
			}
		case <-batchTimer:
			// If the current batch is empty, then we have no work
			// here.
			if len(state.pendingBatch) == 0 {
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if sent, err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				p.Disconnect()
				break out
			} else if !sent {
				continue
			}

			state.numUnAcked += 1
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
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
func (p *peer) handleDownStreamPkt(state *commitmentState, pkt *htlcPacket) {
	var isSettle bool
	switch htlc := pkt.msg.(type) {
	case *lnwire.HTLCAddRequest:
		// A new payment has been initiated via the
		// downstream channel, so we add the new HTLC
		// to our local log, then update the commitment
		// chains.
		htlc.ChannelPoint = state.chanPoint
		index, err := state.channel.AddHTLC(htlc)
		if err != nil {
			// TODO: possibly perform fallback/retry logic
			// depending on type of error
			// TODO: send a cancel message back to the htlcSwitch.
			peerLog.Errorf("Adding HTLC rejected: %v", err)
			pkt.err <- err

			// Increase the available bandwidth of the link,
			// previously it was decremented and because
			// HTLC adding failed we should do the reverse
			// operation.
			htlcSwitch := p.server.htlcSwitch
			htlcSwitch.UpdateLink(htlc.ChannelPoint, pkt.amt)
			return
		}

		p.queueMsg(htlc, nil)

		state.pendingBatch = append(state.pendingBatch, &pendingPayment{
			htlc:  htlc,
			index: index,
			err:   pkt.err,
		})

	case *lnwire.HTLCSettleRequest:
		// An HTLC we forward to the switch has just settle somehere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		pre := htlc.RedemptionProofs[0]
		logIndex, err := state.channel.SettleHTLC(pre)
		if err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for incoming HTLC rejected: %v", err)
			p.Disconnect()
			return
		}

		// With the HTLC settled, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled.
		htlc.ChannelPoint = state.chanPoint
		htlc.HTLCKey = lnwire.HTLCKey(logIndex)

		// Then we send the HTLC settle message to the connected peer
		// so we can continue the propagation of the settle message.
		p.queueMsg(htlc, nil)
		isSettle = true

	case *lnwire.CancelHTLC:
		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		logIndex, err := state.channel.CancelHTLC(pkt.payHash)
		if err != nil {
			peerLog.Errorf("unable to cancel HTLC: %v", err)
			return
		}

		// With the HTLC removed, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled. The "Reason" field will have already been set
		// within the switch.
		htlc.ChannelPoint = state.chanPoint
		htlc.HTLCKey = lnwire.HTLCKey(logIndex)

		// Finally, we send the HTLC message to the peer which
		// initially created the HTLC.
		p.queueMsg(htlc, nil)
		isSettle = true
	}

	// If this newly added update exceeds the max batch size for adds, or
	// this is a settle request, then initiate an update.
	// TODO(roasbeef): enforce max HTLCs in flight limit
	if len(state.pendingBatch) >= 10 || isSettle {
		if sent, err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update "+
				"commitment: %v", err)
			p.Disconnect()
			return
		} else if !sent {
			return
		}

		state.numUnAcked += 1
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (p *peer) handleUpstreamMsg(state *commitmentState, msg lnwire.Message) {
	switch htlcPkt := msg.(type) {
	// TODO(roasbeef): timeouts
	//  * fail if can't parse sphinx mix-header
	case *lnwire.HTLCAddRequest:
		// Before adding the new HTLC to the state machine, parse the
		// onion object in order to obtain the routing information.
		blobReader := bytes.NewReader(htlcPkt.OnionBlob)
		onionPkt := &sphinx.OnionPacket{}
		if err := onionPkt.Decode(blobReader); err != nil {
			peerLog.Errorf("unable to decode onion pkt: %v", err)
			p.Disconnect()
			return
		}

		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the preimage
		index, err := state.channel.ReceiveHTLC(htlcPkt)
		if err != nil {
			peerLog.Errorf("Receiving HTLC rejected: %v", err)
			return
		}

		// TODO(roasbeef): perform sanity checks on per-hop payload
		//  * time-lock is sane, fee, chain, etc

		// Attempt to process the Sphinx packet. We include the payment
		// hash of the HTLC as it's authenticated within the Sphinx
		// packet itself as associated data in order to thwart attempts
		// a replay attacks. In the case of a replay, an attacker is
		// *forced* to use the same payment hash twice, thereby losing
		// their money entirely.
		rHash := htlcPkt.RedemptionHashes[0][:]
		sphinxPacket, err := state.sphinx.ProcessOnionPacket(onionPkt, rHash)
		if err != nil {
			// If we're unable to parse the Sphinx packet, then
			// we'll cancel the HTLC after the current commitment
			// transition.
			peerLog.Errorf("unable to process onion pkt: %v", err)
			state.htlcsToCancel[index] = lnwire.SphinxParseError
			return
		}

		switch sphinxPacket.Action {
		// We're the designated payment destination. Therefore we
		// attempt to see if we have an invoice locally which'll allow
		// us to settle this HTLC.
		case sphinx.ExitNode:
			rHash := htlcPkt.RedemptionHashes[0]
			invoice, err := p.server.invoices.LookupInvoice(rHash)
			if err != nil {
				// If we're the exit node, but don't recognize
				// the payment hash, then we'll fail the HTLC
				// on the next state transition.
				peerLog.Errorf("unable to settle HTLC, "+
					"payment hash (%x) unrecognized", rHash[:])
				state.htlcsToCancel[index] = lnwire.UnknownPaymentHash
				return
			}

			// If we're not currently in debug mode, and the
			// extended HTLC doesn't meet the value requested, then
			// we'll fail the HTLC.
			if !cfg.DebugHTLC && htlcPkt.Amount < invoice.Terms.Value {
				peerLog.Errorf("rejecting HTLC due to incorrect "+
					"amount: expected %v, received %v",
					invoice.Terms.Value, htlcPkt.Amount)
				state.htlcsToCancel[index] = lnwire.IncorrectValue
			} else {
				// Otherwise, everything is in order and we'll
				// settle the HTLC after the current state
				// transition.
				state.htlcsToSettle[index] = invoice
			}

		// There are additional hops left within this route, so we
		// track the next hop according to the index of this HTLC
		// within their log. When forwarding locked-in HLTC's to the
		// switch, we'll attach the routing information so the switch
		// can finalize the circuit.
		case sphinx.MoreHops:
			state.pendingCircuits[index] = sphinxPacket
		default:
			peerLog.Errorf("mal formed onion packet")
			state.htlcsToCancel[index] = lnwire.SphinxParseError
		}
	case *lnwire.HTLCSettleRequest:
		pre := htlcPkt.RedemptionProofs[0]
		idx := uint32(htlcPkt.HTLCKey)
		if err := state.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for outgoing HTLC rejected: %v", err)
			p.Disconnect()
			return
		}

		// TODO(roasbeef): add preimage to DB in order to swipe
		// repeated r-values
	case *lnwire.CancelHTLC:
		idx := uint32(htlcPkt.HTLCKey)
		if err := state.channel.ReceiveCancelHTLC(idx); err != nil {
			peerLog.Errorf("unable to recv HTLC cancel: %v", err)
			p.Disconnect()
			return
		}

		state.cancelReasons[idx] = htlcPkt.Reason

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

		if state.numUnAcked > 0 {
			state.numUnAcked -= 1
			// TODO(roasbeef): only start if numUnacked == 0?
			state.logCommitTimer = time.Tick(300 * time.Millisecond)
		} else {
			if _, err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				p.Disconnect()
				return
			}
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

		// If any of the HTLCs eligible for forwarding are pending
		// settling or timing out previous outgoing payments, then we
		// can them from the pending set, and signal the requester (if
		// existing) that the payment has been fully fulfilled.
		var bandwidthUpdate btcutil.Amount
		settledPayments := make(map[lnwallet.PaymentHash]struct{})
		cancelledHtlcs := make(map[uint32]struct{})
		for _, htlc := range htlcsToForward {
			parentIndex := htlc.ParentIndex
			if p, ok := state.clearedHTCLs[parentIndex]; ok {
				switch htlc.EntryType {
				// If the HTLC was settled successfully, then
				// we return a nil error back to the possible
				// caller.
				case lnwallet.Settle:
					p.err <- nil

				// Otherwise, the HTLC failed, so we propagate
				// the error back to the potential caller.
				case lnwallet.Cancel:
					errMsg := state.cancelReasons[parentIndex]
					p.err <- errors.New(errMsg.String())
				}

				delete(state.clearedHTCLs, htlc.ParentIndex)
			}

			// TODO(roasbeef): rework log entries to a shared
			// interface.
			if htlc.EntryType != lnwallet.Add {
				continue
			}

			// If we can settle this HTLC within our local state
			// update log, then send the update entry to the remote
			// party.
			invoice, ok := state.htlcsToSettle[htlc.Index]
			if ok {
				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := state.channel.SettleHTLC(preimage)
				if err != nil {
					peerLog.Errorf("unable to settle htlc: %v", err)
					p.Disconnect()
					continue
				}

				settleMsg := &lnwire.HTLCSettleRequest{
					ChannelPoint:     state.chanPoint,
					HTLCKey:          lnwire.HTLCKey(logIndex),
					RedemptionProofs: [][32]byte{preimage},
				}
				p.queueMsg(settleMsg, nil)

				delete(state.htlcsToSettle, htlc.Index)
				settledPayments[htlc.RHash] = struct{}{}

				bandwidthUpdate += htlc.Amount
				continue
			}

			// Alternatively, if we marked this HTLC for
			// cancellation, then immediately cancel the HTLC as
			// it's now locked in within both commitment
			// transactions.
			reason, ok := state.htlcsToCancel[htlc.Index]
			if !ok {
				continue
			}

			logIndex, err := state.channel.CancelHTLC(htlc.RHash)
			if err != nil {
				peerLog.Errorf("unable to cancel htlc: %v", err)
				p.Disconnect()
				continue
			}

			cancelMsg := &lnwire.CancelHTLC{
				ChannelPoint: state.chanPoint,
				HTLCKey:      lnwire.HTLCKey(logIndex),
				Reason:       reason,
			}
			p.queueMsg(cancelMsg, nil)
			delete(state.htlcsToCancel, htlc.Index)

			cancelledHtlcs[htlc.Index] = struct{}{}
		}

		go func() {
			for _, htlc := range htlcsToForward {
				// We don't need to forward any HTLCs that we
				// just settled or cancelled above.
				// TODO(roasbeef): key by index instead?
				if _, ok := settledPayments[htlc.RHash]; ok {
					continue
				}
				if _, ok := cancelledHtlcs[htlc.Index]; ok {
					continue
				}

				onionPkt := state.pendingCircuits[htlc.Index]
				delete(state.pendingCircuits, htlc.Index)

				reason := state.cancelReasons[htlc.ParentIndex]
				delete(state.cancelReasons, htlc.ParentIndex)

				// Send this fully activated HTLC to the htlc
				// switch to continue the chained clear/settle.
				pkt, err := logEntryToHtlcPkt(*state.chanPoint,
					htlc, onionPkt, reason)
				if err != nil {
					peerLog.Errorf("unable to make htlc pkt: %v",
						err)
					continue
				}

				state.switchChan <- pkt
			}

		}()

		if len(settledPayments) == 0 && len(cancelledHtlcs) == 0 {
			return
		}

		// Send an update to the htlc switch of our newly available
		// payment bandwidth.
		// TODO(roasbeef): ideally should wait for next state update.
		if bandwidthUpdate != 0 {
			p.server.htlcSwitch.UpdateLink(state.chanPoint,
				bandwidthUpdate)
		}

		// With all the settle updates added to the local and remote
		// HTLC logs, initiate a state transition by updating the
		// remote commitment chain.
		if sent, err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update commitment: %v", err)
			p.Disconnect()
			return
		} else if sent {
			// TODO(roasbeef): wait to delete from htlcsToSettle?
			state.numUnAcked += 1
		}

		// Notify the invoiceRegistry of the invoices we just settled
		// with this latest commitment update.
		// TODO(roasbeef): wait until next transition?
		for invoice, _ := range settledPayments {
			err := p.server.invoices.SettleInvoice(chainhash.Hash(invoice))
			if err != nil {
				peerLog.Errorf("unable to settle invoice: %v", err)
			}
		}
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (p *peer) updateCommitTx(state *commitmentState) (bool, error) {
	sigTheirs, logIndexTheirs, err := state.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		peerLog.Tracef("revocation window exhausted, unable to send %v",
			len(state.pendingBatch))
		return false, nil
	} else if err != nil {
		return false, err
	}

	parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
	if err != nil {
		return false, fmt.Errorf("unable to parse sig: %v", err)
	}

	commitSig := &lnwire.CommitSignature{
		ChannelPoint: state.chanPoint,
		CommitSig:    parsedSig,
		LogIndex:     uint64(logIndexTheirs),
	}
	p.queueMsg(commitSig, nil)

	// Move all pending updates to the map of cleared HTLCs, clearing out
	// the set of pending updates.
	for _, update := range state.pendingBatch {
		// TODO(roasbeef): add parsed next-hop info to pending batch
		// for multi-hop forwarding
		state.clearedHTCLs[update.index] = update
	}
	state.logCommitTimer = nil
	state.pendingBatch = nil

	return true, nil
}

// logEntryToHtlcPkt converts a particular Lightning Commitment Protocol (LCP)
// log entry the corresponding htlcPacket with src/dest set along with the
// proper wire message. This helper method is provided in order to aid an
// htlcManager in forwarding packets to the htlcSwitch.
func logEntryToHtlcPkt(chanPoint wire.OutPoint,
	pd *lnwallet.PaymentDescriptor,
	onionPkt *sphinx.ProcessedPacket,
	reason lnwire.CancelReason) (*htlcPacket, error) {

	pkt := &htlcPacket{}

	// TODO(roasbeef): alter after switch to log entry interface
	var msg lnwire.Message
	switch pd.EntryType {
	case lnwallet.Add:
		// TODO(roasbeef): timeout, onion blob, etc
		var b bytes.Buffer
		if err := onionPkt.Packet.Encode(&b); err != nil {
			return nil, err
		}

		msg = &lnwire.HTLCAddRequest{
			Amount:           btcutil.Amount(pd.Amount),
			RedemptionHashes: [][32]byte{pd.RHash},
			OnionBlob:        b.Bytes(),
		}
	case lnwallet.Settle:
		msg = &lnwire.HTLCSettleRequest{
			RedemptionProofs: [][32]byte{pd.RPreimage},
		}
	case lnwallet.Cancel:
		// For cancellation messages, we'll also need to set the rHash
		// within the htlcPacket so the switch knows on which outbound
		// link to forward the cancellation message
		msg = &lnwire.CancelHTLC{
			Reason: reason,
		}
		pkt.payHash = pd.RHash
	}

	pkt.amt = pd.Amount
	pkt.msg = msg

	pkt.srcLink = chanPoint
	pkt.onion = onionPkt

	return pkt, nil
}

// TODO(roasbeef): make all start/stop mutexes a CAS
