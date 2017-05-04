package main

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

// newChannelMsg packages a lnwallet.LightningChannel with a channel that
// allows the receiver of the request to report when the funding transaction
// has been confirmed and the channel creation process completed.
type newChannelMsg struct {
	channel *lnwallet.LightningChannel
	done    chan struct{}
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
	disconnect int32

	connReq *connmgr.ConnReq
	conn    net.Conn

	addr        *lnwire.NetAddress
	lightningID chainhash.Hash

	inbound bool
	id      int32

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
	activeChannels   map[lnwire.ChannelID]*lnwallet.LightningChannel
	chanSnapshotReqs chan *chanSnapshotReq

	htlcManMtx   sync.RWMutex
	htlcManagers map[lnwire.ChannelID]chan lnwire.Message

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newChannels chan *newChannelMsg

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *closeLinkReq

	// remoteCloseChanReqs is a channel in which any remote requests
	// (initiated by the remote peer) close a particular channel are sent
	// over.
	remoteCloseChanReqs chan *lnwire.CloseRequest

	server *server

	// localSharedFeatures is a product of comparison of our and their
	// local features vectors which consist of features which are present
	// on both sides.
	localSharedFeatures *lnwire.SharedFeatures

	// globalSharedFeatures is a product of comparison of our and their
	// global features vectors which consist of features which are present
	// on both sides.
	globalSharedFeatures *lnwire.SharedFeatures

	queueQuit chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
}

// newPeer creates a new peer from an establish connection object, and a
// pointer to the main server.
func newPeer(conn net.Conn, connReq *connmgr.ConnReq, server *server,
	addr *lnwire.NetAddress, inbound bool) (*peer, error) {

	nodePub := addr.IdentityKey

	p := &peer{
		conn:        conn,
		lightningID: chainhash.Hash(sha256.Sum256(nodePub.SerializeCompressed())),
		addr:        addr,

		id:      atomic.AddInt32(&numNodes, 1),
		inbound: inbound,
		connReq: connReq,

		server: server,

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		activeChannels:   make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		htlcManagers:     make(map[lnwire.ChannelID]chan lnwire.Message),
		chanSnapshotReqs: make(chan *chanSnapshotReq),
		newChannels:      make(chan *newChannelMsg, 1),

		localCloseChanReqs:  make(chan *closeLinkReq),
		remoteCloseChanReqs: make(chan *lnwire.CloseRequest),

		localSharedFeatures:  nil,
		globalSharedFeatures: nil,

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
	}

	return p, nil
}

// Start starts all helper goroutines the peer needs for normal operations.  In
// the case this peer has already been started, then this function is a loop.
func (p *peer) Start() error {
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	peerLog.Tracef("peer %v starting", p)

	// Exchange local and global features, the init message should be very
	// first between two nodes.
	if err := p.sendInitMsg(); err != nil {
		return fmt.Errorf("unable to send init msg: %v", err)
	}

	// Before we launch any of the helper goroutines off the peer struct,
	// we'll first ensure proper adherence to the p2p protocol. The init
	// message MUST be sent before any other message.
	readErr := make(chan error, 1)
	msgChan := make(chan lnwire.Message, 1)
	go func() {
		msg, err := p.readNextMessage()
		if err != nil {
			readErr <- err
			msgChan <- nil
		}
		readErr <- nil
		msgChan <- msg
	}()

	select {
	// In order to avoid blocking indefinitely, we'll give the other peer
	// an upper timeout of 15 seconds to respond before we bail out early.
	case <-time.After(time.Second * 15):
		return fmt.Errorf("peer did not complete handshake within 5 " +
			"seconds")
	case err := <-readErr:
		if err != nil {
			return fmt.Errorf("unable to read init msg: %v", err)
		}
	}

	msg := <-msgChan
	if msg, ok := msg.(*lnwire.Init); ok {
		if err := p.handleInitMsg(msg); err != nil {
			return err
		}
	} else {
		return errors.New("very first message between nodes " +
			"must be init message")
	}

	p.wg.Add(5)
	go p.queueHandler()
	go p.writeHandler()
	go p.readHandler()
	go p.channelManager()
	go p.pingHandler()

	// Fetch and then load all the active channels we have with this remote
	// peer from the database.
	activeChans, err := p.server.chanDB.FetchOpenChannels(p.addr.IdentityKey)
	if err != nil {
		peerLog.Errorf("unable to fetch active chans "+
			"for peer %v: %v", p, err)
		return err
	}

	// Next, load all the active channels we have with this peer,
	// registering them with the switch and launching the necessary
	// goroutines required to operate them.
	peerLog.Debugf("Loaded %v active channels from database with "+
		"peerID(%v)", len(activeChans), p.id)
	if err := p.loadActiveChannels(activeChans); err != nil {
		return fmt.Errorf("unable to load channels: %v", err)
	}

	return nil
}

