package main

import (
	"container/list"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/brontide"

	"bytes"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
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
	pubKeyBytes [33]byte

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

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newChannels chan *newChannelMsg

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *htlcswitch.ChanClose

	// shutdownChanReqs is used to send the Shutdown messages that initiate
	// the cooperative close workflow.
	shutdownChanReqs chan *lnwire.Shutdown

	// closingSignedChanReqs is used to send signatures for proposed
	// channel close transactions during the cooperative close workflow.
	closingSignedChanReqs chan *lnwire.ClosingSigned

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
		conn: conn,
		addr: addr,

		id:      atomic.AddInt32(&numNodes, 1),
		inbound: inbound,
		connReq: connReq,

		server: server,

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		activeChannels:   make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		chanSnapshotReqs: make(chan *chanSnapshotReq),
		newChannels:      make(chan *newChannelMsg, 1),

		localCloseChanReqs:    make(chan *htlcswitch.ChanClose),
		shutdownChanReqs:      make(chan *lnwire.Shutdown),
		closingSignedChanReqs: make(chan *lnwire.ClosingSigned),

		localSharedFeatures:  nil,
		globalSharedFeatures: nil,

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
	}
	copy(p.pubKeyBytes[:], nodePub.SerializeCompressed())

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

	// Once the init message arrives, we can parse it so we can figure out
	// the negotiation of features for this session.
	msg := <-msgChan
	if msg, ok := msg.(*lnwire.Init); ok {
		if err := p.handleInitMsg(msg); err != nil {
			return err
		}
	} else {
		return errors.New("very first message between nodes " +
			"must be init message")
	}

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

	p.wg.Add(5)
	go p.queueHandler()
	go p.writeHandler()
	go p.readHandler()
	go p.channelManager()
	go p.pingHandler()

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

		lnChan, err := lnwallet.NewLightningChannel(p.server.cc.signer,
			p.server.cc.chainNotifier, p.server.cc.feeEstimator, dbChan)
		if err != nil {
			return err
		}

		chanPoint := &dbChan.FundingOutpoint
		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

		p.activeChanMtx.Lock()
		p.activeChannels[chanID] = lnChan
		p.activeChanMtx.Unlock()

		peerLog.Infof("peerID(%v) loaded ChannelPoint(%v)", p.id, chanPoint)

		p.server.breachArbiter.newContracts <- lnChan

		// Register this new channel link with the HTLC Switch. This is
		// necessary to properly route multi-hop payments, and forward
		// new payments triggered by RPC clients.
		link := htlcswitch.NewChannelLink(
			htlcswitch.ChannelLinkConfig{
				Peer:                  p,
				DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
				DecodeOnionObfuscator: p.server.sphinx.DecodeOnionObfuscator,
				GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
					p.PubKey(), lnChan.ShortChanID()),
				SettledContracts: p.server.breachArbiter.settledContracts,
				DebugHTLC:        cfg.DebugHTLC,
				Registry:         p.server.invoices,
				Switch:           p.server.htlcSwitch,
				FwrdingPolicy:    p.server.cc.routingPolicy,
			},
			lnChan,
		)

		if err := p.server.htlcSwitch.AddLink(link); err != nil {
			return err
		}
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
func (p *peer) Disconnect(reason error) {
	if !atomic.CompareAndSwapInt32(&p.disconnect, 0, 1) {
		return
	}

	peerLog.Tracef("Disconnecting %s, reason: %v", p, reason)

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

		case *lnwire.OpenChannel:
			p.server.fundingMgr.processFundingOpen(msg, p.addr)
		case *lnwire.AcceptChannel:
			p.server.fundingMgr.processFundingResponse(msg, p.addr)
		case *lnwire.FundingCreated:
			p.server.fundingMgr.processFundingCreated(msg, p.addr)
		case *lnwire.FundingSigned:
			p.server.fundingMgr.processFundingSigned(msg, p.addr)
		case *lnwire.FundingLocked:
			p.server.fundingMgr.processFundingLocked(msg, p.addr)

		case *lnwire.Shutdown:
			p.shutdownChanReqs <- msg
		case *lnwire.ClosingSigned:
			p.closingSignedChanReqs <- msg

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
		case *lnwire.UpdateFee:
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
				// Dispatch the commitment update message to the proper
				// active goroutine dedicated to this channel.
				link, err := p.server.htlcSwitch.GetLink(targetChan)
				if err != nil {
					peerLog.Errorf("recv'd update for unknown "+
						"channel %v from %v", targetChan, p)
					return
				}
				link.HandleChannelUpdate(nextMsg)
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

	p.Disconnect(errors.New("read handler closed"))

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
	case *lnwire.AcceptChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.FirstCommitmentPoint.Curve = nil
	case *lnwire.OpenChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.FirstCommitmentPoint.Curve = nil
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
				p.Disconnect(errors.Errorf("unable to write message: %v",
					err))
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

// closingScripts are the set of clsoign deslivery scripts for each party. This
// intermediate state is maintained for each active close negotiation, as the
// final signatures sent must cover the specified delivery scripts for each
// party.
type closingScripts struct {
	localScript  []byte
	remoteScript []byte
}

// channelManager is goroutine dedicated to handling all requests/signals
// pertaining to the opening, cooperative closing, and force closing of all
// channels maintained with the remote peer.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) channelManager() {
	// chanShutdowns is a map of channels for which our node has initiated
	// a cooperative channel close. When an lnwire.Shutdown is received,
	// this allows the node to determine the next step to be taken in the
	// workflow.
	chanShutdowns := make(map[lnwire.ChannelID]*htlcswitch.ChanClose)

	deliveryAddrs := make(map[lnwire.ChannelID]*closingScripts)

	// shutdownSigs is a map of signatures maintained by the responder in a
	// cooperative channel close. This map enables us to respond to
	// subsequent steps in the workflow without having to recalculate our
	// signature for the channel close transaction.
	shutdownSigs := make(map[lnwire.ChannelID][]byte)

	// TODO(roasbeef): move to cfg closure func
	genDeliveryScript := func() ([]byte, error) {
		deliveryAddr, err := p.server.cc.wallet.NewAddress(
			lnwallet.WitnessPubKey, false,
		)
		if err != nil {
			return nil, err
		}
		peerLog.Infof("Delivery addr for channel close: %v",
			deliveryAddr)

		return txscript.PayToAddrScript(deliveryAddr)
	}
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

		// A new channel has arrived which means we've just completed a
		// funding workflow. We'll initialize the necessary local
		// state, and notify the htlc switch of a new link.
		case newChanReq := <-p.newChannels:
			chanPoint := newChanReq.channel.ChannelPoint()
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
			newChan := newChanReq.channel

			// First, we'll add this channel to the set of active
			// channels, so we can look it up later easily
			// according to its channel ID.
			p.activeChanMtx.Lock()
			p.activeChannels[chanID] = newChan
			p.activeChanMtx.Unlock()

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			// Next, we'll assemble a ChannelLink along with the
			// necessary items it needs to function.
			linkConfig := htlcswitch.ChannelLinkConfig{
				Peer:                  p,
				DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
				DecodeOnionObfuscator: p.server.sphinx.DecodeOnionObfuscator,
				GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
					p.PubKey(), newChanReq.channel.ShortChanID()),
				SettledContracts: p.server.breachArbiter.settledContracts,
				DebugHTLC:        cfg.DebugHTLC,
				Registry:         p.server.invoices,
				Switch:           p.server.htlcSwitch,
				FwrdingPolicy:    p.server.cc.routingPolicy,
			}
			link := htlcswitch.NewChannelLink(linkConfig, newChan)

			// With the channel link created, we'll now notify the
			// htlc switch so this channel can be used to dispatch
			// local payments and also passively forward payments.
			err := p.server.htlcSwitch.AddLink(link)
			if err != nil {
				peerLog.Errorf("can't register new channel "+
					"link(%v) with peerId(%v)", chanPoint, p.id)
			}

			close(newChanReq.done)

		// We've just received a local quest to close an active
		// channel.
		case req := <-p.localCloseChanReqs:
			// So we'll first transition the channel to a state of
			// pending shutdown.
			chanID := lnwire.NewChanIDFromOutPoint(req.ChanPoint)

			// We'll only track this shutdown request if this is a
			// regular close request, and not in response to a
			// channel breach.
			var (
				deliveryScript []byte
				err            error
			)
			if req.CloseType == htlcswitch.CloseRegular {
				chanShutdowns[chanID] = req

				// As we need to close out the channel and
				// claim our funds on-chain, we'll request a
				// new delivery address from the wallet, and
				// turn that into it corresponding output
				// script.
				deliveryScript, err = genDeliveryScript()
				if err != nil {
					cErr := fmt.Errorf("Unable to generate "+
						"delivery address: %v", err)

					peerLog.Errorf(cErr.Error())

					req.Err <- cErr
					continue
				}

				// We'll also track this delivery script, as
				// we'll need it to reconstruct the cooperative
				// closure transaction during our closing fee
				// negotiation ratchet.
				deliveryAddrs[chanID] = &closingScripts{
					localScript: deliveryScript,
				}
			}

			// With the state marked as shutting down, we can now
			// proceed with the channel close workflow. If this is
			// regular close, we'll send a shutdown. Otherwise,
			// we'll simply be clearing our indexes.
			p.handleLocalClose(req, deliveryScript)

		// A receipt of a message over this channel indicates that
		// either a shutdown proposal has been initiated, or a prior
		// one has been completed, advancing to the next state of
		// channel closure.
		case req := <-p.shutdownChanReqs:
			// If we don't have a channel that matches this channel
			// ID, then we'll ignore this message.
			chanID := req.ChannelID
			p.activeChanMtx.Lock()
			_, ok := p.activeChannels[chanID]
			p.activeChanMtx.Unlock()
			if !ok {
				peerLog.Warnf("Received unsolicited shutdown msg: %v",
					spew.Sdump(req))
				continue
			}

			// First, we'll track their delivery script for when we
			// ultimately create the cooperative closure
			// transaction.
			deliveryScripts, ok := deliveryAddrs[chanID]
			if !ok {
				deliveryAddrs[chanID] = &closingScripts{}
				deliveryScripts = deliveryAddrs[chanID]
			}
			deliveryScripts.remoteScript = req.Address

			// Next, we'll check in the shutdown map to see if
			// we're the initiator or not.  If we don't have an
			// entry for this channel, then this means that we're
			// the responder to the workflow.
			if _, ok := chanShutdowns[req.ChannelID]; !ok {
				// As we're the responder, we'll need to
				// generate a delivery script of our own.
				deliveryScript, err := genDeliveryScript()
				if err != nil {
					peerLog.Errorf("Unable to generate "+
						"delivery address: %v", err)
					continue
				}
				deliveryScripts.localScript = deliveryScript

				// In this case, we'll send a shutdown message,
				// and also prep our closing signature for the
				// case they fees are immediately agreed upon.
				closeSig := p.handleShutdownResponse(req,
					deliveryScript)
				if closeSig != nil {
					shutdownSigs[chanID] = closeSig
				}
			}

		// A receipt of a message over this channel indicates that the
		// final stage of a channel shutdown workflow has been
		// completed.
		case req := <-p.closingSignedChanReqs:
			// First we'll check if this has an entry in the local
			// shutdown map.
			chanID := req.ChannelID
			localCloseReq, ok := chanShutdowns[chanID]

			// If it does, then this means we were the initiator of
			// the channel shutdown procedure.
			if ok {
				// To finalize this shutdown, we'll now send a
				// matching close signed message to the other
				// party, and broadcast the closing transaction
				// to the network.
				p.handleInitClosingSigned(localCloseReq, req,
					deliveryAddrs[chanID])

				delete(chanShutdowns, req.ChannelID)
				delete(deliveryAddrs, req.ChannelID)
				continue
			}

			// Otherwise, we're the responder to the channel
			// shutdown procedure. In this case, we'll mark the
			// channel as pending close, and watch the network for
			// the ultimate confirmation of the closing
			// transaction.
			responderSig := append(shutdownSigs[chanID],
				byte(txscript.SigHashAll))
			p.handleResponseClosingSigned(req, responderSig,
				deliveryAddrs[chanID])

			delete(shutdownSigs, chanID)
			delete(deliveryAddrs, chanID)

		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
}

