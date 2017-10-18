package main

import (
	"container/list"
	"fmt"
	"net"
	"strings"
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

	// idleTimeout is the duration of inactivity before we time out a peer.
	idleTimeout = 5 * time.Minute

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

	// sendQueueSync is a channel that's used to synchronize sends between
	// the queueHandler and the writeHandler. At times the writeHandler may
	// get blocked on sending messages. As a result we require a
	// synchronization mechanism between the two otherwise the queueHandler
	// would need to continually spin checking to see if the writeHandler
	// is ready for an additional message.
	sendQueueSync chan struct{}

	// outgoingQueue is a buffered channel which allows second/third party
	// objects to queue messages to be sent out on the wire.
	outgoingQueue chan outgoinMsg

	// activeChannels is a map which stores the state machines of all
	// active channels. Channels are indexed into the map by the txid of
	// the funding transaction which opened the channel.
	activeChanMtx  sync.RWMutex
	activeChannels map[lnwire.ChannelID]*lnwallet.LightningChannel

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

		sendQueue:     make(chan outgoinMsg),
		sendQueueSync: make(chan struct{}),
		outgoingQueue: make(chan outgoinMsg),

		activeChannels: make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		newChannels:    make(chan *newChannelMsg, 1),

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
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		msg, err := p.readNextMessage()
		if err != nil {
			readErr <- err
			msgChan <- nil
			return
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

		// If the channel we read form disk has a nil next revocation
		// key, then we'll skip loading this channel. We must do this
		// as it doesn't yet have the needed items required to initiate
		// a local state transition, or one triggered by forwarding an
		// HTLC.
		if lnChan.RemoteNextRevocation() == nil {
			peerLog.Debugf("Skipping ChannelPoint(%v), lacking "+
				"next commit point", chanPoint)
			continue
		}

		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

		p.activeChanMtx.Lock()
		p.activeChannels[chanID] = lnChan
		p.activeChanMtx.Unlock()

		peerLog.Infof("peerID(%v) loading ChannelPoint(%v)", p.id, chanPoint)

		select {
		case p.server.breachArbiter.newContracts <- lnChan:
		case <-p.server.quit:
			return fmt.Errorf("server shutting down")
		case <-p.quit:
			return fmt.Errorf("peer shutting down")
		}

		blockEpoch, err := p.server.cc.chainNotifier.RegisterBlockEpochNtfn()
		if err != nil {
			return err
		}
		_, currentHeight, err := p.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return err
		}

		// Before we register this new link with the HTLC Switch, we'll
		// need to fetch its current link-layer forwarding policy from
		// the database.
		graph := p.server.chanDB.ChannelGraph()
		info, p1, p2, err := graph.FetchChannelEdgesByOutpoint(chanPoint)
		if err != nil && err != channeldb.ErrEdgeNotFound {
			return err
		}

		// We'll filter out our policy from the directional channel
		// edges based whom the edge connects to. If it doesn't connect
		// to us, then we know that we were the one that advertised the
		// policy.
		//
		// TODO(roasbeef): can add helper method to get policy for
		// particular channel.
		var selfPolicy *channeldb.ChannelEdgePolicy
		if info != nil && info.NodeKey1.IsEqual(p.server.identityPriv.PubKey()) {
			selfPolicy = p1
		} else {
			selfPolicy = p2
		}

		// If we don't yet have an advertised routing policy, then
		// we'll use the current default, otherwise we'll translate the
		// routing policy into a forwarding policy.
		var forwardingPolicy *htlcswitch.ForwardingPolicy
		if selfPolicy != nil {
			forwardingPolicy = &htlcswitch.ForwardingPolicy{
				MinHTLC:       selfPolicy.MinHTLC,
				BaseFee:       selfPolicy.FeeBaseMSat,
				FeeRate:       selfPolicy.FeeProportionalMillionths,
				TimeLockDelta: uint32(selfPolicy.TimeLockDelta),
			}
		} else {
			forwardingPolicy = &p.server.cc.routingPolicy
		}

		peerLog.Tracef("Using link policy of: %v", spew.Sdump(forwardingPolicy))

		// Register this new channel link with the HTLC Switch. This is
		// necessary to properly route multi-hop payments, and forward
		// new payments triggered by RPC clients.
		linkCfg := htlcswitch.ChannelLinkConfig{
			Peer:                  p,
			DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
			DecodeOnionObfuscator: p.server.sphinx.ExtractErrorEncrypter,
			GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
				p.PubKey(), lnChan.ShortChanID()),
			SettledContracts: p.server.breachArbiter.settledContracts,
			DebugHTLC:        cfg.DebugHTLC,
			HodlHTLC:         cfg.HodlHTLC,
			Registry:         p.server.invoices,
			Switch:           p.server.htlcSwitch,
			FwrdingPolicy:    *forwardingPolicy,
			BlockEpochs:      blockEpoch,
		}
		link := htlcswitch.NewChannelLink(linkCfg, lnChan,
			uint32(currentHeight))

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

	p.wg.Wait()
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