// loadActiveChannels creates indexes within the peer for tracking all active
// channels returned by the database.
func (p *peer) loadActiveChannels(chans []*channeldb.OpenChannel) error {
	for _, dbChan := range chans {
		// If the channel isn't yet open, then we don't need to process
		// it any further.
		if dbChan.IsPending {
			continue
		}

		lnChan, err := lnwallet.NewLightningChannel(p.server.lnwallet.Signer,
			p.server.chainNotifier, dbChan)
		if err != nil {
			return err
		}

		chanPoint := *dbChan.ChanID
		chanID := lnwire.NewChanIDFromOutPoint(&chanPoint)

		p.activeChanMtx.Lock()
		p.activeChannels[chanID] = lnChan
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
		p.htlcManagers[chanID] = upstreamLink
		p.htlcManMtx.Unlock()

		p.wg.Add(1)
		go p.htlcManager(lnChan, plexChan, downstreamLink, upstreamLink)
	}

	return nil
}

// WaitForDisconnect waits until the peer has disconnected. A peer may be
// disconnected if the local or remote side terminating the connection, or an
// irrecoverable protocol error has been encountered.
func (p *peer) WaitForDisconnect() {
	<-p.quit

}

// Disconnect terminates the connection with the remote peer. Additionally, a
// signal is sent to the server and htlcSwitch indicating the resources
// allocated to the peer can now be cleaned up.
func (p *peer) Disconnect() {
	if !atomic.CompareAndSwapInt32(&p.disconnect, 0, 1) {
		return
	}

	peerLog.Tracef("Disconnecting %s", p)

	// Ensure that the TCP connection is properly closed before continuing.
	p.conn.Close()

	close(p.quit)

}

// String returns the string representation of this peer.
func (p *peer) String() string {
	return p.conn.RemoteAddr().String()
}