// handleLocalClose kicks-off the workflow to execute a cooperative or forced
// unilateral closure of the channel initiated by a local subsystem.
//
// TODO(roasbeef): if no more active channels with peer call Remove on connMgr
// with peerID
func (p *peer) handleLocalClose(req *htlcswitch.ChanClose, deliveryScript []byte) {
	chanID := lnwire.NewChanIDFromOutPoint(req.ChanPoint)

	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()
	if !ok {
		err := fmt.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", chanID)
		peerLog.Errorf(err.Error())
		req.Err <- err
		return
	}

	switch req.CloseType {

	// A type of CloseRegular indicates that the user has opted to close
	// out this channel on-chain, so we execute the cooperative channel
	// closure workflow.
	case htlcswitch.CloseRegular:
		err := p.sendShutdown(channel, deliveryScript)
		if err != nil {
			req.Err <- err
			return
		}

	// A type of CloseBreach indicates that the counterparty has breached
	// the channel therefore we need to clean up our local state.
	case htlcswitch.CloseBreach:
		// TODO(roasbeef): no longer need with newer beach logic?
		peerLog.Infof("ChannelPoint(%v) has been breached, wiping "+
			"channel", req.ChanPoint)
		if err := p.WipeChannel(channel); err != nil {
			peerLog.Infof("Unable to wipe channel after detected "+
				"breach: %v", err)
			req.Err <- err
			return
		}
		return
	}
}