// chanMsgStream implements a goroutine-safe, in-order stream of messages to be
// delivered to an active channel. These messages MUST be in order due to the
// nature of the lightning channel commitment state machine. We utilize
// additional synchronization with the fundingManager to ensure we don't
// attempt to dispatch a message to a channel before it is fully active.
type chanMsgStream struct {
	fundingMgr *fundingManager
	htlcSwitch *htlcswitch.Switch

	cid lnwire.ChannelID

	peer *peer

	msgCond *sync.Cond
	msgs    []lnwire.Message

	chanLink htlcswitch.ChannelLink

	mtx sync.Mutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// newChanMsgStream creates a new instance of a chanMsgStream for a particular
// channel identified by its channel ID.
func newChanMsgStream(f *fundingManager, h *htlcswitch.Switch, p *peer,
	c lnwire.ChannelID) *chanMsgStream {

	stream := &chanMsgStream{
		fundingMgr: f,
		htlcSwitch: h,
		peer:       p,
		cid:        c,
		quit:       make(chan struct{}),
	}
	stream.msgCond = sync.NewCond(&stream.mtx)

	return stream
}

// Start starts the chanMsgStream.
func (c *chanMsgStream) Start() {
	c.wg.Add(1)
	go c.msgConsumer()
}

// Stop stops the chanMsgStream.
func (c *chanMsgStream) Stop() {
	// TODO(roasbeef): signal too?

	close(c.quit)

	// Wake up the msgConsumer is we've been signalled to exit.
	c.msgCond.Signal()

	c.wg.Wait()
}

// msgConsumer is the main goroutine that streams messages from the peer's
// readHandler directly to the target channel.
func (c *chanMsgStream) msgConsumer() {
	defer c.wg.Done()

	peerLog.Tracef("Update stream for ChannelID(%x) created", c.cid[:])

	for {
		// First, we'll check our condition. If the queue of messages
		// is empty, then we'll wait until a new item is added.
		c.msgCond.L.Lock()
		for len(c.msgs) == 0 {
			c.msgCond.Wait()

			// If we were woke up in order to exit, then we'll do
			// so. Otherwise, we'll check the message queue for any
			// new items.
			select {
			case <-c.quit:
				peerLog.Tracef("Update stream for "+
					"ChannelID(%x) exiting", c.cid[:])
				c.msgCond.L.Unlock()
				return
			default:
			}
		}

		// Grab the message off the front of the queue, shifting the
		// slice's reference down one in order to remove the message
		// from the queue.
		msg := c.msgs[0]
		c.msgs[0] = nil // Set to nil to prevent GC leak.
		c.msgs = c.msgs[1:]

		// We'll send a message to the funding manager and wait iff an
		// active funding process for this channel hasn't yet
		// completed. We do this in order to account for the following
		// scenario: we send the funding locked message to the other
		// side, they immediately send a channel update message, but we
		// haven't yet sent the channel to the channelManager.
		c.fundingMgr.waitUntilChannelOpen(c.cid)

		// Dispatch the commitment update message to the proper active
		// goroutine dedicated to this channel.
		if c.chanLink == nil {
			link, err := c.htlcSwitch.GetLink(c.cid)
			if err != nil {
				peerLog.Errorf("recv'd update for unknown "+
					"channel %v from %v", c.cid, c.peer)
				continue
			}
			c.chanLink = link
		}

		c.chanLink.HandleChannelUpdate(msg)
		c.msgCond.L.Unlock()
	}
}

// AddMsg adds a new message to the chanMsgStream. This function is safe for
// concurrent access.
func (c *chanMsgStream) AddMsg(msg lnwire.Message) {
	// First, we'll lock the condition, and add the message to the end of
	// the message queue.
	c.msgCond.L.Lock()
	c.msgs = append(c.msgs, msg)
	c.msgCond.L.Unlock()

	// With the message added, we signal to the msgConsumer that there are
	// additional messages to consume.
	c.msgCond.Signal()
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper subsystem.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) readHandler() {

	// We'll stop the timer after a new messages is received, and also
	// reset it after we process the next message.
	idleTimer := time.AfterFunc(idleTimeout, func() {
		err := fmt.Errorf("Peer %s no answer for %s -- disconnecting",
			p, idleTimeout)
		p.Disconnect(err)
	})

	chanMsgStreams := make(map[lnwire.ChannelID]*chanMsgStream)
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, err := p.readNextMessage()
		idleTimer.Stop()
		if err != nil {
			peerLog.Infof("unable to read message from %v: %v",
				p, err)

			switch err.(type) {
			// If this is just a message we don't yet recognize,
			// we'll continue processing as normal as this allows
			// us to introduce new messages in a forwards
			// compatible manner.
			case *lnwire.UnknownMessage:
				idleTimer.Reset(idleTimeout)
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
			p.server.fundingMgr.processFundingAccept(msg, p.addr)
		case *lnwire.FundingCreated:
			p.server.fundingMgr.processFundingCreated(msg, p.addr)
		case *lnwire.FundingSigned:
			p.server.fundingMgr.processFundingSigned(msg, p.addr)
		case *lnwire.FundingLocked:
			p.server.fundingMgr.processFundingLocked(msg, p.addr)

		case *lnwire.Shutdown:
			select {
			case p.shutdownChanReqs <- msg:
			case <-p.quit:
				break out
			}
		case *lnwire.ClosingSigned:
			select {
			case p.closingSignedChanReqs <- msg:
			case <-p.quit:
				break out
			}

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

			p.server.authGossiper.ProcessRemoteAnnouncement(msg,
				p.addr.IdentityKey)
		default:
			peerLog.Errorf("unknown message %v received from peer "+
				"%v", uint16(msg.MsgType()), p)
		}

		if isChanUpdate {
			// If this is a channel update, then we need to feed it
			// into the channel's in-order message stream.
			chanStream, ok := chanMsgStreams[targetChan]
			if !ok {
				// If a stream hasn't yet been created, then
				// we'll do so, add it to the map, and finally
				// start it.
				chanStream = newChanMsgStream(p.server.fundingMgr,
					p.server.htlcSwitch, p, targetChan)
				chanMsgStreams[targetChan] = chanStream
				chanStream.Start()
			}

			// With the stream obtained, add the message to the
			// stream so we can continue processing message.
			chanStream.AddMsg(nextMsg)
		}

		idleTimer.Reset(idleTimeout)
	}

	p.wg.Done()

	p.Disconnect(errors.New("read handler closed"))

	for cid, chanStream := range chanMsgStreams {
		chanStream.Stop()

		delete(chanMsgStreams, cid)
	}

	peerLog.Tracef("readHandler for peer %v done", p)
}

// messageSummary returns a human-readable string that summarizes a
// incoming/outgoing message. Not all messages will have a summary, only those
// which have additional data that can be informative at a glance.
func messageSummary(msg lnwire.Message) string {
	switch msg := msg.(type) {
	case *lnwire.Init:
		// No summary.
		return ""

	case *lnwire.OpenChannel:
		return fmt.Sprintf("temp_chan_id=%x, chain=%x, csv=%v, amt=%v, "+
			"push_amt=%v, reserve=%v, flags=%v",
			msg.ChainHash[:], msg.PendingChannelID[:],
			msg.CsvDelay, msg.FundingAmount, msg.PushAmount,
			msg.ChannelReserve, msg.ChannelFlags)

	case *lnwire.AcceptChannel:
		return fmt.Sprintf("temp_chan_id=%x, reserve=%v, csv=%v",
			msg.PendingChannelID[:], msg.ChannelReserve, msg.CsvDelay)

	case *lnwire.FundingCreated:
		return fmt.Sprintf("temp_chan_id=%x, chan_point=%v",
			msg.PendingChannelID[:], msg.FundingPoint)

	case *lnwire.FundingSigned:
		return fmt.Sprintf("chan_id=%v", msg.ChanID)

	case *lnwire.FundingLocked:
		return fmt.Sprintf("chan_id=%v, next_point=%x",
			msg.ChanID, msg.NextPerCommitmentPoint.SerializeCompressed())

	case *lnwire.Shutdown:
		return fmt.Sprintf("chan_id=%v, script=%x", msg.ChannelID,
			msg.Address[:])

	case *lnwire.ClosingSigned:
		return fmt.Sprintf("chan_id=%v, fee_sat=%v", msg.ChannelID,
			msg.FeeSatoshis)

	case *lnwire.UpdateAddHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, amt=%v, expiry=%v, hash=%x",
			msg.ChanID, msg.ID, msg.Amount, msg.Expiry, msg.PaymentHash[:])

	case *lnwire.UpdateFailHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, reason=%v", msg.ChanID,
			msg.ID, msg.Reason)

	case *lnwire.UpdateFufillHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, pre_image=%x",
			msg.ChanID, msg.ID, msg.PaymentPreimage[:])

	case *lnwire.CommitSig:
		return fmt.Sprintf("chan_id=%v, num_htlcs=%v", msg.ChanID,
			len(msg.HtlcSigs))

	case *lnwire.RevokeAndAck:
		return fmt.Sprintf("chan_id=%v, rev=%x, next_point=%x",
			msg.ChanID, msg.Revocation[:],
			msg.NextRevocationKey.SerializeCompressed())

	case *lnwire.UpdateFailMalformedHTLC:
		return fmt.Sprintf("chan_id=%v, id=%v, fail_code=%v",
			msg.ChanID, msg.ID, msg.FailureCode)

	case *lnwire.Error:
		return fmt.Sprintf("chan_id=%v, err=%v", msg.ChanID, msg.Data)

	case *lnwire.AnnounceSignatures:
		return fmt.Sprintf("chan_id=%v, short_chan_id=%v", msg.ChannelID,
			msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelAnnouncement:
		return fmt.Sprintf("chain_hash=%x, short_chan_id=%v",
			msg.ChainHash[:], msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelUpdate:
		return fmt.Sprintf("chain_hash=%x, short_chan_id=%v, update_time=%v",
			msg.ChainHash[:], msg.ShortChannelID.ToUint64(),
			time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.NodeAnnouncement:
		return fmt.Sprintf("node=%x, update_time=%v",
			msg.NodeID.SerializeCompressed(),
			time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.Ping:
		// No summary.
		return ""

	case *lnwire.Pong:
		// No summary.
		return ""

	case *lnwire.UpdateFee:
		return fmt.Sprintf("chan_id=%v, fee_update_sat=%v",
			msg.ChanID, int64(msg.FeePerKw))
	}

	return ""
}

// logWireMessage logs the receipt or sending of particular wire message. This
// function is used rather than just logging the message in order to produce
// less spammy log messages in trace mode by setting the 'Curve" parameter to
// nil. Doing this avoids printing out each of the field elements in the curve
// parameters for secp256k1.
func (p *peer) logWireMessage(msg lnwire.Message, read bool) {
	summaryPrefix := "Received"
	if !read {
		summaryPrefix = "Sending"
	}

	peerLog.Debugf("%v", newLogClosure(func() string {
		// Debug summary of message.
		summary := messageSummary(msg)
		if len(summary) > 0 {
			summary = "(" + summary + ")"
		}

		return fmt.Sprintf("%v %v%s from %s", summaryPrefix,
			msg.MsgType(), summary, p)
	}))

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

	// TODO(roasbeef): add write deadline?

	// Finally, write the message itself in a single swoop.
	_, err = p.conn.Write(b.Bytes())
	return err
}

// writeHandler is a goroutine dedicated to reading messages off of an incoming
// queue, and writing them out to the wire. This goroutine coordinates with the
// queueHandler in order to ensure the incoming message queue is quickly
// drained.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) writeHandler() {
	var exitErr error
out:
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
				exitErr = errors.Errorf("unable to write message: %v", err)
				break out
			}

			// If the queueHandler was waiting for us to complete
			// the last write, then we'll send it a sginal that
			// we're done and are awaiting additional messages.
			select {
			case p.sendQueueSync <- struct{}{}:
			default:
			}

		case <-p.quit:
			exitErr = errors.Errorf("peer exiting")
			break out
		}
	}

	p.wg.Done()

	p.Disconnect(exitErr)

	peerLog.Tracef("writeHandler for peer %v done", p)
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
	drain:
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
				// If the write handler is currently blocked,
				// then we'll break out of this loop, to avoid
				// tightly spinning waiting for a blocked write
				// handler.
				break drain
			}
		}

		// If there weren't any messages to send, or the writehandler
		// is still blocked, then we'll accept a new message into the
		// queue from outside sub-systems. We'll also attempt to send
		// to the writeHandler again, as if this succeeds we'll once
		// again try to aggressively drain the pending message queue.
		select {
		case <-p.quit:
			return
		case msg := <-p.outgoingQueue:
			pendingMsgs.PushBack(msg)
		case <-p.sendQueueSync:
			// Fall through so we can go back to the top of the
			// drain loop.
		}

	}
}