// readNextMessage reads, and returns the next message on the wire along with
// any additional raw payload.
func (p *peer) readNextMessage() (lnwire.Message, error) {
	noiseConn, ok := p.conn.(*brontide.Conn)
	if !ok {
		return nil, fmt.Errorf("brontide.Conn required to read messages")
	}

	// First we'll read the next _full_ message. We do this rather than
	// reading incrementally from the stream as the Lightning wire protocol
	// is message oriented and allows nodes to pad on additional data to
	// the message stream.
	rawMsg, err := noiseConn.ReadNextMessage()
	atomic.AddUint64(&p.bytesReceived, uint64(len(rawMsg)))
	if err != nil {
		return nil, err
	}

	// Next, create a new io.Reader implementation from the raw message,
	// and use this to decode the message directly from.
	msgReader := bytes.NewReader(rawMsg)
	nextMsg, err := lnwire.ReadMessage(msgReader, 0)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): add message summaries
	p.logWireMessage(nextMsg, true)

	return nextMsg, nil
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper subsystem.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) readHandler() {
	var activeChanMtx sync.Mutex
	activeChanStreams := make(map[lnwire.ChannelID]struct{})

out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, err := p.readNextMessage()
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

		var (
			isChanUpdate bool
			targetChan   lnwire.ChannelID
		)

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
			pongBytes := make([]byte, msg.NumPongBytes)
			p.queueMsg(lnwire.NewPong(pongBytes), nil)

		case *lnwire.SingleFundingRequest:
			p.server.fundingMgr.processFundingRequest(msg, p.addr)
		case *lnwire.SingleFundingResponse:
			p.server.fundingMgr.processFundingResponse(msg, p.addr)
		case *lnwire.SingleFundingComplete:
			p.server.fundingMgr.processFundingComplete(msg, p.addr)
		case *lnwire.SingleFundingSignComplete:
			p.server.fundingMgr.processFundingSignComplete(msg, p.addr)
		case *lnwire.FundingLocked:
			p.server.fundingMgr.processFundingLocked(msg, p.addr)
		case *lnwire.CloseRequest:
			p.remoteCloseChanReqs <- msg

		case *lnwire.Error:
			p.server.fundingMgr.processFundingError(msg, p.addr)

		// TODO(roasbeef): create ChanUpdater interface for the below
		case *lnwire.UpdateAddHTLC:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.UpdateFufillHTLC:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.UpdateFailHTLC:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.RevokeAndAck:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.CommitSig:
			isChanUpdate = true
			targetChan = msg.ChanID

		case *lnwire.ChannelUpdate,
			*lnwire.ChannelAnnouncement,
			*lnwire.NodeAnnouncement,
			*lnwire.AnnounceSignatures:

			p.server.discoverSrv.ProcessRemoteAnnouncement(msg,
				p.addr.IdentityKey)
		default:
			peerLog.Errorf("unknown message received from peer "+
				"%v", p)
		}

		if isChanUpdate {
			sendUpdate := func() {
				// Dispatch the commitment update message to
				// the proper active goroutine dedicated to
				// this channel.
				p.htlcManMtx.RLock()
				channel, ok := p.htlcManagers[targetChan]
				p.htlcManMtx.RUnlock()
				if !ok {
					peerLog.Errorf("recv'd update for unknown "+
						"channel %v from %v", targetChan, p)
					return
				}

				channel <- nextMsg
			}

			// Check the map of active channel streams, if this map
			// has an entry, then this means the channel is fully
			// open. In this case, we can send the channel update
			// directly without any further waiting.
			activeChanMtx.Lock()
			_, ok := activeChanStreams[targetChan]
			activeChanMtx.Unlock()
			if ok {
				sendUpdate()
				continue
			}

			// Otherwise, we'll launch a goroutine to synchronize
			// the processing of this message, with the opening of
			// the channel as marked by the funding manage.
			go func() {
				// Block until the channel is marked open.
				p.server.fundingMgr.waitUntilChannelOpen(targetChan)

				// Once the channel is open, we'll mark the
				// stream as active and send the update to the
				// channel. Marking the stream lets us take the
				// fast path above, skipping the check to the
				// funding manager.
				activeChanMtx.Lock()
				activeChanStreams[targetChan] = struct{}{}
				sendUpdate()
				activeChanMtx.Unlock()
			}()
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
	case *lnwire.RevokeAndAck:
		m.NextRevocationKey.Curve = nil
	case *lnwire.NodeAnnouncement:
		m.NodeID.Curve = nil
	case *lnwire.ChannelAnnouncement:
		m.NodeID1.Curve = nil
		m.NodeID2.Curve = nil
		m.BitcoinKey1.Curve = nil
		m.BitcoinKey2.Curve = nil
	case *lnwire.SingleFundingComplete:
		m.RevocationKey.Curve = nil
	case *lnwire.SingleFundingRequest:
		m.CommitmentKey.Curve = nil
		m.ChannelDerivationPoint.Curve = nil
	case *lnwire.SingleFundingResponse:
		m.ChannelDerivationPoint.Curve = nil
		m.CommitmentKey.Curve = nil
		m.RevocationKey.Curve = nil
	case *lnwire.FundingLocked:
		m.NextPerCommitmentPoint.Curve = nil
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

	// As the Lightning wire protocol is fully message oriented, we only
	// allows one wire message per outer encapsulated crypto message. So
	// we'll create a temporary buffer to write the message directly to.
	var msgPayload [lnwire.MaxMessagePayload]byte
	b := bytes.NewBuffer(msgPayload[0:0:len(msgPayload)])

	// With the temp buffer created and sliced properly (length zero, full
	// capacity), we'll now encode the message directly into this buffer.
	n, err := lnwire.WriteMessage(b, msg, 0)
	atomic.AddUint64(&p.bytesSent, uint64(n))

	// Finally, write the message itself in a single swoop.
	_, err = p.conn.Write(b.Bytes())
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

	// TODO(roasbeef): make dynamic in order to create fake cover traffic
	const numPingBytes = 16

out:
	for {
		select {
		case <-pingTicker.C:
			p.queueMsg(lnwire.NewPing(numPingBytes), nil)
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
			snapshots := make([]*channeldb.ChannelSnapshot, 0,
				len(p.activeChannels))
			for _, activeChan := range p.activeChannels {
				snapshot := activeChan.StateSnapshot()
				snapshots = append(snapshots, snapshot)
			}
			p.activeChanMtx.RUnlock()
			req.resp <- snapshots

		case newChanReq := <-p.newChannels:
			chanPoint := newChanReq.channel.ChannelPoint()
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

			p.activeChanMtx.Lock()
			p.activeChannels[chanID] = newChanReq.channel
			p.activeChanMtx.Unlock()

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			// Now that the channel is open, notify the Htlc
			// Switch of a new active link.
			// TODO(roasbeef): register needs to account for
			// in-flight htlc's on restart
			chanSnapShot := newChanReq.channel.StateSnapshot()
			downstreamLink := make(chan *htlcPacket, 10)
			plexChan := p.server.htlcSwitch.RegisterLink(p,
				chanSnapShot, downstreamLink)

			// With the channel registered to the HtlcSwitch spawn
			// a goroutine to handle commitment updates for this
			// new channel.
			upstreamLink := make(chan lnwire.Message, 10)
			p.htlcManMtx.Lock()
			p.htlcManagers[chanID] = upstreamLink
			p.htlcManMtx.Unlock()

			p.wg.Add(1)
			go p.htlcManager(newChanReq.channel, plexChan,
				downstreamLink, upstreamLink)

			close(newChanReq.done)

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

	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	closeReq := lnwire.NewCloseRequest(chanID, closeSig)
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

	chanID := lnwire.NewChanIDFromOutPoint(req.chanPoint)

	p.activeChanMtx.RLock()
	channel := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()

	switch req.CloseType {
	// A type of CloseRegular indicates that the user has opted to close
	// out this channel on-chain, so we execute the cooperative channel
	// closure workflow.
	case CloseRegular:
		closingTxid, err = p.executeCooperativeClose(channel)
		peerLog.Infof("Attempting cooperative close of "+
			"ChannelPoint(%v) with txid: %v", req.chanPoint,
			closingTxid)

	// A type of CloseBreach indicates that the counterparty has breached
	// the channel therefore we need to clean up our local state.
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

	// Once we've completed the cooperative channel closure, we'll wipe the
	// channel so we reject any incoming forward or payment requests via
	// this channel.
	p.server.breachArbiter.settledContracts <- req.chanPoint
	if err := wipeChannel(p, channel); err != nil {
		req.err <- err
		return
	}

	// Clear out the current channel state, marking the channel as being
	// closed within the database.
	chanInfo := channel.StateSnapshot()
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:   *req.chanPoint,
		ClosingTXID: *closingTxid,
		RemotePub:   &chanInfo.RemoteIdentity,
		Capacity:    chanInfo.Capacity,
		OurBalance:  chanInfo.LocalBalance,
		CloseType:   channeldb.CooperativeClose,
		IsPending:   true,
	}
	if err := channel.DeleteState(closeSummary); err != nil {
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
	notifier := p.server.chainNotifier
	go waitForChanToClose(notifier, req.err, req.chanPoint, closingTxid, func() {
		// First, we'll mark the database as being fully closed so
		// we'll no longer watch for its ultimate closure upon startup.
		err := p.server.chanDB.MarkChanFullyClosed(req.chanPoint)
		if err != nil {
			req.err <- err
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
	})
}

// handleRemoteClose completes a request for cooperative channel closure
// initiated by the remote node.
func (p *peer) handleRemoteClose(req *lnwire.CloseRequest) {
	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[req.ChanID]
	p.activeChanMtx.RUnlock()
	if !ok {
		peerLog.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", req.ChanID)
		return
	}

	chanPoint := channel.ChannelPoint()

	// Now that we have their signature for the closure transaction, we can
	// assemble the final closure transaction, complete with our signature.
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
	err = p.server.lnwallet.PublishTransaction(closeTx)
	if err != nil && !strings.Contains(err.Error(), "already have") {
		peerLog.Errorf("channel close tx from "+
			"ChannelPoint(%v) rejected: %v",
			chanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		//  * remove check above to error
		return
	}

	p.server.breachArbiter.settledContracts <- chanPoint

	// We've just broadcast the transaction which closes the channel, so
	// we'll wipe the channel from all our local indexes and also signal to
	// the switch that this channel is now closed.
	peerLog.Infof("ChannelPoint(%v) is now closed", chanPoint)
	if err := wipeChannel(p, channel); err != nil {
		peerLog.Errorf("unable to wipe channel: %v", err)
	}

	// Clear out the current channel state, marking the channel as being
	// closed within the database.
	closeTxid := closeTx.TxHash()
	chanInfo := channel.StateSnapshot()
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:   *chanPoint,
		ClosingTXID: closeTxid,
		RemotePub:   &chanInfo.RemoteIdentity,
		Capacity:    chanInfo.Capacity,
		OurBalance:  chanInfo.LocalBalance,
		CloseType:   channeldb.CooperativeClose,
		IsPending:   true,
	}
	if err := channel.DeleteState(closeSummary); err != nil {
		peerLog.Errorf("unable to delete channel state: %v", err)
		return
	}

	// Finally, we'll launch a goroutine to watch the network for the
	// confirmation of the closing transaction, and mark the channel as
	// such within the database (once it's confirmed").
	notifier := p.server.chainNotifier
	go waitForChanToClose(notifier, nil, chanPoint, &closeTxid,
		func() {
			// Now that the closing transaction has been confirmed,
			// we'll mark the database as being fully closed so now
			// that we no longer watch for its ultimate closure
			// upon startup.
			err := p.server.chanDB.MarkChanFullyClosed(chanPoint)
			if err != nil {
				peerLog.Errorf("unable to mark channel as closed: %v", err)
				return
			}
		},
	)
}

// waitForChanToClose uses the passed notifier to wait until the channel has
// been detected as closed on chain and then concludes by executing the
// following actions: the channel point will be sent over the settleChan, and
// finally the callback will be executed. If any error is encountered within
// the function, then it will be sent over the errChan.
func waitForChanToClose(notifier chainntnfs.ChainNotifier,
	errChan chan error, chanPoint *wire.OutPoint,
	closingTxID *chainhash.Hash, cb func()) {

	// TODO(roasbeef): add param for num needed confs
	confNtfn, err := notifier.RegisterConfirmationsNtfn(closingTxID, 1)
	if err != nil && errChan != nil {
		errChan <- err
		return
	}

	// In the case that the ChainNotifier is shutting down, all subscriber
	// notification channels will be closed, generating a nil receive.
	height, ok := <-confNtfn.Confirmed
	if !ok {
		return
	}

	// The channel has been closed, remove it from any active indexes, and
	// the database state.
	srvrLog.Infof("ChannelPoint(%v) is now closed at "+
		"height %v", chanPoint, height.BlockHeight)

	// Finally, execute the closure call back to mark the confirmation of
	// the transaction closing the contract.
	cb()
}

// wipeChannel removes the passed channel from all indexes associated with the
// peer, and deletes the channel from the database.
func wipeChannel(p *peer, channel *lnwallet.LightningChannel) error {
	chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())

	p.activeChanMtx.Lock()
	delete(p.activeChannels, chanID)
	p.activeChanMtx.Unlock()

	// Instruct the Htlc Switch to close this link as the channel is no
	// longer active.
	p.server.htlcSwitch.UnregisterLink(p.addr.IdentityKey, &chanID)

	// Additionally, close up "down stream" link for the htlcManager which
	// has been assigned to this channel. This servers the link between the
	// htlcManager and the switch, signalling that the channel is no longer
	// active.
	p.htlcManMtx.RLock()

	// If the channel can't be found in the map, then this channel has
	// already been wiped.
	htlcWireLink, ok := p.htlcManagers[chanID]
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
	delete(p.htlcManagers, chanID)
	p.htlcManMtx.RUnlock()

	return nil
}

// pendingPayment represents a pending HTLC which has yet to be settled by the
// upstream peer. A pending payment encapsulates the initial HTLC add request
// additionally coupling the index of the HTLC within the log, and an error
// channel to signal the payment requester once the payment has been fully
// fufilled.
type pendingPayment struct {
	htlc  *lnwire.UpdateAddHTLC
	index uint64

	preImage chan [32]byte
	err      chan error
	done     chan struct{}
}

// commitmentState is the volatile+persistent state of an active channel's
// commitment update state-machine. This struct is used by htlcManager's to
// save meta-state required for proper functioning.
type commitmentState struct {
	// htlcsToSettle is a list of preimages which allow us to settle one or
	// many of the pending HTLCs we've received from the upstream peer.
	htlcsToSettle map[uint64]*channeldb.Invoice

	// htlcsToCancel is a set of HTLCs identified by their log index which
	// are to be cancelled upon the next state transition.
	htlcsToCancel map[uint64]lnwire.FailCode

	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	cancelReasons map[uint64]lnwire.FailCode

	// pendingBatch is slice of payments which have been added to the
	// channel update log, but not yet committed to latest commitment.
	pendingBatch []*pendingPayment

	// clearedHTCLs is a map of outgoing HTLCs we've committed to in our
	// chain which have not yet been settled by the upstream peer.
	clearedHTCLs map[uint64]*pendingPayment

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
	pendingCircuits map[uint64]*sphinx.ProcessedPacket

	channel   *lnwallet.LightningChannel
	chanPoint *wire.OutPoint
	chanID    lnwire.ChannelID
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

	chanPoint := channel.ChannelPoint()
	state := &commitmentState{
		channel:         channel,
		chanPoint:       chanPoint,
		chanID:          lnwire.NewChanIDFromOutPoint(chanPoint),
		clearedHTCLs:    make(map[uint64]*pendingPayment),
		htlcsToSettle:   make(map[uint64]*channeldb.Invoice),
		htlcsToCancel:   make(map[uint64]lnwire.FailCode),
		cancelReasons:   make(map[uint64]lnwire.FailCode),
		pendingCircuits: make(map[uint64]*sphinx.ProcessedPacket),
		sphinx:          p.server.sphinx,
		switchChan:      htlcPlex,
	}

	// TODO(roasbeef): check to see if able to settle any currently pending
	// HTLCs
	//   * also need signals when new invoices are added by the
	//   invoiceRegistry

	batchTimer := time.NewTicker(50 * time.Millisecond)
	defer batchTimer.Stop()

	logCommitTimer := time.NewTicker(100 * time.Millisecond)
	defer logCommitTimer.Stop()
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
			// TODO(roasbeef): path never taken now that server
			// force closes's directly?
			peerLog.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peerID(%x)",
				state.chanPoint, p.id)
			break out

		case <-logCommitTimer.C:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit due to our
			// commitment chains being desynchronized.
			if state.channel.FullySynced() &&
				len(state.htlcsToSettle) == 0 {
				continue
			}

			if err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update commitment: %v",
					err)
				p.Disconnect()
				break out
			}

		case <-batchTimer.C:
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
			if err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				p.Disconnect()
				break out
			}

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