// handleShutdownResponse is called when a responder in a cooperative channel
// close workflow receives a Shutdown message. This is the second step in the
// cooperative close workflow. This function generates a close transaction with
// a proposed fee amount and sends the signed transaction to the initiator.
func (p *peer) handleShutdownResponse(msg *lnwire.Shutdown,
	localDeliveryScript []byte) []byte {

	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[msg.ChannelID]
	p.activeChanMtx.RUnlock()
	if !ok {
		peerLog.Errorf("unable to close channel, ChannelPoint(%v) is "+
			"unknown", msg.ChannelID)
		return nil
	}

	// As we just received a shutdown message, we'll also send a shutdown
	// message with our desired fee so we can start the negotiation.
	err := p.sendShutdown(channel, localDeliveryScript)
	if err != nil {
		peerLog.Errorf("error while sending shutdown message: %v", err)
		return nil
	}

	// Calculate an initial proposed fee rate for the close transaction.
	feeRate := p.server.cc.feeEstimator.EstimateFeePerWeight(1) * 1000

	// TODO(roasbeef): actually perform fee negotiation here, only send sig
	// if we agree to fee

	// Once both sides agree on a fee, we'll create a signature that closes
	// the channel using the agree upon fee rate.
	closeSig, proposedFee, err := channel.CreateCloseProposal(
		feeRate, localDeliveryScript, msg.Address,
	)
	if err != nil {
		peerLog.Errorf("unable to create close proposal: %v", err)
		return nil
	}
	parsedSig, err := btcec.ParseSignature(closeSig, btcec.S256())
	if err != nil {
		peerLog.Errorf("unable to parse signature: %v", err)
		return nil
	}

	// With the closing signature assembled, we'll send the matching close
	// signed message to the other party so they can broadcast the closing
	// transaction.
	closingSigned := lnwire.NewClosingSigned(msg.ChannelID, proposedFee,
		parsedSig)
	p.queueMsg(closingSigned, nil)

	return closeSig
}