// pingHandler is responsible for periodically sending ping messages to the
// remote peer in order to keep the connection alive and/or determine if the
// connection is still active.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) pingHandler() {
	defer p.wg.Done()

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
	p.activeChanMtx.RLock()
	defer p.activeChanMtx.RUnlock()

	snapshots := make([]*channeldb.ChannelSnapshot, 0, len(p.activeChannels))
	for _, activeChan := range p.activeChannels {
		snapshot := activeChan.StateSnapshot()
		snapshots = append(snapshots, snapshot)
	}

	return snapshots
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
	defer p.wg.Done()

	// chanShutdowns is a map of channels for which our node has initiated
	// a cooperative channel close. When an lnwire.Shutdown is received,
	// this allows the node to determine the next step to be taken in the
	// workflow.
	chanShutdowns := make(map[lnwire.ChannelID]*htlcswitch.ChanClose)

	deliveryAddrs := make(map[lnwire.ChannelID]*closingScripts)

	// initiator[ShutdownSigs|FeeProposals] holds the
	// [signature|feeProposal] for the last ClosingSigned sent to the peer
	// by the initiator. This enables us to respond to subsequent steps in
	// the workflow without having to recalculate our signature for the
	// channel close transaction, and track the sent fee proposals for fee
	// negotiation purposes.
	initiatorShutdownSigs := make(map[lnwire.ChannelID][]byte)
	initiatorFeeProposals := make(map[lnwire.ChannelID]uint64)

	// responder[ShutdownSigs|FeeProposals] is similar to the the maps
	// above, just for the responder.
	responderShutdownSigs := make(map[lnwire.ChannelID][]byte)
	responderFeeProposals := make(map[lnwire.ChannelID]uint64)

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
		// A new channel has arrived which means we've just completed a
		// funding workflow. We'll initialize the necessary local
		// state, and notify the htlc switch of a new link.
		case newChanReq := <-p.newChannels:
			chanPoint := newChanReq.channel.ChannelPoint()
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
			newChan := newChanReq.channel

			// Make sure this channel is not already active.
			p.activeChanMtx.Lock()
			if _, ok := p.activeChannels[chanID]; ok {
				peerLog.Infof("Already have ChannelPoint(%v), ignoring.", chanPoint)
				p.activeChanMtx.Unlock()
				close(newChanReq.done)
				newChanReq.channel.Stop()
				continue
			}

			// If not already active, we'll add this channel to the set of active
			// channels, so we can look it up later easily
			// according to its channel ID.
			p.activeChannels[chanID] = newChan
			p.activeChanMtx.Unlock()

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with peerId(%v)", chanPoint, p.id)

			// Next, we'll assemble a ChannelLink along with the
			// necessary items it needs to function.
			//
			// TODO(roasbeef): panic on below?
			blockEpoch, err := p.server.cc.chainNotifier.RegisterBlockEpochNtfn()
			if err != nil {
				peerLog.Errorf("unable to register for block epoch: %v", err)
				continue
			}
			_, currentHeight, err := p.server.cc.chainIO.GetBestBlock()
			if err != nil {
				peerLog.Errorf("unable to get best block: %v", err)
				continue
			}
			linkConfig := htlcswitch.ChannelLinkConfig{
				Peer:                  p,
				DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
				DecodeOnionObfuscator: p.server.sphinx.ExtractErrorEncrypter,
				GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
					p.PubKey(), newChanReq.channel.ShortChanID()),
				SettledContracts: p.server.breachArbiter.settledContracts,
				DebugHTLC:        cfg.DebugHTLC,
				HodlHTLC:         cfg.HodlHTLC,
				Registry:         p.server.invoices,
				Switch:           p.server.htlcSwitch,
				FwrdingPolicy:    p.server.cc.routingPolicy,
				BlockEpochs:      blockEpoch,
			}
			link := htlcswitch.NewChannelLink(linkConfig, newChan,
				uint32(currentHeight))

			// With the channel link created, we'll now notify the
			// htlc switch so this channel can be used to dispatch
			// local payments and also passively forward payments.
			if err := p.server.htlcSwitch.AddLink(link); err != nil {
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
				// Check responderShutdownSigs for an already
				// existing shutdown signature for this channel.
				// If such a signature exists, it means we
				// already have sent a response to a shutdown
				// message for this channel, so ignore this one.
				_, exists := responderShutdownSigs[req.ChannelID]
				if exists {
					continue
				}

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
				// case the fees are immediately agreed upon.
				closeSig, proposedFee := p.handleShutdownResponse(
					req, deliveryScript)
				if closeSig != nil {
					responderShutdownSigs[req.ChannelID] = closeSig
					responderFeeProposals[req.ChannelID] = proposedFee
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
				shutdownSig := initiatorShutdownSigs[req.ChannelID]
				initiatorSig := append(shutdownSig,
					byte(txscript.SigHashAll))

				// To finalize this shtudown, we'll now send a
				// matching close signed message to the other
				// party, and broadcast the closing transaction
				// to the network. If the fees are still being
				// negotiated, handleClosingSigned returns the
				// signature and proposed fee we sent to the
				// peer. In the case fee negotiation was
				// complete, and the closing tx was broadcasted,
				// closeSig will be nil, and we can delete the
				// state associated with this channel shutdown.
				closeSig, proposedFee := p.handleClosingSigned(
					localCloseReq, req,
					deliveryAddrs[chanID], initiatorSig,
					initiatorFeeProposals[req.ChannelID])
				if closeSig != nil {
					initiatorShutdownSigs[req.ChannelID] = closeSig
					initiatorFeeProposals[req.ChannelID] = proposedFee
				} else {
					delete(initiatorShutdownSigs, req.ChannelID)
					delete(initiatorFeeProposals, req.ChannelID)
					delete(chanShutdowns, req.ChannelID)
					delete(deliveryAddrs, req.ChannelID)
				}
				continue
			}

			shutdownSig := responderShutdownSigs[req.ChannelID]
			responderSig := append(shutdownSig,
				byte(txscript.SigHashAll))

			// Otherwise, we're the responder to the channel
			// shutdown procedure. The procedure will be the same,
			// but we don't have a local request to to notify about
			// updates, so just pass in nil instead.
			closeSig, proposedFee := p.handleClosingSigned(nil, req,
				deliveryAddrs[chanID], responderSig,
				responderFeeProposals[req.ChannelID])
			if closeSig != nil {
				responderShutdownSigs[req.ChannelID] = closeSig
				responderFeeProposals[req.ChannelID] = proposedFee
			} else {
				delete(responderShutdownSigs, req.ChannelID)
				delete(responderFeeProposals, req.ChannelID)
				delete(deliveryAddrs, chanID)
			}

		case <-p.quit:
			break out
		}
	}
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
// Returns the signature used to signed the close proposal, and the proposed
// fee.
func (p *peer) handleShutdownResponse(msg *lnwire.Shutdown,
	localDeliveryScript []byte) ([]byte, uint64) {
	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[msg.ChannelID]
	p.activeChanMtx.RUnlock()
	if !ok {
		peerLog.Errorf("unable to close channel, ChannelPoint(%v) is "+
			"unknown", msg.ChannelID)
		return nil, 0
	}

	// As we just received a shutdown message, we'll also send a shutdown
	// message with our desired fee so we can start the negotiation.
	err := p.sendShutdown(channel, localDeliveryScript)
	if err != nil {
		peerLog.Errorf("error while sending shutdown message: %v", err)
		return nil, 0
	}

	// Calculate an initial proposed fee rate for the close transaction.
	feeRate := p.server.cc.feeEstimator.EstimateFeePerWeight(1) * 1000

	// We propose a fee and send a close proposal to the peer. This will
	// start the fee negotiations. Once both sides agree on a fee, we'll
	// create a signature that closes the channel using the agreed upon fee.
	fee := channel.CalcFee(feeRate)
	closeSig, proposedFee, err := channel.CreateCloseProposal(
		fee, localDeliveryScript, msg.Address,
	)
	if err != nil {
		peerLog.Errorf("unable to create close proposal: %v", err)
		return nil, 0
	}
	parsedSig, err := btcec.ParseSignature(closeSig, btcec.S256())
	if err != nil {
		peerLog.Errorf("unable to parse signature: %v", err)
		return nil, 0
	}

	// With the closing signature assembled, we'll send the matching close
	// signed message to the other party so they can broadcast the closing
	// transaction if they agree with the fee, or create a new close
	// proposal if they don't.
	closingSigned := lnwire.NewClosingSigned(msg.ChannelID, proposedFee,
		parsedSig)
	p.queueMsg(closingSigned, nil)

	return closeSig, proposedFee
}

