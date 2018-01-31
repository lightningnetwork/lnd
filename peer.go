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
	"github.com/lightningnetwork/lnd/contractcourt"

	"bytes"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
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

// outgoingMsg packages an lnwire.Message to be sent out on the wire, along with
// a buffered channel which will be sent upon once the write is complete. This
// buffered channel acts as a semaphore to be used for synchronization purposes.
type outgoingMsg struct {
	msg     lnwire.Message
	errChan chan error // MUST be buffered.
}

// newChannelMsg packages a lnwallet.LightningChannel with a channel that
// allows the receiver of the request to report when the funding transaction
// has been confirmed and the channel creation process completed.
type newChannelMsg struct {
	channel *lnwallet.LightningChannel
	done    chan struct{}
}

// closeMsgs is a wrapper struct around any wire messages that deal with the
// cooperative channel closure negotiation process. This struct includes the
// raw channel ID targeted along with the original message.
type closeMsg struct {
	cid lnwire.ChannelID
	msg lnwire.Message
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
	sendQueue chan outgoingMsg

	// outgoingQueue is a buffered channel which allows second/third party
	// objects to queue messages to be sent out on the wire.
	outgoingQueue chan outgoingMsg

	// activeChannels is a map which stores the state machines of all
	// active channels. Channels are indexed into the map by the txid of
	// the funding transaction which opened the channel.
	activeChanMtx  sync.RWMutex
	activeChannels map[lnwire.ChannelID]*lnwallet.LightningChannel

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newChannels chan *newChannelMsg

	// activeChanCloses is a map that keep track of all the active
	// cooperative channel closures that are active. Any channel closing
	// messages are directed to one of these active state machines. Once
	// the channel has been closed, the state machine will be delete from
	// the map.
	activeChanCloses map[lnwire.ChannelID]*channelCloser

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *htlcswitch.ChanClose

	// chanCloseMsgs is a channel that any message related to channel
	// closures are sent over. This includes lnwire.Shutdown message as
	// well as lnwire.ClosingSigned messages.
	chanCloseMsgs chan *closeMsg

	server *server

	// localFeatures is the set of local features that we advertised to the
	// remote node.
	localFeatures *lnwire.RawFeatureVector

	// remoteLocalFeatures is the local feature vector received from the
	// peer during the connection handshake.
	remoteLocalFeatures *lnwire.FeatureVector

	// remoteGlobalFeatures is the global feature vector received from the
	// peer during the connection handshake.
	remoteGlobalFeatures *lnwire.FeatureVector

	queueQuit chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
}