// handleInitMsg handles the incoming init message which contains global and
// local features vectors. If feature vectors are incompatible then disconnect.
func (p *peer) handleInitMsg(msg *lnwire.Init) error {
	localSharedFeatures, err := p.server.localFeatures.Compare(msg.LocalFeatures)
	if err != nil {
		err := errors.Errorf("can't compare remote and local feature "+
			"vectors: %v", err)
		peerLog.Error(err)
		return err
	}
	p.localSharedFeatures = localSharedFeatures

	globalSharedFeatures, err := p.server.globalFeatures.Compare(msg.GlobalFeatures)
	if err != nil {
		err := errors.Errorf("can't compare remote and global feature "+
			"vectors: %v", err)
		peerLog.Error(err)
		return err
	}
	p.globalSharedFeatures = globalSharedFeatures

	return nil
}

// sendInitMsg sends init message to remote peer which contains our currently
// supported local and global features.
func (p *peer) sendInitMsg() error {
	msg := lnwire.NewInitMessage(
		p.server.globalFeatures,
		p.server.localFeatures,
	)

	return p.writeMessage(msg)
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
func (p *peer) handleDownStreamPkt(state *commitmentState, pkt *htlcPacket) {
	var isSettle bool
	switch htlc := pkt.msg.(type) {
	case *lnwire.UpdateAddHTLC:
		// A new payment has been initiated via the
		// downstream channel, so we add the new HTLC
		// to our local log, then update the commitment
		// chains.
		htlc.ChanID = state.chanID
		index, err := state.channel.AddHTLC(htlc)
		if err != nil {
			// TODO: possibly perform fallback/retry logic
			// depending on type of error
			peerLog.Errorf("Adding HTLC rejected: %v", err)
			pkt.err <- err
			close(pkt.done)

			// The HTLC was unable to be added to the state
			// machine, as a result, we'll signal the switch to
			// cancel the pending payment.
			// TODO(roasbeef): need to update link as well if local
			// HTLC?
			state.switchChan <- &htlcPacket{
				amt: htlc.Amount,
				msg: &lnwire.UpdateFailHTLC{
					Reason: []byte{byte(0)},
				},
				srcLink: state.chanID,
			}
			return
		}

		p.queueMsg(htlc, nil)

		state.pendingBatch = append(state.pendingBatch, &pendingPayment{
			htlc:     htlc,
			index:    index,
			preImage: pkt.preImage,
			err:      pkt.err,
			done:     pkt.done,
		})

	case *lnwire.UpdateFufillHTLC:
		// An HTLC we forward to the switch has just settled somewhere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		pre := htlc.PaymentPreimage
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
		htlc.ChanID = state.chanID
		htlc.ID = logIndex

		// Then we send the HTLC settle message to the connected peer
		// so we can continue the propagation of the settle message.
		p.queueMsg(htlc, nil)
		isSettle = true

	case *lnwire.UpdateFailHTLC:
		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		logIndex, err := state.channel.FailHTLC(pkt.payHash)
		if err != nil {
			peerLog.Errorf("unable to cancel HTLC: %v", err)
			return
		}

		// With the HTLC removed, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled. The "Reason" field will have already been set
		// within the switch.
		htlc.ChanID = state.chanID
		htlc.ID = logIndex

		// Finally, we send the HTLC message to the peer which
		// initially created the HTLC.
		p.queueMsg(htlc, nil)
		isSettle = true
	}

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	// TODO(roasbeef): enforce max HTLCs in flight limit
	if len(state.pendingBatch) >= 10 || isSettle {
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update "+
				"commitment: %v", err)
			p.Disconnect()
			return
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (p *peer) handleUpstreamMsg(state *commitmentState, msg lnwire.Message) {
	switch htlcPkt := msg.(type) {
	// TODO(roasbeef): timeouts
	//  * fail if can't parse sphinx mix-header
	case *lnwire.UpdateAddHTLC:
		// Before adding the new HTLC to the state machine, parse the
		// onion object in order to obtain the routing information.
		blobReader := bytes.NewReader(htlcPkt.OnionBlob[:])
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
			p.Disconnect()
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
		rHash := htlcPkt.PaymentHash[:]
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
			rHash := htlcPkt.PaymentHash
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

	case *lnwire.UpdateFufillHTLC:
		pre := htlcPkt.PaymentPreimage
		idx := htlcPkt.ID
		if err := state.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for outgoing HTLC rejected: %v", err)
			p.Disconnect()
			return
		}

		// TODO(roasbeef): add preimage to DB in order to swipe
		// repeated r-values
	case *lnwire.UpdateFailHTLC:
		idx := htlcPkt.ID
		if err := state.channel.ReceiveFailHTLC(idx); err != nil {
			peerLog.Errorf("unable to recv HTLC cancel: %v", err)
			p.Disconnect()
			return
		}

		state.cancelReasons[idx] = lnwire.FailCode(htlcPkt.Reason[0])

	case *lnwire.CommitSig:
		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		// TODO(roasbeef): redundant re-serialization
		sig := htlcPkt.CommitSig.Serialize()
		if err := state.channel.ReceiveNewCommitment(sig); err != nil {
			peerLog.Errorf("unable to accept new commitment: %v", err)
			p.Disconnect()
			return
		}

		// As we've just just accepted a new state, we'll now
		// immediately send the remote peer a revocation for our prior
		// state.
		nextRevocation, err := state.channel.RevokeCurrentCommitment()
		if err != nil {
			peerLog.Errorf("unable to revoke commitment: %v", err)
			return
		}
		p.queueMsg(nextRevocation, nil)

		// If both commitment chains are fully synced from our PoV,
		// then we don't need to reply with a signature as both sides
		// already have a commitment with the latest accepted state.
		if state.channel.FullySynced() {
			return
		}

		// Otherwise, the remote party initiated the state transition,
		// so we'll reply with a signature to provide them with their
		// version of the latest commitment state.
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update commitment: %v", err)
			p.Disconnect()
			return
		}

	case *lnwire.RevokeAndAck:
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
		cancelledHtlcs := make(map[uint64]struct{})
		for _, htlc := range htlcsToForward {
			parentIndex := htlc.ParentIndex
			if p, ok := state.clearedHTCLs[parentIndex]; ok {
				switch htlc.EntryType {
				// If the HTLC was settled successfully, then
				// we return a nil error as well as the payment
				// preimage back to the possible caller.
				case lnwallet.Settle:
					p.preImage <- htlc.RPreimage
					p.err <- nil

				// Otherwise, the HTLC failed, so we propagate
				// the error back to the potential caller.
				case lnwallet.Fail:
					errMsg := state.cancelReasons[parentIndex]
					p.preImage <- [32]byte{}
					p.err <- errors.New(errMsg.String())
				}

				close(p.done)

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

				settleMsg := &lnwire.UpdateFufillHTLC{
					ChanID:          state.chanID,
					ID:              logIndex,
					PaymentPreimage: preimage,
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

			logIndex, err := state.channel.FailHTLC(htlc.RHash)
			if err != nil {
				peerLog.Errorf("unable to cancel htlc: %v", err)
				p.Disconnect()
				continue
			}

			cancelMsg := &lnwire.UpdateFailHTLC{
				ChanID: state.chanID,
				ID:     logIndex,
				Reason: []byte{byte(reason)},
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
				pkt, err := logEntryToHtlcPkt(state.chanID,
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
			p.server.htlcSwitch.UpdateLink(state.chanID,
				bandwidthUpdate)
		}

		// With all the settle updates added to the local and remote
		// HTLC logs, initiate a state transition by updating the
		// remote commitment chain.
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update commitment: %v", err)
			p.Disconnect()
			return
		}

		// Notify the invoiceRegistry of the invoices we just settled
		// with this latest commitment update.
		// TODO(roasbeef): wait until next transition?
		for invoice := range settledPayments {
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
func (p *peer) updateCommitTx(state *commitmentState) error {
	sigTheirs, err := state.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		peerLog.Tracef("ChannelPoint(%v): revocation window exhausted, unable to send %v",
			state.chanPoint, len(state.pendingBatch))
		return nil
	} else if err != nil {
		return err
	}

	parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
	if err != nil {
		return fmt.Errorf("unable to parse sig: %v", err)
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    state.chanID,
		CommitSig: parsedSig,
	}
	p.queueMsg(commitSig, nil)

	// As we've just cleared out a batch, move all pending updates to the
	// map of cleared HTLCs, clearing out the set of pending updates.
	for _, update := range state.pendingBatch {
		state.clearedHTCLs[update.index] = update
	}

	// Finally, clear our the current batch, and flip the pendingUpdate
	// bool to indicate were waiting for a commitment signature.
	// TODO(roasbeef): re-slice instead to avoid GC?
	state.pendingBatch = nil

	return nil
}

// logEntryToHtlcPkt converts a particular Lightning Commitment Protocol (LCP)
// log entry the corresponding htlcPacket with src/dest set along with the
// proper wire message. This helper method is provided in order to aid an
// htlcManager in forwarding packets to the htlcSwitch.
func logEntryToHtlcPkt(chanID lnwire.ChannelID, pd *lnwallet.PaymentDescriptor,
	onionPkt *sphinx.ProcessedPacket,
	reason lnwire.FailCode) (*htlcPacket, error) {

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

		htlc := &lnwire.UpdateAddHTLC{
			Amount:      pd.Amount,
			PaymentHash: pd.RHash,
		}
		copy(htlc.OnionBlob[:], b.Bytes())
		msg = htlc

	case lnwallet.Settle:
		msg = &lnwire.UpdateFufillHTLC{
			PaymentPreimage: pd.RPreimage,
		}

	case lnwallet.Fail:
		// For cancellation messages, we'll also need to set the rHash
		// within the htlcPacket so the switch knows on which outbound
		// link to forward the cancellation message
		msg = &lnwire.UpdateFailHTLC{
			Reason: []byte{byte(reason)},
		}
		pkt.payHash = pd.RHash
	}

	pkt.amt = pd.Amount
	pkt.msg = msg

	pkt.srcLink = chanID
	pkt.onion = onionPkt

	return pkt, nil
}

// TODO(roasbeef): make all start/stop mutexes a CAS