// calculateCompromiseFee performs the current fee negotiation algorithm,
// taking into consideration our ideal fee based on current fee environment,
// the fee we last proposed (if any), and the fee proposed by the peer.
func calculateCompromiseFee(ourIdealFee, lastSentFee, peerFee uint64) uint64 {
	// We will accept a proposed fee in the interval
	// [0.5*ourIdealFee, 2*ourIdealFee]. If the peer's fee doesn't fall in
	// this range, we'll propose the average of the peer's fee and our last
	// sent fee, as long as it is in this range.
	// TODO(halseth): Dynamic fee to determine what we consider min/max for
	// timely confirmation.
	maxFee := 2 * ourIdealFee
	minFee := ourIdealFee / 2

	// If we didn't propose a fee before, just use our ideal fee value for
	// the average calculation.
	if lastSentFee == 0 {
		lastSentFee = ourIdealFee
	}
	avgFee := (lastSentFee + peerFee) / 2

	switch {
	case peerFee <= maxFee && peerFee >= minFee:
		// Peer fee is in the accepted range.
		return peerFee
	case avgFee <= maxFee && avgFee >= minFee:
		// The peer's fee is not in the accepted range, but the average
		// fee is.
		return avgFee
	case avgFee > maxFee:
		// TODO(halseth): We must ensure fee is not higher than the
		// current fee on the commitment transaction.

		// We cannot accept the average fee, as it is more than twice
		// our own estimate. Set our proposed to the maximum we can
		// accept.
		return maxFee
	default:
		// Cannot accept the average, as we consider it too low.
		return minFee
	}
}