// newPeer creates a new peer from an establish connection object, and a
// pointer to the main server.
func newPeer(conn net.Conn, connReq *connmgr.ConnReq, server *server,
	addr *lnwire.NetAddress, inbound bool,
	localFeatures *lnwire.RawFeatureVector) (*peer, error) {

	nodePub := addr.IdentityKey

	p := &peer{
		conn: conn,
		addr: addr,

		id:      atomic.AddInt32(&numNodes, 1),
		inbound: inbound,
		connReq: connReq,

		server: server,

		localFeatures: localFeatures,

		sendQueue:     make(chan outgoingMsg),
		outgoingQueue: make(chan outgoingMsg),

		activeChannels: make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		newChannels:    make(chan *newChannelMsg, 1),

		activeChanCloses:   make(map[lnwire.ChannelID]*channelCloser),
		localCloseChanReqs: make(chan *htlcswitch.ChanClose),
		chanCloseMsgs:      make(chan *closeMsg),

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
		lnChan, err := lnwallet.NewLightningChannel(
			p.server.cc.signer, p.server.witnessBeacon, dbChan,
		)
		if err != nil {
			return err
		}

		chanPoint := &dbChan.FundingOutpoint

		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

		p.activeChanMtx.Lock()
		p.activeChannels[chanID] = lnChan
		p.activeChanMtx.Unlock()

		peerLog.Infof("peerID(%v) loading ChannelPoint(%v)", p.id, chanPoint)

		// Skip adding any permanently irreconcilable channels to the
		// htlcswitch.
		if dbChan.IsBorked {
			continue
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
		if info != nil && bytes.Equal(info.NodeKey1Bytes[:],
			p.server.identityPriv.PubKey().SerializeCompressed()) {

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
		chainEvents, err := p.server.chainArb.SubscribeChannelEvents(
			*chanPoint, false,
		)
		if err != nil {
			return err
		}
		linkCfg := htlcswitch.ChannelLinkConfig{
			Peer:                  p,
			DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
			DecodeOnionObfuscator: p.server.sphinx.ExtractErrorEncrypter,
			GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
				p.PubKey(), lnChan.ShortChanID()),
			DebugHTLC:     cfg.DebugHTLC,
			HodlHTLC:      cfg.HodlHTLC,
			Registry:      p.server.invoices,
			Switch:        p.server.htlcSwitch,
			FwrdingPolicy: *forwardingPolicy,
			FeeEstimator:  p.server.cc.feeEstimator,
			BlockEpochs:   blockEpoch,
			PreimageCache: p.server.witnessBeacon,
			ChainEvents:   chainEvents,
			UpdateContractSignals: func(signals *contractcourt.ContractSignals) error {
				return p.server.chainArb.UpdateContractSignals(
					*chanPoint, signals,
				)
			},
			SyncStates: true,
			BatchTicker: htlcswitch.NewBatchTicker(
				time.NewTicker(50 * time.Millisecond)),
			BatchSize: 10,
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

// msgStream implements a goroutine-safe, in-order stream of messages to be
// delivered via closure to a receiver. These messages MUST be in order due to
// the nature of the lightning channel commitment and gossiper state machines.
// TODO(conner): use stream handler interface to abstract out stream
// state/logging
type msgStream struct {
	streamShutdown int32

	peer *peer

	apply func(lnwire.Message)

	startMsg string
	stopMsg  string

	msgCond *sync.Cond
	msgs    []lnwire.Message

	mtx sync.Mutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// newMsgStream creates a new instance of a chanMsgStream for a particular
// channel identified by its channel ID.
func newMsgStream(p *peer, startMsg, stopMsg string,
	apply func(lnwire.Message)) *msgStream {

	stream := &msgStream{
		peer:     p,
		apply:    apply,
		startMsg: startMsg,
		stopMsg:  stopMsg,
		quit:     make(chan struct{}),
	}
	stream.msgCond = sync.NewCond(&stream.mtx)

	return stream
}

// Start starts the chanMsgStream.
func (ms *msgStream) Start() {
	ms.wg.Add(1)
	go ms.msgConsumer()
}

// Stop stops the chanMsgStream.
func (ms *msgStream) Stop() {
	// TODO(roasbeef): signal too?

	close(ms.quit)

	// Now that we've closed the channel, we'll repeatedly signal the msg
	// consumer until we've detected that it has exited.
	for atomic.LoadInt32(&ms.streamShutdown) == 0 {
		ms.msgCond.Signal()
		time.Sleep(time.Millisecond * 100)
	}

	ms.wg.Wait()
}

// msgConsumer is the main goroutine that streams messages from the peer's
// readHandler directly to the target channel.
func (ms *msgStream) msgConsumer() {
	defer ms.wg.Done()
	defer peerLog.Tracef(ms.stopMsg)

	peerLog.Tracef(ms.startMsg)

	for {
		// First, we'll check our condition. If the queue of messages
		// is empty, then we'll wait until a new item is added.
		ms.msgCond.L.Lock()
		for len(ms.msgs) == 0 {
			ms.msgCond.Wait()

			// If we woke up in order to exit, then we'll do so.
			// Otherwise, we'll check the message queue for any new
			// items.
			select {
			case <-ms.quit:
				ms.msgCond.L.Unlock()
				atomic.StoreInt32(&ms.streamShutdown, 1)
				return
			default:
			}
		}

		// Grab the message off the front of the queue, shifting the
		// slice's reference down one in order to remove the message
		// from the queue.
		msg := ms.msgs[0]
		ms.msgs[0] = nil // Set to nil to prevent GC leak.
		ms.msgs = ms.msgs[1:]

		ms.msgCond.L.Unlock()

		ms.apply(msg)
	}
}

// AddMsg adds a new message to the msgStream. This function is safe for
// concurrent access.
func (ms *msgStream) AddMsg(msg lnwire.Message) {
	// First, we'll lock the condition, and add the message to the end of
	// the message queue.
	ms.msgCond.L.Lock()
	ms.msgs = append(ms.msgs, msg)
	ms.msgCond.L.Unlock()

	// With the message added, we signal to the msgConsumer that there are
	// additional messages to consume.
	ms.msgCond.Signal()
}

// newChanMsgStream is used to create a msgStream between the peer and
// particular channel link in the htlcswitch.  We utilize additional
// synchronization with the fundingManager to ensure we don't attempt to
// dispatch a message to a channel before it is fully active. A reference to the
// channel this stream forwards to his held in scope to prevent unnecessary
// lookups.
func newChanMsgStream(p *peer, cid lnwire.ChannelID) *msgStream {

	var chanLink htlcswitch.ChannelLink

	return newMsgStream(p,
		fmt.Sprintf("Update stream for ChannelID(%x) created", cid[:]),
		fmt.Sprintf("Update stream for ChannelID(%x) exiting", cid[:]),
		func(msg lnwire.Message) {
			_, isChanSycMsg := msg.(*lnwire.ChannelReestablish)

			// If this is the chanSync message, then we'll deliver
			// it immediately to the active link.
			if !isChanSycMsg {
				// We'll send a message to the funding manager
				// and wait iff an active funding process for
				// this channel hasn't yet completed.  We do
				// this in order to account for the following
				// scenario: we send the funding locked message
				// to the other side, they immediately send a
				// channel update message, but we haven't yet
				// sent the channel to the channelManager.
				p.server.fundingMgr.waitUntilChannelOpen(cid)
			}

			// TODO(roasbeef): only wait if not chan sync

			// Dispatch the commitment update message to the proper active
			// goroutine dedicated to this channel.
			if chanLink == nil {
				link, err := p.server.htlcSwitch.GetLink(cid)
				if err != nil {
					peerLog.Errorf("recv'd update for unknown "+
						"channel %v from %v", cid, p)
					return
				}
				chanLink = link
			}

			chanLink.HandleChannelUpdate(msg)
		},
	)
}

// newDiscMsgStream is used to setup a msgStream between the peer and the
// authenticated gossiper. This stream should be used to forward all remote
// channel announcements.
func newDiscMsgStream(p *peer) *msgStream {
	return newMsgStream(p,
		"Update stream for gossiper created",
		"Update stream for gossiper exited",
		func(msg lnwire.Message) {
			p.server.authGossiper.ProcessRemoteAnnouncement(msg,
				p.addr.IdentityKey)
		},
	)
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

	discStream := newDiscMsgStream(p)
	discStream.Start()
	defer discStream.Stop()

	chanMsgStreams := make(map[lnwire.ChannelID]*msgStream)
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
			case p.chanCloseMsgs <- &closeMsg{msg.ChannelID, msg}:
			case <-p.quit:
				break out
			}
		case *lnwire.ClosingSigned:
			select {
			case p.chanCloseMsgs <- &closeMsg{msg.ChannelID, msg}:
			case <-p.quit:
				break out
			}

		case *lnwire.Error:
			p.server.fundingMgr.processFundingError(msg, p.addr)

		// TODO(roasbeef): create ChanUpdater interface for the below
		case *lnwire.UpdateAddHTLC:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.UpdateFulfillHTLC:
			isChanUpdate = true
			targetChan = msg.ChanID
		case *lnwire.UpdateFailMalformedHTLC:
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
		case *lnwire.ChannelReestablish:
			isChanUpdate = true
			targetChan = msg.ChanID

		case *lnwire.ChannelUpdate,
			*lnwire.ChannelAnnouncement,
			*lnwire.NodeAnnouncement,
			*lnwire.AnnounceSignatures:

			discStream.AddMsg(msg)

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
				chanStream = newChanMsgStream(p, targetChan)
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
		return fmt.Sprintf("temp_chan_id=%x, chain=%v, csv=%v, amt=%v, "+
			"push_amt=%v, reserve=%v, flags=%v",
			msg.PendingChannelID[:], msg.ChainHash,
			msg.CsvDelay, msg.FundingAmount, msg.PushAmount,
			msg.ChannelReserve, msg.ChannelFlags)

	case *lnwire.AcceptChannel:
		return fmt.Sprintf("temp_chan_id=%x, reserve=%v, csv=%v, num_confs=%v",
			msg.PendingChannelID[:], msg.ChannelReserve, msg.CsvDelay,
			msg.MinAcceptDepth)

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
		return fmt.Sprintf("chan_id=%v, id=%v, reason=%x", msg.ChanID,
			msg.ID, msg.Reason)

	case *lnwire.UpdateFulfillHTLC:
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
		return fmt.Sprintf("chan_id=%v, err=%v", msg.ChanID, string(msg.Data))

	case *lnwire.AnnounceSignatures:
		return fmt.Sprintf("chan_id=%v, short_chan_id=%v", msg.ChannelID,
			msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelAnnouncement:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v",
			msg.ChainHash, msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelUpdate:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v, flag=%v, "+
			"update_time=%v", msg.ChainHash,
			msg.ShortChannelID.ToUint64(), msg.Flags,
			time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.NodeAnnouncement:
		return fmt.Sprintf("node=%x, update_time=%v",
			msg.NodeID, time.Unix(int64(msg.Timestamp), 0))

	case *lnwire.Ping:
		// No summary.
		return ""

	case *lnwire.Pong:
		// No summary.
		return ""

	case *lnwire.UpdateFee:
		return fmt.Sprintf("chan_id=%v, fee_update_sat=%v",
			msg.ChanID, int64(msg.FeePerKw))

	case *lnwire.ChannelReestablish:
		return fmt.Sprintf("next_local_height=%v, remote_tail_height=%v",
			msg.NextLocalCommitHeight, msg.RemoteCommitTailHeight)
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

		preposition := "to"
		if read {
			preposition = "from"
		}

		return fmt.Sprintf("%v %v%s %v %s", summaryPrefix,
			msg.MsgType(), summary, preposition, p)
	}))

	switch m := msg.(type) {
	case *lnwire.ChannelReestablish:
		if m.LocalUnrevokedCommitPoint != nil {
			m.LocalUnrevokedCommitPoint.Curve = nil
		}
	case *lnwire.RevokeAndAck:
		m.NextRevocationKey.Curve = nil
	case *lnwire.AcceptChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.HtlcPoint.Curve = nil
		m.FirstCommitmentPoint.Curve = nil
	case *lnwire.OpenChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.HtlcPoint.Curve = nil
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
			if outMsg.errChan != nil {
				outMsg.errChan <- err
			}

			if err != nil {
				exitErr = errors.Errorf("unable to write message: %v", err)
				break out
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

	// pendingMsgs will hold all messages waiting to be added
	// to the sendQueue.
	pendingMsgs := list.New()

	for {
		// Examine the front of the queue.
		elem := pendingMsgs.Front()
		if elem != nil {
			// There's an element on the queue, try adding
			// it to the sendQueue. We also watch for
			// messages on the outgoingQueue, in case the
			// writeHandler cannot accept messages on the
			// sendQueue.
			select {
			case p.sendQueue <- elem.Value.(outgoingMsg):
				pendingMsgs.Remove(elem)
			case msg := <-p.outgoingQueue:
				pendingMsgs.PushBack(msg)
			case <-p.quit:
				return
			}
		} else {
			// If there weren't any messages to send to the
			// writeHandler, then we'll accept a new message
			// into the queue from outside sub-systems.
			select {
			case msg := <-p.outgoingQueue:
				pendingMsgs.PushBack(msg)
			case <-p.quit:
				return
			}
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
// wire. It returns an error if we failed to queue the message. An error
// is sent on errChan if the message fails being sent to the peer, or
// nil otherwise.
func (p *peer) queueMsg(msg lnwire.Message, errChan chan error) {
	select {
	case p.outgoingQueue <- outgoingMsg{msg, errChan}:
	case <-p.quit:
		peerLog.Tracef("Peer shutting down, could not enqueue msg.")
		if errChan != nil {
			errChan <- fmt.Errorf("peer shutting down")
		}
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

// genDeliveryScript returns a new script to be used to send our funds to in
// the case of a cooperative channel close negotiation.
func (p *peer) genDeliveryScript() ([]byte, error) {
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

// channelManager is goroutine dedicated to handling all requests/signals
// pertaining to the opening, cooperative closing, and force closing of all
// channels maintained with the remote peer.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) channelManager() {
	defer p.wg.Done()

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
			if currentChan, ok := p.activeChannels[chanID]; ok {
				peerLog.Infof("Already have ChannelPoint(%v), "+
					"ignoring.", chanPoint)

				p.activeChanMtx.Unlock()
				close(newChanReq.done)
				newChanReq.channel.Stop()

				// If we're being sent a new channel, and our
				// existing channel doesn't have the next
				// revocation, then we need to update the
				// current existing channel.
				if currentChan.RemoteNextRevocation() != nil {
					continue
				}

				peerLog.Infof("Processing retransmitted "+
					"FundingLocked for ChannelPoint(%v)",
					chanPoint)

				nextRevoke := newChan.RemoteNextRevocation()
				err := currentChan.InitNextRevocation(nextRevoke)
				if err != nil {
					peerLog.Errorf("unable to init chan "+
						"revocation: %v", err)
					continue
				}

				continue
			}

			// If not already active, we'll add this channel to the
			// set of active channels, so we can look it up later
			// easily according to its channel ID.
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
			chainEvents, err := p.server.chainArb.SubscribeChannelEvents(
				*chanPoint, false,
			)
			if err != nil {
				peerLog.Errorf("unable to subscribe to chain "+
					"events: %v", err)
				continue
			}
			linkConfig := htlcswitch.ChannelLinkConfig{
				Peer:                  p,
				DecodeHopIterator:     p.server.sphinx.DecodeHopIterator,
				DecodeOnionObfuscator: p.server.sphinx.ExtractErrorEncrypter,
				GetLastChannelUpdate: createGetLastUpdate(p.server.chanRouter,
					p.PubKey(), newChanReq.channel.ShortChanID()),
				DebugHTLC:     cfg.DebugHTLC,
				HodlHTLC:      cfg.HodlHTLC,
				Registry:      p.server.invoices,
				Switch:        p.server.htlcSwitch,
				FwrdingPolicy: p.server.cc.routingPolicy,
				FeeEstimator:  p.server.cc.feeEstimator,
				BlockEpochs:   blockEpoch,
				PreimageCache: p.server.witnessBeacon,
				ChainEvents:   chainEvents,
				UpdateContractSignals: func(signals *contractcourt.ContractSignals) error {
					return p.server.chainArb.UpdateContractSignals(
						*chanPoint, signals,
					)
				},
				SyncStates: false,
				BatchTicker: htlcswitch.NewBatchTicker(
					time.NewTicker(50 * time.Millisecond)),
				BatchSize: 10,
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

		// We've just received a local request to close an active
		// channel. If will either kick of a cooperative channel
		// closure negotiation, or be a notification of a breached
		// contract that should be abandoned.
		case req := <-p.localCloseChanReqs:
			p.handleLocalCloseReq(req)

		// We've received a new cooperative channel closure related
		// message from the remote peer, we'll use this message to
		// advance the chan closer state machine.
		case closeMsg := <-p.chanCloseMsgs:
			// We'll now fetch the matching closing state machine
			// in order to continue, or finalize the channel
			// closure process.
			chanCloser, err := p.fetchActiveChanCloser(closeMsg.cid)
			if err != nil {
				// TODO(roasbeef): send protocol error?
				peerLog.Errorf("unable to respond to remote "+
					"close msg: %v", err)
				continue
			}

			// Next, we'll process the next message using the
			// target state machine. We'll either continue
			// negotiation, or halt.
			msgs, closeFin, err := chanCloser.ProcessCloseMsg(
				closeMsg.msg,
			)
			if err != nil {
				err := fmt.Errorf("unable to process close "+
					"msg: %v", err)
				peerLog.Error(err)

				// As the negotiations failed, we'll reset the
				// channel state to ensure we act to on-chain
				// events as normal.
				chanCloser.cfg.channel.ResetState()

				if chanCloser.CloseRequest() != nil {
					chanCloser.CloseRequest().Err <- err
				}
				delete(p.activeChanCloses, closeMsg.cid)
				continue
			}

			// Queue any messages to the remote peer that need to
			// be sent as a part of this latest round of
			// negotiations.
			for _, msg := range msgs {
				p.queueMsg(msg, nil)
			}

			// If we haven't finished close negotiations, then
			// we'll continue as we can't yet finalize the closure.
			if !closeFin {
				continue
			}

			// Otherwise, we've agreed on a closing fee! In this
			// case, we'll wrap up the channel closure by notifying
			// relevant sub-systems and launching a goroutine to
			// wait for close tx conf.
			p.finalizeChanClosure(chanCloser)
		case <-p.quit:

			// As, we've been signalled to exit, we'll reset all
			// our active channel back to their default state.
			p.activeChanMtx.Lock()
			for _, channel := range p.activeChannels {
				channel.ResetState()
			}
			p.activeChanMtx.Unlock()

			break out
		}
	}
}

// fetchActiveChanCloser attempts to fetch the active chan closer state machine
// for the target channel ID. If the channel isn't active an error is returned.
// Otherwise, either an existing state machine will be returned, or a new one
// will be created.
func (p *peer) fetchActiveChanCloser(chanID lnwire.ChannelID) (*channelCloser, error) {
	// First, we'll ensure that we actually know of the target channel. If
	// not, we'll ignore this message.
	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unable to close channel, "+
			"ChannelID(%v) is unknown", chanID)
	}

	// We'll attempt to look up the matching state machine, if we can't
	// find one then this means that the remote party is initiating a
	// cooperative channel closure.
	chanCloser, ok := p.activeChanCloses[chanID]
	if !ok {
		// We'll create a valid closing state machine in order to
		// respond to the initiated cooperative channel closure.
		deliveryAddr, err := p.genDeliveryScript()
		if err != nil {
			return nil, err
		}

		// In order to begin fee negotiations, we'll first compute our
		// target ideal fee-per-kw. We'll set this to a lax value, as
		// we weren't the ones that initiated the channel closure.
		satPerWight, err := p.server.cc.feeEstimator.EstimateFeePerWeight(6)
		if err != nil {
			return nil, fmt.Errorf("unable to query fee "+
				"estimator: %v", err)
		}

		// We'll then convert the sat per weight to sat per k/w as this
		// is the native unit used within the protocol when dealing
		// with fees.
		targetFeePerKw := satPerWight * 1000

		_, startingHeight, err := p.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return nil, err
		}

		// Before we create the chan closer, we'll start a new
		// cooperative channel closure transaction from the chain arb.
		// Wtih this context, we'll ensure that we're able to respond
		// if *any* of the transactions we sign off on are ever
		// broadcast.
		closeCtx, err := p.server.chainArb.BeginCoopChanClose(
			*channel.ChannelPoint(),
		)
		if err != nil {
			return nil, err
		}

		chanCloser = newChannelCloser(
			chanCloseCfg{
				channel:           channel,
				unregisterChannel: p.server.htlcSwitch.RemoveLink,
				broadcastTx:       p.server.cc.wallet.PublishTransaction,
				quit:              p.quit,
			},
			deliveryAddr,
			targetFeePerKw,
			uint32(startingHeight),
			nil,
			closeCtx,
		)
		p.activeChanCloses[chanID] = chanCloser
	}

	return chanCloser, nil
}

// handleLocalCloseReq kicks-off the workflow to execute a cooperative or
// forced unilateral closure of the channel initiated by a local subsystem.
//
// TODO(roasbeef): if no more active channels with peer call Remove on connMgr
// with peerID
func (p *peer) handleLocalCloseReq(req *htlcswitch.ChanClose) {
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
		// First, we'll fetch a fresh delivery address that we'll use
		// to send the funds to in the case of a successful
		// negotiation.
		deliveryAddr, err := p.genDeliveryScript()
		if err != nil {
			peerLog.Errorf(err.Error())
			req.Err <- err
			return
		}

		// Before we create the chan closer, we'll start a new
		// cooperative channel closure transaction from the chain arb.
		// Wtih this context, we'll ensure that we're able to respond
		// if *any* of the transactions we sign off on are ever
		// broadcast.
		closeCtx, err := p.server.chainArb.BeginCoopChanClose(
			*channel.ChannelPoint(),
		)
		if err != nil {
			peerLog.Errorf(err.Error())
			req.Err <- err
			return
		}

		// Next, we'll create a new channel closer state machine to
		// handle the close negotiation.
		_, startingHeight, err := p.server.cc.chainIO.GetBestBlock()
		if err != nil {
			peerLog.Errorf(err.Error())
			req.Err <- err
			return
		}
		chanCloser := newChannelCloser(
			chanCloseCfg{
				channel:           channel,
				unregisterChannel: p.server.htlcSwitch.RemoveLink,
				broadcastTx:       p.server.cc.wallet.PublishTransaction,
				quit:              p.quit,
			},
			deliveryAddr,
			req.TargetFeePerKw,
			uint32(startingHeight),
			req,
			closeCtx,
		)
		p.activeChanCloses[chanID] = chanCloser

		// Finally, we'll initiate the channel shutdown within the
		// chanCloser, and send the shutdown message to the remote
		// party to kick things off.
		shutdownMsg, err := chanCloser.ShutdownChan()
		if err != nil {
			peerLog.Errorf(err.Error())
			req.Err <- err
			delete(p.activeChanCloses, chanID)

			// As we were unable to shutdown the channel, we'll
			// return it back to its normal state.
			channel.ResetState()
			return
		}

		p.queueMsg(shutdownMsg, nil)

	// A type of CloseBreach indicates that the counterparty has breached
	// the channel therefore we need to clean up our local state.
	case htlcswitch.CloseBreach:
		// TODO(roasbeef): no longer need with newer beach logic?
		peerLog.Infof("ChannelPoint(%v) has been breached, wiping "+
			"channel", req.ChanPoint)
		if err := p.WipeChannel(req.ChanPoint); err != nil {
			peerLog.Infof("Unable to wipe channel after detected "+
				"breach: %v", err)
			req.Err <- err
			return
		}
		return
	}
}

// finalizeChanClosure performs the final clean up steps once the cooperative
// closure transaction has been fully broadcast. The finalized closing state
// machine should be passed in. Once the transaction has been sufficiently
// confirmed, the channel will be marked as fully closed within the database,
// and any clients will be notified of updates to the closing state.
func (p *peer) finalizeChanClosure(chanCloser *channelCloser) {
	closeReq := chanCloser.CloseRequest()

	// First, we'll clear all indexes related to the channel in question.
	chanPoint := chanCloser.cfg.channel.ChannelPoint()
	if err := p.WipeChannel(chanPoint); err != nil {
		if closeReq != nil {
			closeReq.Err <- err
		}
	}

	chanCloser.cfg.channel.Stop()

	// Next, we'll launch a goroutine which will request to be notified by
	// the ChainNotifier once the closure transaction obtains a single
	// confirmation.
	notifier := p.server.cc.chainNotifier

	// If any error happens during waitForChanToClose, forward it to
	// closeReq. If this channel closure is not locally initiated, closeReq
	// will be nil, so just ignore the error.
	errChan := make(chan error, 1)
	if closeReq != nil {
		errChan = closeReq.Err
	}

	closingTx, err := chanCloser.ClosingTx()
	if err != nil {
		if closeReq != nil {
			peerLog.Error(err)
			closeReq.Err <- err
		}
	}

	closingTxid := closingTx.TxHash()

	// If this is a locally requested shutdown, update the caller with a
	// new event detailing the current pending state of this request.
	if closeReq != nil {
		closeReq.Updates <- &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: &lnrpc.PendingUpdate{
					Txid: closingTxid[:],
				},
			},
		}
	}

	go waitForChanToClose(chanCloser.negotiationHeight, notifier, errChan,
		chanPoint, &closingTxid, func() {
			// Respond to the local subsystem which requested the
			// channel closure.
			if closeReq != nil {
				closeReq.Updates <- &lnrpc.CloseStatusUpdate{
					Update: &lnrpc.CloseStatusUpdate_ChanClose{
						ChanClose: &lnrpc.ChannelCloseUpdate{
							ClosingTxid: closingTxid[:],
							Success:     true,
						},
					},
				}
			}
		})
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

// WipeChannel removes the passed channel point from all indexes associated
// with the peer, and the switch.
func (p *peer) WipeChannel(chanPoint *wire.OutPoint) error {

	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	p.activeChanMtx.Lock()
	if channel, ok := p.activeChannels[chanID]; ok {
		channel.Stop()
		delete(p.activeChannels, chanID)
	}
	p.activeChanMtx.Unlock()

	// Instruct the HtlcSwitch to close this link as the channel is no
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
	p.remoteLocalFeatures = lnwire.NewFeatureVector(msg.LocalFeatures,
		lnwire.LocalFeatures)
	p.remoteGlobalFeatures = lnwire.NewFeatureVector(msg.GlobalFeatures,
		lnwire.GlobalFeatures)

	unknownLocalFeatures := p.remoteLocalFeatures.UnknownRequiredFeatures()
	if len(unknownLocalFeatures) > 0 {
		err := errors.Errorf("Peer set unknown local feature bits: %v",
			unknownLocalFeatures)
		peerLog.Error(err)
		return err
	}

	unknownGlobalFeatures := p.remoteGlobalFeatures.UnknownRequiredFeatures()
	if len(unknownGlobalFeatures) > 0 {
		err := errors.Errorf("Peer set unknown global feature bits: %v",
			unknownGlobalFeatures)
		peerLog.Error(err)
		return err
	}

	return nil
}

// sendInitMsg sends init message to remote peer which contains our currently
// supported local and global features.
func (p *peer) sendInitMsg() error {
	msg := lnwire.NewInitMessage(
		p.server.globalFeatures.RawFeatureVector,
		p.localFeatures,
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

		// If we're the outgoing node on the first edge, then that
		// means the second edge is our policy. Otherwise, the first
		// edge is our policy.
		var local *channeldb.ChannelEdgePolicy
		if bytes.Equal(edge1.Node.PubKeyBytes[:], pubKey[:]) {
			local = edge2
		} else {
			local = edge1
		}

		update := &lnwire.ChannelUpdate{
			ChainHash:       info.ChainHash,
			ShortChannelID:  lnwire.NewShortChanIDFromInt(local.ChannelID),
			Timestamp:       uint32(local.LastUpdate.Unix()),
			Flags:           local.Flags,
			TimeLockDelta:   local.TimeLockDelta,
			HtlcMinimumMsat: local.MinHTLC,
			BaseFee:         uint32(local.FeeBaseMSat),
			FeeRate:         uint32(local.FeeProportionalMillionths),
		}
		update.Signature, err = lnwire.NewSigFromRawSignature(local.SigBytes)
		if err != nil {
			return nil, err
		}

		hswcLog.Debugf("Sending latest channel_update: %v",
			spew.Sdump(update))

		return update, nil
	}
}