// handleInitClosingSigned is called when the initiator in a cooperative
// channel close workflow receives a ClosingSigned message from the responder.
// This method completes the channel close transaction, sends back a
// corresponding ClosingSigned message, then broadcasts the channel close
// transaction. It also performs channel cleanup and reports status back to the
// caller. This is the initiator's final step in the channel close workflow.
//
// Following the broadcast, both the initiator and responder in the channel
// closure workflow should watch the blockchain for a confirmation of the
// closing transaction before considering the channel terminated. In the case
// of an unresponsive remote party, the initiator can either choose to execute
// a force closure, or backoff for a period of time, and retry the cooperative
// closure.
func (p *peer) handleInitClosingSigned(req *htlcswitch.ChanClose,
	msg *lnwire.ClosingSigned, deliveryScripts *closingScripts) {

	chanID := lnwire.NewChanIDFromOutPoint(req.ChanPoint)
	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()
	if !ok {
		err := fmt.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", chanID)
		peerLog.Errorf(err.Error())
		req.Err <- err
		return
	}

	// Calculate a fee rate that we believe to be fair and will ensure a
	// timely confirmation.
	//
	// TODO(bvu): with a dynamic fee implementation, we will compare this
	// to the fee proposed by the responder in their ClosingSigned message.
	feeRate := p.server.cc.feeEstimator.EstimateFeePerWeight(1) * 1000

	// We agree with the proposed channel close transaction and fee rate,
	// so generate our signature.
	initiatorSig, proposedFee, err := channel.CreateCloseProposal(
		feeRate, deliveryScripts.localScript, deliveryScripts.remoteScript,
	)
	if err != nil {
		req.Err <- err
		return
	}
	initSig := append(initiatorSig, byte(txscript.SigHashAll))

	// Complete coop close transaction with the signatures of the close
	// initiator and responder.
	responderSig := msg.Signature
	respSig := append(responderSig.Serialize(), byte(txscript.SigHashAll))
	closeTx, err := channel.CompleteCooperativeClose(initSig, respSig,
		deliveryScripts.localScript, deliveryScripts.remoteScript,
		feeRate)
	if err != nil {
		req.Err <- err
		// TODO(roasbeef): send ErrorGeneric to other side
		return
	}

	// As we're the initiator of this channel shutdown procedure we'll now
	// create a mirrored close signed message with our completed signature.
	parsedSig, err := btcec.ParseSignature(initSig, btcec.S256())
	if err != nil {
		req.Err <- err
		return
	}
	closingSigned := lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	p.queueMsg(closingSigned, nil)

	// Finally, broadcast the closure transaction to the network.
	peerLog.Infof("Broadcasting cooperative close tx: %v",
		newLogClosure(func() string {
			return spew.Sdump(closeTx)
		}))
	if err := p.server.cc.wallet.PublishTransaction(closeTx); err != nil {
		peerLog.Errorf("channel close tx from "+
			"ChannelPoint(%v) rejected: %v",
			req.ChanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		return
	}

	// Once we've completed the cooperative channel closure, we'll wipe the
	// channel so we reject any incoming forward or payment requests via
	// this channel.
	p.server.breachArbiter.settledContracts <- req.ChanPoint
	if err := p.WipeChannel(channel); err != nil {
		req.Err <- err
		return
	}

	// TODO(roasbeef): also add closure height to summary

	// Clear out the current channel state, marking the channel as being
	// closed within the database.
	closingTxid := closeTx.TxHash()
	chanInfo := channel.StateSnapshot()
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:      *req.ChanPoint,
		ClosingTXID:    closingTxid,
		RemotePub:      &chanInfo.RemoteIdentity,
		Capacity:       chanInfo.Capacity,
		SettledBalance: chanInfo.LocalBalance,
		CloseType:      channeldb.CooperativeClose,
		IsPending:      true,
	}
	if err := channel.DeleteState(closeSummary); err != nil {
		req.Err <- err
		return
	}

	// Update the caller with a new event detailing the current pending
	// state of this request.
	req.Updates <- &lnrpc.CloseStatusUpdate{
		Update: &lnrpc.CloseStatusUpdate_ClosePending{
			ClosePending: &lnrpc.PendingUpdate{
				Txid: closingTxid[:],
			},
		},
	}

	_, bestHeight, err := p.server.cc.chainIO.GetBestBlock()
	if err != nil {
		req.Err <- err
		return
	}

	// Finally, launch a goroutine which will request to be notified by the
	// ChainNotifier once the closure transaction obtains a single
	// confirmation.
	notifier := p.server.cc.chainNotifier
	go waitForChanToClose(uint32(bestHeight), notifier, req.Err,
		req.ChanPoint, &closingTxid, func() {

			// First, we'll mark the database as being fully closed
			// so we'll no longer watch for its ultimate closure
			// upon startup.
			err := p.server.chanDB.MarkChanFullyClosed(req.ChanPoint)
			if err != nil {
				req.Err <- err
				return
			}

			// Respond to the local subsystem which requested the
			// channel closure.
			req.Updates <- &lnrpc.CloseStatusUpdate{
				Update: &lnrpc.CloseStatusUpdate_ChanClose{
					ChanClose: &lnrpc.ChannelCloseUpdate{
						ClosingTxid: closingTxid[:],
						Success:     true,
					},
				},
			}
		})
}