// handleClosingSigned is called when the a ClosingSigned message is received
// from the peer. If we are the initiator in the shutdown procedure, localReq
// should be set to the local close request. If we are the responder, it should
// be set to nil.
//
// This method sends the necessary ClosingSigned message to continue fee
// negotiation, and in case we agreed on a fee completes the channel close
// transaction, and then broadcasts it. It also performs channel cleanup (and
// reports status back to the caller if this was a local shutdown request).
//
// It returns the signature and the proposed fee included in the ClosingSigned
// sent to the peer.
//
// Following the broadcast, both the initiator and responder in the channel
// closure workflow should watch the blockchain for a confirmation of the
// closing transaction before considering the channel terminated. In the case
// of an unresponsive remote party, the initiator can either choose to execute
// a force closure, or backoff for a period of time, and retry the cooperative
// closure.
func (p *peer) handleClosingSigned(localReq *htlcswitch.ChanClose,
	msg *lnwire.ClosingSigned, deliveryScripts *closingScripts,
	lastSig []byte, lastFee uint64) ([]byte, uint64) {

	chanID := msg.ChannelID
	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()
	if !ok {
		err := fmt.Errorf("unable to close channel, ChannelID(%v) is "+
			"unknown", chanID)
		peerLog.Errorf(err.Error())
		if localReq != nil {
			localReq.Err <- err
		}
		return nil, 0
	}
	// We now consider the fee proposed by the peer, together with the fee
	// we last proposed (if any). This method will in case more fee
	// negotiation is necessary send a new ClosingSigned message to the peer
	// with our new proposed fee. In case we can agree on a fee, it will
	// assemble the close transaction, and we can go on to broadcasting it.
	closeTx, ourSig, ourFee, err := p.negotiateFeeAndCreateCloseTx(channel,
		msg, deliveryScripts, lastSig, lastFee)
	if err != nil {
		if localReq != nil {
			localReq.Err <- err
		}
		return nil, 0
	}

	// If closeTx == nil it means that we did not agree on a fee, but we
	// proposed a new fee to the peer. Return the signature used for this
	// new proposal, and the fee we proposed, for use when we get a reponse.
	if closeTx == nil {
		return ourSig, ourFee
	}

	chanPoint := channel.ChannelPoint()

	select {
	case p.server.breachArbiter.settledContracts <- chanPoint:
	case <-p.server.quit:
		return nil, 0
	case <-p.quit:
		return nil, 0
	}

	// We agreed on a fee, and we can broadcast the closure transaction to
	// the network.
	peerLog.Infof("Broadcasting cooperative close tx: %v",
		newLogClosure(func() string {
			return spew.Sdump(closeTx)
		}))

	if err := p.server.cc.wallet.PublishTransaction(closeTx); err != nil {
		// TODO(halseth): Add relevant error types to the
		// WalletController interface as this is quite fragile.
		if strings.Contains(err.Error(), "already exists") ||
			strings.Contains(err.Error(), "already have") {
			peerLog.Infof("channel close tx from ChannelPoint(%v) "+
				" already exist, probably broadcasted by peer: %v",
				chanPoint, err)
		} else {
			peerLog.Errorf("channel close tx from ChannelPoint(%v) "+
				" rejected: %v", chanPoint, err)

			// TODO(roasbeef): send ErrorGeneric to other side
			return nil, 0
		}
	}

	// Once we've completed the cooperative channel closure, we'll wipe the
	// channel so we reject any incoming forward or payment requests via
	// this channel.
	select {
	case p.server.breachArbiter.settledContracts <- chanPoint:
	case <-p.server.quit:
		return nil, 0
	}
	if err := p.WipeChannel(channel); err != nil {
		if localReq != nil {
			localReq.Err <- err
		}
		return nil, 0
	}

	// TODO(roasbeef): also add closure height to summary

	// Clear out the current channel state, marking the channel as being
	// closed within the database.
	closingTxid := closeTx.TxHash()
	chanInfo := channel.StateSnapshot()
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:      *chanPoint,
		ClosingTXID:    closingTxid,
		RemotePub:      &chanInfo.RemoteIdentity,
		Capacity:       chanInfo.Capacity,
		SettledBalance: chanInfo.LocalBalance.ToSatoshis(),
		CloseType:      channeldb.CooperativeClose,
		IsPending:      true,
	}
	if err := channel.DeleteState(closeSummary); err != nil {
		if localReq != nil {
			localReq.Err <- err
		}
		return nil, 0
	}

	// If this is a locally requested shutdown, update the caller with a new
	// event detailing the current pending state of this request.
	if localReq != nil {
		localReq.Updates <- &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: &lnrpc.PendingUpdate{
					Txid: closingTxid[:],
				},
			},
		}
	}

	_, bestHeight, err := p.server.cc.chainIO.GetBestBlock()
	if err != nil {
		if localReq != nil {
			localReq.Err <- err
		}
		return nil, 0
	}

	// Finally, launch a goroutine which will request to be notified by the
	// ChainNotifier once the closure transaction obtains a single
	// confirmation.
	notifier := p.server.cc.chainNotifier

	// If any error happens during waitForChanToClose, forard it to
	// localReq. If this channel closure is not locally initiated, localReq
	// will be nil, so just ignore the error.
	errChan := make(chan error, 1)
	if localReq != nil {
		errChan = localReq.Err
	}

	go waitForChanToClose(uint32(bestHeight), notifier, errChan,
		chanPoint, &closingTxid, func() {

			// First, we'll mark the database as being fully closed
			// so we'll no longer watch for its ultimate closure
			// upon startup.
			err := p.server.chanDB.MarkChanFullyClosed(chanPoint)
			if err != nil {
				if localReq != nil {
					localReq.Err <- err
				}
				return
			}

			// Respond to the local subsystem which requested the
			// channel closure.
			if localReq != nil {
				localReq.Updates <- &lnrpc.CloseStatusUpdate{
					Update: &lnrpc.CloseStatusUpdate_ChanClose{
						ChanClose: &lnrpc.ChannelCloseUpdate{
							ClosingTxid: closingTxid[:],
							Success:     true,
						},
					},
				}
			}
		})
	return nil, 0
}

// negotiateFeeAndCreateCloseTx takes into consideration the closing transaction
// fee proposed by the remote peer in the ClosingSigned message and our
// previously proposed fee (set to 0 if no previous), and continues the fee
// negotiation it process. In case the peer agreed on the same fee as we
// previously sent, it will assemble the close transaction and broadcast it. In
// case the peer propose a fee different from our previous proposal, but that
// can be accepted, a ClosingSigned message with the accepted fee is sent,
// before the closing transaction is broadcasted. In the case where we cannot
// accept the peer's proposed fee, a new fee proposal will be sent.
//
// TODO(halseth): In the case where we cannot accept the fee, and we cannot
// make more proposals, this method should return an error, and we should fail
// the channel.
func (p *peer) negotiateFeeAndCreateCloseTx(channel *lnwallet.LightningChannel,
	msg *lnwire.ClosingSigned, deliveryScripts *closingScripts, ourSig []byte,
	ourFeeProp uint64) (*wire.MsgTx, []byte, uint64, error) {

	peerFeeProposal := msg.FeeSatoshis

	// If the fee proposed by the peer is different from what we proposed
	// before (or we did not propose anything yet), we must check if we can
	// accept the proposal, or if we should negotiate.
	if peerFeeProposal != ourFeeProp {
		// The peer has suggested a different fee from what we proposed.
		// Let's calculate if this one is tolerable.
		ourIdealFeeRate := p.server.cc.feeEstimator.
			EstimateFeePerWeight(1) * 1000
		ourIdealFee := channel.CalcFee(ourIdealFeeRate)
		fee := calculateCompromiseFee(ourIdealFee, ourFeeProp,
			peerFeeProposal)

		// Our new proposed fee must be strictly between what we
		// proposed before and what the peer proposed.
		isAcceptable := false
		if fee < peerFeeProposal && fee > ourFeeProp {
			isAcceptable = true
		}
		if fee < ourFeeProp && fee > peerFeeProposal {
			isAcceptable = true
		}

		if !isAcceptable {
			// TODO(halseth): fail channel
		}

		// Since the compromise fee is different from the fee we last
		// proposed, we must update our proposal.

		// Create a new close proposal with the compromise fee, and
		// send this to the peer.
		closeSig, proposedFee, err := channel.CreateCloseProposal(fee,
			deliveryScripts.localScript, deliveryScripts.remoteScript)
		if err != nil {
			peerLog.Errorf("unable to create close proposal: %v",
				err)
			return nil, nil, 0, err
		}
		parsedSig, err := btcec.ParseSignature(closeSig, btcec.S256())
		if err != nil {
			peerLog.Errorf("unable to parse signature: %v", err)
			return nil, nil, 0, err
		}
		closingSigned := lnwire.NewClosingSigned(msg.ChannelID,
			proposedFee, parsedSig)
		p.queueMsg(closingSigned, nil)

		// If the compromise fee was different from what the peer
		// proposed, then we must return and wait for an answer, if not
		// we can go on to complete the close transaction.
		if fee != peerFeeProposal {
			return nil, closeSig, proposedFee, nil
		}

		// We accept the fee proposed by the peer, so prepare our
		// signature to complete the close transaction.
		ourSig = append(closeSig, byte(txscript.SigHashAll))
	}

	// We agreed on a fee, and we have the peer's signature for this fee,
	// so we can assemble the close tx.
	peerSig := append(msg.Signature.Serialize(), byte(txscript.SigHashAll))
	chanPoint := channel.ChannelPoint()
	closeTx, err := channel.CompleteCooperativeClose(ourSig, peerSig,
		deliveryScripts.localScript, deliveryScripts.remoteScript,
		peerFeeProposal)
	if err != nil {
		peerLog.Errorf("unable to complete cooperative "+
			"close for ChannelPoint(%v): %v",
			chanPoint, err)
		// TODO(roasbeef): send ErrorGeneric to other side
		return nil, nil, 0, err
	}
	return closeTx, nil, 0, nil
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
	peerLog.Infof("ChannelPoint(%v) is now closed at "+
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
		info, edge1, edge2, err := router.GetChannelByID(chanID)
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

		update := &lnwire.ChannelUpdate{
			Signature:       local.Signature,
			ChainHash:       info.ChainHash,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(local.ChannelID),
			Timestamp:       uint32(local.LastUpdate.Unix()),
			Flags:           local.Flags,
			TimeLockDelta:   local.TimeLockDelta,
			HtlcMinimumMsat: local.MinHTLC,
			BaseFee:         uint32(local.FeeBaseMSat),
			FeeRate:         uint32(local.FeeProportionalMillionths),
		}

		hswcLog.Debugf("Sending latest channel_update: %v",
			spew.Sdump(update))

		return update, nil
	}
}