// handleResponseClosingSigned is called when the responder in a cooperative
// close workflow receives a ClosingSigned message. This function handles the
// finalization of the cooperative close from the perspective of the responder.
func (p *peer) handleResponseClosingSigned(msg *lnwire.ClosingSigned,
	respSig []byte, deliveryScripts *closingScripts) {

	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[msg.ChannelID]
	p.activeChanMtx.RUnlock()
	if !ok {
		peerLog.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", msg.ChannelID)
		return
	}

	// Now that we have the initiator's signature for the closure
	// transaction, we can assemble the final closure transaction, complete
	// with our signature.
	initiatorSig := msg.Signature
	initSig := append(initiatorSig.Serialize(), byte(txscript.SigHashAll))
	chanPoint := channel.ChannelPoint()

	// Calculate our expected fee rate.
	// TODO(roasbeef): should instead use the fee within the message
	feeRate := p.server.cc.feeEstimator.EstimateFeePerWeight(1) * 1000
	closeTx, err := channel.CompleteCooperativeClose(respSig, initSig,
		deliveryScripts.localScript, deliveryScripts.remoteScript,
		feeRate)
	if err != nil {
		peerLog.Errorf("unable to complete cooperative "+
			"close for ChannelPoint(%v): %v",
			chanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		return
	}
	closeTxid := closeTx.TxHash()

	_, bestHeight, err := p.server.cc.chainIO.GetBestBlock()
	if err != nil {
		peerLog.Errorf("unable to get best height: %v", err)
	}

	// Once we've completed the cooperative channel closure, we'll wipe the
	// channel so we reject any incoming forward or payment requests via
	// this channel.
	p.server.breachArbiter.settledContracts <- chanPoint

	// We've just broadcast the transaction which closes the channel, so
	// we'll wipe the channel from all our local indexes and also signal to
	// the switch that this channel is now closed.
	peerLog.Infof("ChannelPoint(%v) is now closed", chanPoint)
	if err := p.WipeChannel(channel); err != nil {
		peerLog.Errorf("unable to wipe channel: %v", err)
	}

	// Clear out the current channel state, marking the channel as being
	// closed within the database.
	chanInfo := channel.StateSnapshot()
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:      *chanPoint,
		ClosingTXID:    closeTxid,
		RemotePub:      &chanInfo.RemoteIdentity,
		Capacity:       chanInfo.Capacity,
		SettledBalance: chanInfo.LocalBalance,
		CloseType:      channeldb.CooperativeClose,
		IsPending:      true,
	}
	if err := channel.DeleteState(closeSummary); err != nil {
		peerLog.Errorf("unable to delete channel state: %v", err)
		return
	}

	// Finally, we'll launch a goroutine to watch the network for the
	// confirmation of the closing transaction, and mark the channel as
	// such within the database (once it's confirmed").
	notifier := p.server.cc.chainNotifier
	go waitForChanToClose(uint32(bestHeight), notifier, nil, chanPoint,
		&closeTxid, func() {
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
func waitForChanToClose(bestHeight uint32, notifier chainntnfs.ChainNotifier,
	errChan chan error, chanPoint *wire.OutPoint,
	closingTxID *chainhash.Hash, cb func()) {

	peerLog.Infof("Waiting for confirmation of cooperative close of "+
		"ChannelPoint(%v) with txid: %v", chanPoint,
		closingTxID)

	// TODO(roasbeef): add param for num needed confs
	confNtfn, err := notifier.RegisterConfirmationsNtfn(closingTxID, 1,
		bestHeight)
	if err != nil {
		if errChan != nil {
			errChan <- err
		}
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

// sendShutdown handles the creation and sending of the Shutdown messages sent
// between peers to initiate the cooperative channel close workflow. In
// addition, sendShutdown also signals to the HTLC switch to stop accepting
// HTLCs for the specified channel.
func (p *peer) sendShutdown(channel *lnwallet.LightningChannel,
	deliveryScript []byte) error {

	// In order to construct the shutdown message, we'll need to
	// reconstruct the channelID, and the current set delivery script for
	// the channel closure.
	chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())

	// With both items constructed we'll now send the shutdown message for
	// this particular channel, advertising a shutdown request to our
	// desired closing script.
	shutdown := lnwire.NewShutdown(chanID, deliveryScript)
	p.queueMsg(shutdown, nil)

	// Finally, we'll unregister the link from the switch in order to
	// Prevent the HTLC switch from receiving additional HTLCs for this
	// channel.
	p.server.htlcSwitch.RemoveLink(chanID)

	return nil
}

// WipeChannel removes the passed channel from all indexes associated with the
// peer, and deletes the channel from the database.
func (p *peer) WipeChannel(channel *lnwallet.LightningChannel) error {
	channel.Stop()

	chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())

	p.activeChanMtx.Lock()
	delete(p.activeChannels, chanID)
	p.activeChanMtx.Unlock()

	// Instruct the Htlc Switch to close this link as the channel is no
	// longer active.

	if err := p.server.htlcSwitch.RemoveLink(chanID); err != nil {
		if err == htlcswitch.ErrChannelLinkNotFound {
			peerLog.Warnf("unable remove channel link with "+
				"ChannelPoint(%v): %v", chanID, err)
			return nil
		}
		return err
	}

	return nil
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

// SendMessage queues a message for sending to the target peer.
func (p *peer) SendMessage(msg lnwire.Message) error {
	p.queueMsg(msg, nil)
	return nil
}

// PubKey returns the pubkey of the peer in compressed serialized format.
func (p *peer) PubKey() [33]byte {
	return p.pubKeyBytes
}

// TODO(roasbeef): make all start/stop mutexes a CAS

// createGetLastUpdate returns the handler which serve as a source of the last
// update of the channel in a form of lnwire update message.
func createGetLastUpdate(router *routing.ChannelRouter,
	pubKey [33]byte, chanID lnwire.ShortChannelID) func() (*lnwire.ChannelUpdate,
	error) {

	return func() (*lnwire.ChannelUpdate, error) {
		_, edge1, edge2, err := router.GetChannelByID(chanID)
		if err != nil {
			return nil, err
		}

		if edge1 == nil || edge2 == nil {
			return nil, errors.Errorf("unable to find "+
				"channel by ShortChannelID(%v)", chanID)
		}

		var local *channeldb.ChannelEdgePolicy
		if bytes.Compare(edge1.Node.PubKey.SerializeCompressed(),
			pubKey[:]) == 0 {
			local = edge2
		} else {
			local = edge1
		}

		return &lnwire.ChannelUpdate{
			Signature:       local.Signature,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(local.ChannelID),
			Timestamp:       uint32(time.Now().Unix()),
			Flags:           local.Flags,
			TimeLockDelta:   local.TimeLockDelta,
			HtlcMinimumMsat: uint64(local.MinHTLC),
			BaseFee:         uint32(local.FeeBaseMSat),
			FeeRate:         uint32(local.FeeProportionalMillionths),
		}, nil
	}
}
