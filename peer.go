package lnd

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/buffer"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// pingInterval is the interval at which ping messages are sent.
	pingInterval = 1 * time.Minute

	// idleTimeout is the duration of inactivity before we time out a peer.
	idleTimeout = 5 * time.Minute

	// writeMessageTimeout is the timeout used when writing a message to peer.
	writeMessageTimeout = 5 * time.Second

	// readMessageTimeout is the timeout used when reading a message from a
	// peer.
	readMessageTimeout = 5 * time.Second

	// handshakeTimeout is the timeout used when waiting for peer init message.
	handshakeTimeout = 15 * time.Second

	// outgoingQueueLen is the buffer size of the channel which houses
	// messages to be sent across the wire, requested by objects outside
	// this struct.
	outgoingQueueLen = 50

	// errorBufferSize is the number of historic peer errors that we store.
	errorBufferSize = 10
)

// outgoingMsg packages an lnwire.Message to be sent out on the wire, along with
// a buffered channel which will be sent upon once the write is complete. This
// buffered channel acts as a semaphore to be used for synchronization purposes.
type outgoingMsg struct {
	priority bool
	msg      lnwire.Message
	errChan  chan error // MUST be buffered.
}

// newChannelMsg packages a channeldb.OpenChannel with a channel that allows
// the receiver of the request to report when the funding transaction has been
// confirmed and the channel creation process completed.
type newChannelMsg struct {
	channel *channeldb.OpenChannel
	err     chan error
}

// closeMsgs is a wrapper struct around any wire messages that deal with the
// cooperative channel closure negotiation process. This struct includes the
// raw channel ID targeted along with the original message.
type closeMsg struct {
	cid lnwire.ChannelID
	msg lnwire.Message
}

// pendingUpdate describes the pending state of a closing channel.
type pendingUpdate struct {
	Txid        []byte
	OutputIndex uint32
}

// channelCloseUpdate contains the outcome of the close channel operation.
type channelCloseUpdate struct {
	ClosingTxid []byte
	Success     bool
}

// timestampedError is a timestamped error that is used to store the most recent
// errors we have experienced with our peers.
type timestampedError struct {
	error     error
	timestamp time.Time
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
	disconnect int32

	// The following fields are only meant to be used *atomically*
	bytesReceived uint64
	bytesSent     uint64

	// pingTime is a rough estimate of the RTT (round-trip-time) between us
	// and the connected peer. This time is expressed in micro seconds.
	// To be used atomically.
	// TODO(roasbeef): also use a WMA or EMA?
	pingTime int64

	// pingLastSend is the Unix time expressed in nanoseconds when we sent
	// our last ping message.  To be used atomically.
	pingLastSend int64

	connReq *connmgr.ConnReq
	conn    net.Conn

	addr        *lnwire.NetAddress
	pubKeyBytes [33]byte

	// activeSignal when closed signals that the peer is now active and
	// ready to process messages.
	activeSignal chan struct{}

	// startTime is the time this peer connection was successfully
	// established. It will be zero for peers that did not successfully
	// Start().
	startTime time.Time

	inbound bool

	// sendQueue is the channel which is used to queue outgoing to be
	// written onto the wire. Note that this channel is unbuffered.
	sendQueue chan outgoingMsg

	// outgoingQueue is a buffered channel which allows second/third party
	// objects to queue messages to be sent out on the wire.
	outgoingQueue chan outgoingMsg

	// activeChanMtx protects access to the activeChannels and
	// addeddChannels maps.
	activeChanMtx sync.RWMutex

	// activeChannels is a map which stores the state machines of all
	// active channels. Channels are indexed into the map by the txid of
	// the funding transaction which opened the channel.
	//
	// NOTE: On startup, pending channels are stored as nil in this map.
	// Confirmed channels have channel data populated in the map. This means
	// that accesses to this map should nil-check the LightningChannel to
	// see if this is a pending channel or not. The tradeoff here is either
	// having two maps everywhere (one for pending, one for confirmed chans)
	// or having an extra nil-check per access.
	activeChannels map[lnwire.ChannelID]*lnwallet.LightningChannel

	// addedChannels tracks any new channels opened during this peer's
	// lifecycle. We use this to filter out these new channels when the time
	// comes to request a reenable for active channels, since they will have
	// waited a shorter duration.
	addedChannels map[lnwire.ChannelID]struct{}

	// newChannels is used by the fundingManager to send fully opened
	// channels to the source peer which handled the funding workflow.
	newChannels chan *newChannelMsg

	// activeMsgStreams is a map from channel id to the channel streams that
	// proxy messages to individual, active links.
	activeMsgStreams map[lnwire.ChannelID]*msgStream

	// activeChanCloses is a map that keep track of all the active
	// cooperative channel closures that are active. Any channel closing
	// messages are directed to one of these active state machines. Once
	// the channel has been closed, the state machine will be delete from
	// the map.
	activeChanCloses map[lnwire.ChannelID]*channelCloser

	// localCloseChanReqs is a channel in which any local requests to close
	// a particular channel are sent over.
	localCloseChanReqs chan *htlcswitch.ChanClose

	// linkFailures receives all reported channel failures from the switch,
	// and instructs the channelManager to clean remaining channel state.
	linkFailures chan linkFailureReport

	// chanCloseMsgs is a channel that any message related to channel
	// closures are sent over. This includes lnwire.Shutdown message as
	// well as lnwire.ClosingSigned messages.
	chanCloseMsgs chan *closeMsg

	// chanActiveTimeout specifies the duration the peer will wait to
	// request a channel reenable, beginning from the time the peer was
	// started.
	chanActiveTimeout time.Duration

	server *server

	// features is the set of features that we advertised to the remote
	// node.
	features *lnwire.FeatureVector

	// legacyFeatures is the set of features that we advertised to the remote
	// node for backwards compatibility. Nodes that have not implemented
	// flat featurs will still be able to read our feature bits from the
	// legacy global field, but we will also advertise everything in the
	// default features field.
	legacyFeatures *lnwire.FeatureVector

	// outgoingCltvRejectDelta defines the number of blocks before expiry of
	// an htlc where we don't offer an htlc anymore.
	outgoingCltvRejectDelta uint32

	// remoteFeatures is the feature vector received from the peer during
	// the connection handshake.
	remoteFeatures *lnwire.FeatureVector

	// resentChanSyncMsg is a set that keeps track of which channels we
	// have re-sent channel reestablishment messages for. This is done to
	// avoid getting into loop where both peers will respond to the other
	// peer's chansync message with its own over and over again.
	resentChanSyncMsg map[lnwire.ChannelID]struct{}

	// errorBuffer stores a set of errors related to a peer. It contains
	// error messages that our peer has recently sent us over the wire and
	// records of unknown messages that were sent to us and, so that we can
	// track a full record of the communication errors we have had with our
	// peer. If we choose to disconnect from a peer, it also stores the
	// reason we had for disconnecting.
	errorBuffer *queue.CircularBuffer

	// writePool is the task pool to that manages reuse of write buffers.
	// Write tasks are submitted to the pool in order to conserve the total
	// number of write buffers allocated at any one time, and decouple write
	// buffer allocation from the peer life cycle.
	writePool *pool.Write

	readPool *pool.Read

	queueQuit chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
}

// A compile-time check to ensure that peer satisfies the lnpeer.Peer interface.
var _ lnpeer.Peer = (*peer)(nil)

// newPeer creates a new peer from an establish connection object, and a
// pointer to the main server. It takes an error buffer which may contain errors
// from a previous connection with the peer if we have been connected to them
// before.
func newPeer(conn net.Conn, connReq *connmgr.ConnReq, server *server,
	addr *lnwire.NetAddress, inbound bool,
	features, legacyFeatures *lnwire.FeatureVector,
	chanActiveTimeout time.Duration,
	outgoingCltvRejectDelta uint32,
	errBuffer *queue.CircularBuffer) (
	*peer, error) {

	nodePub := addr.IdentityKey

	p := &peer{
		conn: conn,
		addr: addr,

		activeSignal: make(chan struct{}),

		inbound: inbound,
		connReq: connReq,

		server: server,

		features:       features,
		legacyFeatures: legacyFeatures,

		outgoingCltvRejectDelta: outgoingCltvRejectDelta,

		sendQueue:     make(chan outgoingMsg),
		outgoingQueue: make(chan outgoingMsg),

		addedChannels:  make(map[lnwire.ChannelID]struct{}),
		activeChannels: make(map[lnwire.ChannelID]*lnwallet.LightningChannel),
		newChannels:    make(chan *newChannelMsg, 1),

		activeMsgStreams: make(map[lnwire.ChannelID]*msgStream),

		activeChanCloses:   make(map[lnwire.ChannelID]*channelCloser),
		localCloseChanReqs: make(chan *htlcswitch.ChanClose),
		linkFailures:       make(chan linkFailureReport),
		chanCloseMsgs:      make(chan *closeMsg),
		resentChanSyncMsg:  make(map[lnwire.ChannelID]struct{}),

		chanActiveTimeout: chanActiveTimeout,

		errorBuffer: errBuffer,

		writePool: server.writePool,
		readPool:  server.readPool,

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

	peerLog.Tracef("Peer %v starting", p)

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
	// an upper timeout to respond before we bail out early.
	case <-time.After(handshakeTimeout):
		return fmt.Errorf("peer did not complete handshake within %v",
			handshakeTimeout)
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
			p.storeError(err)
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

	if len(activeChans) == 0 {
		p.server.prunePersistentPeerConnection(p.pubKeyBytes)
	}

	// Next, load all the active channels we have with this peer,
	// registering them with the switch and launching the necessary
	// goroutines required to operate them.
	peerLog.Debugf("Loaded %v active channels from database with "+
		"NodeKey(%x)", len(activeChans), p.PubKey())

	msgs, err := p.loadActiveChannels(activeChans)
	if err != nil {
		return fmt.Errorf("unable to load channels: %v", err)
	}

	p.startTime = time.Now()

	p.wg.Add(5)
	go p.queueHandler()
	go p.writeHandler()
	go p.readHandler()
	go p.channelManager()
	go p.pingHandler()

	// Signal to any external processes that the peer is now active.
	close(p.activeSignal)

	// Now that the peer has started up, we send any channel sync messages
	// that must be resent for borked channels.
	if len(msgs) > 0 {
		peerLog.Infof("Sending %d channel sync messages to peer after "+
			"loading active channels", len(msgs))
		if err := p.SendMessage(true, msgs...); err != nil {
			peerLog.Warnf("Failed sending channel sync "+
				"messages to peer %v: %v", p, err)
		}
	}

	// Node announcements don't propagate very well throughout the network
	// as there isn't a way to efficiently query for them through their
	// timestamp, mostly affecting nodes that were offline during the time
	// of broadcast. We'll resend our node announcement to the remote peer
	// as a best-effort delivery such that it can also propagate to their
	// peers. To ensure they can successfully process it in most cases,
	// we'll only resend it as long as we have at least one confirmed
	// advertised channel with the remote peer.
	//
	// TODO(wilmer): Remove this once we're able to query for node
	// announcements through their timestamps.
	p.maybeSendNodeAnn(activeChans)

	return nil
}

// initGossipSync initializes either a gossip syncer or an initial routing
// dump, depending on the negotiated synchronization method.
func (p *peer) initGossipSync() {
	switch {

	// If the remote peer knows of the new gossip queries feature, then
	// we'll create a new gossipSyncer in the AuthenticatedGossiper for it.
	case p.remoteFeatures.HasFeature(lnwire.GossipQueriesOptional):
		srvrLog.Infof("Negotiated chan series queries with %x",
			p.pubKeyBytes[:])

		// Register the this peer's for gossip syncer with the gossiper.
		// This is blocks synchronously to ensure the gossip syncer is
		// registered with the gossiper before attempting to read
		// messages from the remote peer.
		//
		// TODO(wilmer): Only sync updates from non-channel peers. This
		// requires an improved version of the current network
		// bootstrapper to ensure we can find and connect to non-channel
		// peers.
		p.server.authGossiper.InitSyncState(p)
	}
}

// QuitSignal is a method that should return a channel which will be sent upon
// or closed once the backing peer exits. This allows callers using the
// interface to cancel any processing in the event the backing implementation
// exits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) QuitSignal() <-chan struct{} {
	return p.quit
}

// loadActiveChannels creates indexes within the peer for tracking all active
// channels returned by the database. It returns a slice of channel reestablish
// messages that should be sent to the peer immediately, in case we have borked
// channels that haven't been closed yet.
func (p *peer) loadActiveChannels(chans []*channeldb.OpenChannel) (
	[]lnwire.Message, error) {

	// Return a slice of messages to send to the peers in case the channel
	// cannot be loaded normally.
	var msgs []lnwire.Message

	for _, dbChan := range chans {
		lnChan, err := lnwallet.NewLightningChannel(
			p.server.cc.signer, dbChan, p.server.sigPool,
		)
		if err != nil {
			return nil, err
		}

		chanPoint := &dbChan.FundingOutpoint

		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

		peerLog.Infof("NodeKey(%x) loading ChannelPoint(%v)",
			p.PubKey(), chanPoint)

		// Skip adding any permanently irreconcilable channels to the
		// htlcswitch.
		switch {
		case !dbChan.HasChanStatus(channeldb.ChanStatusDefault) &&
			!dbChan.HasChanStatus(channeldb.ChanStatusRestored):

			peerLog.Warnf("ChannelPoint(%v) has status %v, won't "+
				"start.", chanPoint, dbChan.ChanStatus())

			// To help our peer recover from a potential data loss,
			// we resend our channel reestablish message if the
			// channel is in a borked state. We won't process any
			// channel reestablish message sent from the peer, but
			// that's okay since the assumption is that we did when
			// marking the channel borked.
			chanSync, err := dbChan.ChanSyncMsg()
			if err != nil {
				peerLog.Errorf("Unable to create channel "+
					"reestablish message for channel %v: "+
					"%v", chanPoint, err)
				continue
			}

			msgs = append(msgs, chanSync)
			continue
		}

		_, currentHeight, err := p.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return nil, err
		}

		// Before we register this new link with the HTLC Switch, we'll
		// need to fetch its current link-layer forwarding policy from
		// the database.
		graph := p.server.chanDB.ChannelGraph()
		info, p1, p2, err := graph.FetchChannelEdgesByOutpoint(chanPoint)
		if err != nil && err != channeldb.ErrEdgeNotFound {
			return nil, err
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
				MinHTLCOut:    selfPolicy.MinHTLC,
				MaxHTLC:       selfPolicy.MaxHTLC,
				BaseFee:       selfPolicy.FeeBaseMSat,
				FeeRate:       selfPolicy.FeeProportionalMillionths,
				TimeLockDelta: uint32(selfPolicy.TimeLockDelta),
			}
		} else {
			peerLog.Warnf("Unable to find our forwarding policy "+
				"for channel %v, using default values",
				chanPoint)
			forwardingPolicy = &p.server.cc.routingPolicy
		}

		peerLog.Tracef("Using link policy of: %v",
			spew.Sdump(forwardingPolicy))

		// If the channel is pending, set the value to nil in the
		// activeChannels map. This is done to signify that the channel is
		// pending. We don't add the link to the switch here - it's the funding
		// manager's responsibility to spin up pending channels. Adding them
		// here would just be extra work as we'll tear them down when creating
		// + adding the final link.
		if lnChan.IsPending() {
			p.activeChanMtx.Lock()
			p.activeChannels[chanID] = nil
			p.activeChanMtx.Unlock()

			continue
		}

		// Subscribe to the set of on-chain events for this channel.
		chainEvents, err := p.server.chainArb.SubscribeChannelEvents(
			*chanPoint,
		)
		if err != nil {
			return nil, err
		}

		err = p.addLink(
			chanPoint, lnChan, forwardingPolicy, chainEvents,
			currentHeight, true,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to add link %v to "+
				"switch: %v", chanPoint, err)
		}

		p.activeChanMtx.Lock()
		p.activeChannels[chanID] = lnChan
		p.activeChanMtx.Unlock()
	}

	return msgs, nil
}

// addLink creates and adds a new link from the specified channel.
func (p *peer) addLink(chanPoint *wire.OutPoint,
	lnChan *lnwallet.LightningChannel,
	forwardingPolicy *htlcswitch.ForwardingPolicy,
	chainEvents *contractcourt.ChainEventSubscription,
	currentHeight int32, syncStates bool) error {

	// onChannelFailure will be called by the link in case the channel
	// fails for some reason.
	onChannelFailure := func(chanID lnwire.ChannelID,
		shortChanID lnwire.ShortChannelID,
		linkErr htlcswitch.LinkFailureError) {

		failure := linkFailureReport{
			chanPoint:   *chanPoint,
			chanID:      chanID,
			shortChanID: shortChanID,
			linkErr:     linkErr,
		}

		select {
		case p.linkFailures <- failure:
		case <-p.quit:
		case <-p.server.quit:
		}
	}

	linkCfg := htlcswitch.ChannelLinkConfig{
		Peer:                   p,
		DecodeHopIterators:     p.server.sphinx.DecodeHopIterators,
		ExtractErrorEncrypter:  p.server.sphinx.ExtractErrorEncrypter,
		FetchLastChannelUpdate: p.server.fetchLastChanUpdate(),
		HodlMask:               cfg.Hodl.Mask(),
		Registry:               p.server.invoices,
		Switch:                 p.server.htlcSwitch,
		Circuits:               p.server.htlcSwitch.CircuitModifier(),
		ForwardPackets:         p.server.htlcSwitch.ForwardPackets,
		FwrdingPolicy:          *forwardingPolicy,
		FeeEstimator:           p.server.cc.feeEstimator,
		PreimageCache:          p.server.witnessBeacon,
		ChainEvents:            chainEvents,
		UpdateContractSignals: func(signals *contractcourt.ContractSignals) error {
			return p.server.chainArb.UpdateContractSignals(
				*chanPoint, signals,
			)
		},
		OnChannelFailure:        onChannelFailure,
		SyncStates:              syncStates,
		BatchTicker:             ticker.New(50 * time.Millisecond),
		FwdPkgGCTicker:          ticker.New(time.Minute),
		PendingCommitTicker:     ticker.New(time.Minute),
		BatchSize:               10,
		UnsafeReplay:            cfg.UnsafeReplay,
		MinFeeUpdateTimeout:     htlcswitch.DefaultMinLinkFeeUpdateTimeout,
		MaxFeeUpdateTimeout:     htlcswitch.DefaultMaxLinkFeeUpdateTimeout,
		OutgoingCltvRejectDelta: p.outgoingCltvRejectDelta,
		TowerClient:             p.server.towerClient,
		MaxOutgoingCltvExpiry:   cfg.MaxOutgoingCltvExpiry,
		MaxFeeAllocation:        cfg.MaxChannelFeeAllocation,
		NotifyActiveLink:        p.server.channelNotifier.NotifyActiveLinkEvent,
		NotifyActiveChannel:     p.server.channelNotifier.NotifyActiveChannelEvent,
		NotifyInactiveChannel:   p.server.channelNotifier.NotifyInactiveChannelEvent,
		HtlcNotifier:            p.server.htlcNotifier,
	}

	link := htlcswitch.NewChannelLink(linkCfg, lnChan)

	// Before adding our new link, purge the switch of any pending or live
	// links going by the same channel id. If one is found, we'll shut it
	// down to ensure that the mailboxes are only ever under the control of
	// one link.
	p.server.htlcSwitch.RemoveLink(link.ChanID())

	// With the channel link created, we'll now notify the htlc switch so
	// this channel can be used to dispatch local payments and also
	// passively forward payments.
	return p.server.htlcSwitch.AddLink(link)
}

// maybeSendNodeAnn sends our node announcement to the remote peer if at least
// one confirmed advertised channel exists with them.
func (p *peer) maybeSendNodeAnn(channels []*channeldb.OpenChannel) {
	hasConfirmedPublicChan := false
	for _, channel := range channels {
		if channel.IsPending {
			continue
		}
		if channel.ChannelFlags&lnwire.FFAnnounceChannel == 0 {
			continue
		}

		hasConfirmedPublicChan = true
		break
	}
	if !hasConfirmedPublicChan {
		return
	}

	ourNodeAnn, err := p.server.genNodeAnnouncement(false)
	if err != nil {
		srvrLog.Debugf("Unable to retrieve node announcement: %v", err)
		return
	}

	if err := p.SendMessageLazy(false, &ourNodeAnn); err != nil {
		srvrLog.Debugf("Unable to resend node announcement to %x: %v",
			p.pubKeyBytes, err)
	}
}

// WaitForDisconnect waits until the peer has disconnected. A peer may be
// disconnected if the local or remote side terminating the connection, or an
// irrecoverable protocol error has been encountered. This method will only
// begin watching the peer's waitgroup after the ready channel or the peer's
// quit channel are signaled. The ready channel should only be signaled if a
// call to Start returns no error. Otherwise, if the peer fails to start,
// calling Disconnect will signal the quit channel and the method will not
// block, since no goroutines were spawned.
func (p *peer) WaitForDisconnect(ready chan struct{}) {
	select {
	case <-ready:
	case <-p.quit:
	}

	p.wg.Wait()
}

// Disconnect terminates the connection with the remote peer. Additionally, a
// signal is sent to the server and htlcSwitch indicating the resources
// allocated to the peer can now be cleaned up.
func (p *peer) Disconnect(reason error) {
	if !atomic.CompareAndSwapInt32(&p.disconnect, 0, 1) {
		return
	}

	err := fmt.Errorf("disconnecting %s, reason: %v", p, reason)
	p.storeError(err)

	peerLog.Infof(err.Error())

	// Ensure that the TCP connection is properly closed before continuing.
	p.conn.Close()

	close(p.quit)
}

// String returns the string representation of this peer.
func (p *peer) String() string {
	return fmt.Sprintf("%x@%s", p.pubKeyBytes, p.conn.RemoteAddr())
}

// readNextMessage reads, and returns the next message on the wire along with
// any additional raw payload.
func (p *peer) readNextMessage() (lnwire.Message, error) {
	noiseConn, ok := p.conn.(*brontide.Conn)
	if !ok {
		return nil, fmt.Errorf("brontide.Conn required to read messages")
	}

	err := noiseConn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}

	pktLen, err := noiseConn.ReadNextHeader()
	if err != nil {
		return nil, err
	}

	// First we'll read the next _full_ message. We do this rather than
	// reading incrementally from the stream as the Lightning wire protocol
	// is message oriented and allows nodes to pad on additional data to
	// the message stream.
	var rawMsg []byte
	err = p.readPool.Submit(func(buf *buffer.Read) error {
		// Before reading the body of the message, set the read timeout
		// accordingly to ensure we don't block other readers using the
		// pool. We do so only after the task has been scheduled to
		// ensure the deadline doesn't expire while the message is in
		// the process of being scheduled.
		readDeadline := time.Now().Add(readMessageTimeout)
		readErr := noiseConn.SetReadDeadline(readDeadline)
		if readErr != nil {
			return readErr
		}

		rawMsg, readErr = noiseConn.ReadNextBody(buf[:pktLen])
		return readErr
	})

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

	p.logWireMessage(nextMsg, true)

	return nextMsg, nil
}

// msgStream implements a goroutine-safe, in-order stream of messages to be
// delivered via closure to a receiver. These messages MUST be in order due to
// the nature of the lightning channel commitment and gossiper state machines.
// TODO(conner): use stream handler interface to abstract out stream
// state/logging
type msgStream struct {
	streamShutdown int32 // To be used atomically.

	peer *peer

	apply func(lnwire.Message)

	startMsg string
	stopMsg  string

	msgCond *sync.Cond
	msgs    []lnwire.Message

	mtx sync.Mutex

	producerSema chan struct{}

	wg   sync.WaitGroup
	quit chan struct{}
}

// newMsgStream creates a new instance of a chanMsgStream for a particular
// channel identified by its channel ID. bufSize is the max number of messages
// that should be buffered in the internal queue. Callers should set this to a
// sane value that avoids blocking unnecessarily, but doesn't allow an
// unbounded amount of memory to be allocated to buffer incoming messages.
func newMsgStream(p *peer, startMsg, stopMsg string, bufSize uint32,
	apply func(lnwire.Message)) *msgStream {

	stream := &msgStream{
		peer:         p,
		apply:        apply,
		startMsg:     startMsg,
		stopMsg:      stopMsg,
		producerSema: make(chan struct{}, bufSize),
		quit:         make(chan struct{}),
	}
	stream.msgCond = sync.NewCond(&stream.mtx)

	// Before we return the active stream, we'll populate the producer's
	// semaphore channel. We'll use this to ensure that the producer won't
	// attempt to allocate memory in the queue for an item until it has
	// sufficient extra space.
	for i := uint32(0); i < bufSize; i++ {
		stream.producerSema <- struct{}{}
	}

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
	defer atomic.StoreInt32(&ms.streamShutdown, 1)

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
			case <-ms.peer.quit:
				ms.msgCond.L.Unlock()
				return
			case <-ms.quit:
				ms.msgCond.L.Unlock()
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

		// We've just successfully processed an item, so we'll signal
		// to the producer that a new slot in the buffer. We'll use
		// this to bound the size of the buffer to avoid allowing it to
		// grow indefinitely.
		select {
		case ms.producerSema <- struct{}{}:
		case <-ms.peer.quit:
			return
		case <-ms.quit:
			return
		}
	}
}

// AddMsg adds a new message to the msgStream. This function is safe for
// concurrent access.
func (ms *msgStream) AddMsg(msg lnwire.Message) {
	// First, we'll attempt to receive from the producerSema struct. This
	// acts as a sempahore to prevent us from indefinitely buffering
	// incoming items from the wire. Either the msg queue isn't full, and
	// we'll not block, or the queue is full, and we'll block until either
	// we're signalled to quit, or a slot is freed up.
	select {
	case <-ms.producerSema:
	case <-ms.peer.quit:
		return
	case <-ms.quit:
		return
	}

	// Next, we'll lock the condition, and add the message to the end of
	// the message queue.
	ms.msgCond.L.Lock()
	ms.msgs = append(ms.msgs, msg)
	ms.msgCond.L.Unlock()

	// With the message added, we signal to the msgConsumer that there are
	// additional messages to consume.
	ms.msgCond.Signal()
}

// waitUntilLinkActive waits until the target link is active and returns a
// ChannelLink to pass messages to. It accomplishes this by subscribing to
// an ActiveLinkEvent which is emitted by the link when it first starts up.
func waitUntilLinkActive(p *peer, cid lnwire.ChannelID) htlcswitch.ChannelLink {
	// Subscribe to receive channel events.
	//
	// NOTE: If the link is already active by SubscribeChannelEvents, then
	// GetLink will retrieve the link and we can send messages. If the link
	// becomes active between SubscribeChannelEvents and GetLink, then GetLink
	// will retrieve the link. If the link becomes active after GetLink, then
	// we will get an ActiveLinkEvent notification and retrieve the link. If
	// the call to GetLink is before SubscribeChannelEvents, however, there
	// will be a race condition.
	sub, err := p.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		// If we have a non-nil error, then the server is shutting down and we
		// can exit here and return nil. This means no message will be delivered
		// to the link.
		return nil
	}
	defer sub.Cancel()

	// The link may already be active by this point, and we may have missed the
	// ActiveLinkEvent. Check if the link exists.
	link, _ := p.server.htlcSwitch.GetLink(cid)
	if link != nil {
		return link
	}

	// If the link is nil, we must wait for it to be active.
	for {
		select {
		// A new event has been sent by the ChannelNotifier. We first check
		// whether the event is an ActiveLinkEvent. If it is, we'll check
		// that the event is for this channel. Otherwise, we discard the
		// message.
		case e := <-sub.Updates():
			event, ok := e.(channelnotifier.ActiveLinkEvent)
			if !ok {
				// Ignore this notification.
				continue
			}

			chanPoint := event.ChannelPoint

			// Check whether the retrieved chanPoint matches the target
			// channel id.
			if !cid.IsChanPoint(chanPoint) {
				continue
			}

			// The link shouldn't be nil as we received an
			// ActiveLinkEvent. If it is nil, we return nil and the
			// calling function should catch it.
			link, _ = p.server.htlcSwitch.GetLink(cid)
			return link

		case <-p.quit:
			return nil
		}
	}
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
		1000,
		func(msg lnwire.Message) {
			// This check is fine because if the link no longer exists, it will
			// be removed from the activeChannels map and subsequent messages
			// shouldn't reach the chan msg stream.
			if chanLink == nil {
				chanLink = waitUntilLinkActive(p, cid)

				// If the link is still not active and the calling function
				// errored out, just return.
				if chanLink == nil {
					return
				}
			}

			// In order to avoid unnecessarily delivering message
			// as the peer is exiting, we'll check quickly to see
			// if we need to exit.
			select {
			case <-p.quit:
				return
			default:
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
		1000,
		func(msg lnwire.Message) {
			p.server.authGossiper.ProcessRemoteAnnouncement(msg, p)
		},
	)
}

// readHandler is responsible for reading messages off the wire in series, then
// properly dispatching the handling of the message to the proper subsystem.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) readHandler() {
	defer p.wg.Done()

	// We'll stop the timer after a new messages is received, and also
	// reset it after we process the next message.
	idleTimer := time.AfterFunc(idleTimeout, func() {
		err := fmt.Errorf("peer %s no answer for %s -- disconnecting",
			p, idleTimeout)
		p.Disconnect(err)
	})

	// Initialize our negotiated gossip sync method before reading messages
	// off the wire. When using gossip queries, this ensures a gossip
	// syncer is active by the time query messages arrive.
	//
	// TODO(conner): have peer store gossip syncer directly and bypass
	// gossiper?
	p.initGossipSync()

	discStream := newDiscMsgStream(p)
	discStream.Start()
	defer discStream.Stop()
out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, err := p.readNextMessage()
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		if err != nil {
			peerLog.Infof("unable to read message from %v: %v",
				p, err)

			// If we could not read our peer's message due to an
			// unknown type or invalid alias, we continue processing
			// as normal. We store unknown message and address
			// types, as they may provide debugging insight.
			switch e := err.(type) {
			// If this is just a message we don't yet recognize,
			// we'll continue processing as normal as this allows
			// us to introduce new messages in a forwards
			// compatible manner.
			case *lnwire.UnknownMessage:
				p.storeError(e)
				idleTimer.Reset(idleTimeout)
				continue

			// If they sent us an address type that we don't yet
			// know of, then this isn't a dire error, so we'll
			// simply continue parsing the remainder of their
			// messages.
			case *lnwire.ErrUnknownAddrType:
				p.storeError(e)
				idleTimer.Reset(idleTimeout)
				continue

			// If the NodeAnnouncement has an invalid alias, then
			// we'll log that error above and continue so we can
			// continue to read messages from the peer. We do not
			// store this error because it is of little debugging
			// value.
			case *lnwire.ErrInvalidNodeAlias:
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
			targetChan   lnwire.ChannelID
			isLinkUpdate bool
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
			p.server.fundingMgr.processFundingOpen(msg, p)
		case *lnwire.AcceptChannel:
			p.server.fundingMgr.processFundingAccept(msg, p)
		case *lnwire.FundingCreated:
			p.server.fundingMgr.processFundingCreated(msg, p)
		case *lnwire.FundingSigned:
			p.server.fundingMgr.processFundingSigned(msg, p)
		case *lnwire.FundingLocked:
			p.server.fundingMgr.processFundingLocked(msg, p)

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
			targetChan = msg.ChanID
			isLinkUpdate = p.handleError(msg)

		case *lnwire.ChannelReestablish:
			targetChan = msg.ChanID
			isLinkUpdate = p.isActiveChannel(targetChan)

			// If we failed to find the link in question, and the
			// message received was a channel sync message, then
			// this might be a peer trying to resync closed channel.
			// In this case we'll try to resend our last channel
			// sync message, such that the peer can recover funds
			// from the closed channel.
			if !isLinkUpdate {
				err := p.resendChanSyncMsg(targetChan)
				if err != nil {
					// TODO(halseth): send error to peer?
					peerLog.Errorf("resend failed: %v",
						err)
				}
			}

		case LinkUpdater:
			targetChan = msg.TargetChanID()
			isLinkUpdate = p.isActiveChannel(targetChan)

		case *lnwire.ChannelUpdate,
			*lnwire.ChannelAnnouncement,
			*lnwire.NodeAnnouncement,
			*lnwire.AnnounceSignatures,
			*lnwire.GossipTimestampRange,
			*lnwire.QueryShortChanIDs,
			*lnwire.QueryChannelRange,
			*lnwire.ReplyChannelRange,
			*lnwire.ReplyShortChanIDsEnd:

			discStream.AddMsg(msg)

		default:
			// If the message we received is unknown to us, store
			// the type to track the failure.
			err := fmt.Errorf("unknown message type %v received",
				uint16(msg.MsgType()))
			p.storeError(err)

			peerLog.Errorf("peer: %v, %v", p, err)
		}

		if isLinkUpdate {
			// If this is a channel update, then we need to feed it
			// into the channel's in-order message stream.
			chanStream, ok := p.activeMsgStreams[targetChan]
			if !ok {
				// If a stream hasn't yet been created, then
				// we'll do so, add it to the map, and finally
				// start it.
				chanStream = newChanMsgStream(p, targetChan)
				p.activeMsgStreams[targetChan] = chanStream
				chanStream.Start()
				defer chanStream.Stop()
			}

			// With the stream obtained, add the message to the
			// stream so we can continue processing message.
			chanStream.AddMsg(nextMsg)
		}

		idleTimer.Reset(idleTimeout)
	}

	p.Disconnect(errors.New("read handler closed"))

	peerLog.Tracef("readHandler for peer %v done", p)
}

// isActiveChannel returns true if the provided channel id is active, otherwise
// returns false.
func (p *peer) isActiveChannel(chanID lnwire.ChannelID) bool {
	p.activeChanMtx.RLock()
	_, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()
	return ok
}

// storeError stores an error in our peer's buffer of recent errors with the
// current timestamp. Errors are only stored if we have at least one active
// channel with the peer to mitigate dos attack vectors where a peer costlessly
// connects to us and spams us with errors.
func (p *peer) storeError(err error) {
	var haveChannels bool

	p.activeChanMtx.RLock()
	for _, channel := range p.activeChannels {
		// Pending channels will be nil in the activeChannels map.
		if channel == nil {
			continue
		}

		haveChannels = true
		break
	}
	p.activeChanMtx.RUnlock()

	// If we do not have any active channels with the peer, we do not store
	// errors as a dos mitigation.
	if !haveChannels {
		peerLog.Tracef("no channels with peer: %v, not storing err", p)
		return
	}

	p.errorBuffer.Add(
		&timestampedError{timestamp: time.Now(), error: err},
	)
}

// handleError processes an error message read from the remote peer. The boolean
// returns indicates whether the message should be delivered to a targeted peer.
// It stores the error we received from the peer in memory if we have a channel
// open with the peer.
//
// NOTE: This method should only be called from within the readHandler.
func (p *peer) handleError(msg *lnwire.Error) bool {
	key := p.addr.IdentityKey

	// Store the error we have received.
	p.storeError(msg)

	switch {

	// In the case of an all-zero channel ID we want to forward the error to
	// all channels with this peer.
	case msg.ChanID == lnwire.ConnectionWideID:
		for _, chanStream := range p.activeMsgStreams {
			chanStream.AddMsg(msg)
		}
		return false

	// If the channel ID for the error message corresponds to a pending
	// channel, then the funding manager will handle the error.
	case p.server.fundingMgr.IsPendingChannel(msg.ChanID, key):
		p.server.fundingMgr.processFundingError(msg, key)
		return false

	// If not we hand the error to the channel link for this channel.
	case p.isActiveChannel(msg.ChanID):
		return true

	default:
		return false
	}
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
		return fmt.Sprintf("%v", msg.Error())

	case *lnwire.AnnounceSignatures:
		return fmt.Sprintf("chan_id=%v, short_chan_id=%v", msg.ChannelID,
			msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelAnnouncement:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v",
			msg.ChainHash, msg.ShortChannelID.ToUint64())

	case *lnwire.ChannelUpdate:
		return fmt.Sprintf("chain_hash=%v, short_chan_id=%v, "+
			"mflags=%v, cflags=%v, update_time=%v", msg.ChainHash,
			msg.ShortChannelID.ToUint64(), msg.MessageFlags,
			msg.ChannelFlags, time.Unix(int64(msg.Timestamp), 0))

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

	case *lnwire.ReplyShortChanIDsEnd:
		return fmt.Sprintf("chain_hash=%v, complete=%v", msg.ChainHash,
			msg.Complete)

	case *lnwire.ReplyChannelRange:
		return fmt.Sprintf("start_height=%v, end_height=%v, "+
			"num_chans=%v, encoding=%v", msg.FirstBlockHeight,
			msg.LastBlockHeight(), len(msg.ShortChanIDs),
			msg.EncodingType)

	case *lnwire.QueryShortChanIDs:
		return fmt.Sprintf("chain_hash=%v, encoding=%v, num_chans=%v",
			msg.ChainHash, msg.EncodingType, len(msg.ShortChanIDs))

	case *lnwire.QueryChannelRange:
		return fmt.Sprintf("chain_hash=%v, start_height=%v, "+
			"end_height=%v", msg.ChainHash, msg.FirstBlockHeight,
			msg.LastBlockHeight())

	case *lnwire.GossipTimestampRange:
		return fmt.Sprintf("chain_hash=%v, first_stamp=%v, "+
			"stamp_range=%v", msg.ChainHash,
			time.Unix(int64(msg.FirstTimestamp), 0),
			msg.TimestampRange)

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

// writeMessage writes and flushes the target lnwire.Message to the remote peer.
// If the passed message is nil, this method will only try to flush an existing
// message buffered on the connection. It is safe to recall this method with a
// nil message iff a timeout error is returned. This will continue to flush the
// pending message to the wire.
func (p *peer) writeMessage(msg lnwire.Message) error {
	// Simply exit if we're shutting down.
	if atomic.LoadInt32(&p.disconnect) != 0 {
		return lnpeer.ErrPeerExiting
	}

	// Only log the message on the first attempt.
	if msg != nil {
		p.logWireMessage(msg, false)
	}

	noiseConn, ok := p.conn.(*brontide.Conn)
	if !ok {
		return fmt.Errorf("brontide.Conn required to write messages")
	}

	flushMsg := func() error {
		// Ensure the write deadline is set before we attempt to send
		// the message.
		writeDeadline := time.Now().Add(writeMessageTimeout)
		err := noiseConn.SetWriteDeadline(writeDeadline)
		if err != nil {
			return err
		}

		// Flush the pending message to the wire. If an error is
		// encountered, e.g. write timeout, the number of bytes written
		// so far will be returned.
		n, err := noiseConn.Flush()

		// Record the number of bytes written on the wire, if any.
		if n > 0 {
			atomic.AddUint64(&p.bytesSent, uint64(n))
		}

		return err
	}

	// If the current message has already been serialized, encrypted, and
	// buffered on the underlying connection we will skip straight to
	// flushing it to the wire.
	if msg == nil {
		return flushMsg()
	}

	// Otherwise, this is a new message. We'll acquire a write buffer to
	// serialize the message and buffer the ciphertext on the connection.
	err := p.writePool.Submit(func(buf *bytes.Buffer) error {
		// Using a buffer allocated by the write pool, encode the
		// message directly into the buffer.
		_, writeErr := lnwire.WriteMessage(buf, msg, 0)
		if writeErr != nil {
			return writeErr
		}

		// Finally, write the message itself in a single swoop. This
		// will buffer the ciphertext on the underlying connection. We
		// will defer flushing the message until the write pool has been
		// released.
		return noiseConn.WriteMessage(buf.Bytes())
	})
	if err != nil {
		return err
	}

	return flushMsg()
}

// writeHandler is a goroutine dedicated to reading messages off of an incoming
// queue, and writing them out to the wire. This goroutine coordinates with the
// queueHandler in order to ensure the incoming message queue is quickly
// drained.
//
// NOTE: This method MUST be run as a goroutine.
func (p *peer) writeHandler() {
	// We'll stop the timer after a new messages is sent, and also reset it
	// after we process the next message.
	idleTimer := time.AfterFunc(idleTimeout, func() {
		err := fmt.Errorf("peer %s no write for %s -- disconnecting",
			p, idleTimeout)
		p.Disconnect(err)
	})

	var exitErr error

out:
	for {
		select {
		case outMsg := <-p.sendQueue:
			// If we're about to send a ping message, then log the
			// exact time in which we send the message so we can
			// use the delay as a rough estimate of latency to the
			// remote peer.
			if _, ok := outMsg.msg.(*lnwire.Ping); ok {
				// TODO(roasbeef): do this before the write?
				// possibly account for processing within func?
				now := time.Now().UnixNano()
				atomic.StoreInt64(&p.pingLastSend, now)
			}

			// Record the time at which we first attempt to send the
			// message.
			startTime := time.Now()

		retry:
			// Write out the message to the socket. If a timeout
			// error is encountered, we will catch this and retry
			// after backing off in case the remote peer is just
			// slow to process messages from the wire.
			err := p.writeMessage(outMsg.msg)
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				peerLog.Debugf("Write timeout detected for "+
					"peer %s, first write for message "+
					"attempted %v ago", p,
					time.Since(startTime))

				// If we received a timeout error, this implies
				// that the message was buffered on the
				// connection successfully and that a flush was
				// attempted. We'll set the message to nil so
				// that on a subsequent pass we only try to
				// flush the buffered message, and forgo
				// reserializing or reencrypting it.
				outMsg.msg = nil

				goto retry
			}

			// The write succeeded, reset the idle timer to prevent
			// us from disconnecting the peer.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idleTimeout)

			// If the peer requested a synchronous write, respond
			// with the error.
			if outMsg.errChan != nil {
				outMsg.errChan <- err
			}

			if err != nil {
				exitErr = fmt.Errorf("unable to write "+
					"message: %v", err)
				break out
			}

		case <-p.quit:
			exitErr = lnpeer.ErrPeerExiting
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

	// priorityMsgs holds an in order list of messages deemed high-priority
	// to be added to the sendQueue. This predominately includes messages
	// from the funding manager and htlcswitch.
	priorityMsgs := list.New()

	// lazyMsgs holds an in order list of messages deemed low-priority to be
	// added to the sendQueue only after all high-priority messages have
	// been queued. This predominately includes messages from the gossiper.
	lazyMsgs := list.New()

	for {
		// Examine the front of the priority queue, if it is empty check
		// the low priority queue.
		elem := priorityMsgs.Front()
		if elem == nil {
			elem = lazyMsgs.Front()
		}

		if elem != nil {
			front := elem.Value.(outgoingMsg)

			// There's an element on the queue, try adding
			// it to the sendQueue. We also watch for
			// messages on the outgoingQueue, in case the
			// writeHandler cannot accept messages on the
			// sendQueue.
			select {
			case p.sendQueue <- front:
				if front.priority {
					priorityMsgs.Remove(elem)
				} else {
					lazyMsgs.Remove(elem)
				}
			case msg := <-p.outgoingQueue:
				if msg.priority {
					priorityMsgs.PushBack(msg)
				} else {
					lazyMsgs.PushBack(msg)
				}
			case <-p.quit:
				return
			}
		} else {
			// If there weren't any messages to send to the
			// writeHandler, then we'll accept a new message
			// into the queue from outside sub-systems.
			select {
			case msg := <-p.outgoingQueue:
				if msg.priority {
					priorityMsgs.PushBack(msg)
				} else {
					lazyMsgs.PushBack(msg)
				}
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

// queueMsg adds the lnwire.Message to the back of the high priority send queue.
// If the errChan is non-nil, an error is sent back if the msg failed to queue
// or failed to write, and nil otherwise.
func (p *peer) queueMsg(msg lnwire.Message, errChan chan error) {
	p.queue(true, msg, errChan)
}

// queueMsgLazy adds the lnwire.Message to the back of the low priority send
// queue. If the errChan is non-nil, an error is sent back if the msg failed to
// queue or failed to write, and nil otherwise.
func (p *peer) queueMsgLazy(msg lnwire.Message, errChan chan error) {
	p.queue(false, msg, errChan)
}

// queue sends a given message to the queueHandler using the passed priority. If
// the errChan is non-nil, an error is sent back if the msg failed to queue or
// failed to write, and nil otherwise.
func (p *peer) queue(priority bool, msg lnwire.Message, errChan chan error) {
	select {
	case p.outgoingQueue <- outgoingMsg{priority, msg, errChan}:
	case <-p.quit:
		peerLog.Tracef("Peer shutting down, could not enqueue msg.")
		if errChan != nil {
			errChan <- lnpeer.ErrPeerExiting
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
		// If the activeChan is nil, then we skip it as the channel is pending.
		if activeChan == nil {
			continue
		}

		// We'll only return a snapshot for channels that are
		// *immedately* available for routing payments over.
		if activeChan.RemoteNextRevocation() == nil {
			continue
		}

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

	// reenableTimeout will fire once after the configured channel status
	// interval  has elapsed. This will trigger us to sign new channel
	// updates and broadcast them with the "disabled" flag unset.
	reenableTimeout := time.After(p.chanActiveTimeout)

out:
	for {
		select {
		// A new channel has arrived which means we've just completed a
		// funding workflow. We'll initialize the necessary local
		// state, and notify the htlc switch of a new link.
		case newChanReq := <-p.newChannels:
			newChan := newChanReq.channel
			chanPoint := &newChan.FundingOutpoint
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

			// Only update RemoteNextRevocation if the channel is in the
			// activeChannels map and if we added the link to the switch.
			// Only active channels will be added to the switch.
			p.activeChanMtx.Lock()
			currentChan, ok := p.activeChannels[chanID]
			if ok && currentChan != nil {
				peerLog.Infof("Already have ChannelPoint(%v), "+
					"ignoring.", chanPoint)

				p.activeChanMtx.Unlock()
				close(newChanReq.err)

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

				nextRevoke := newChan.RemoteNextRevocation
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
			lnChan, err := lnwallet.NewLightningChannel(
				p.server.cc.signer, newChan, p.server.sigPool,
			)
			if err != nil {
				p.activeChanMtx.Unlock()
				err := fmt.Errorf("unable to create "+
					"LightningChannel: %v", err)
				peerLog.Errorf(err.Error())

				newChanReq.err <- err
				continue
			}

			// This refreshes the activeChannels entry if the link was not in
			// the switch, also populates for new entries.
			p.activeChannels[chanID] = lnChan
			p.addedChannels[chanID] = struct{}{}
			p.activeChanMtx.Unlock()

			peerLog.Infof("New channel active ChannelPoint(%v) "+
				"with NodeKey(%x)", chanPoint, p.PubKey())

			// Next, we'll assemble a ChannelLink along with the
			// necessary items it needs to function.
			//
			// TODO(roasbeef): panic on below?
			_, currentHeight, err := p.server.cc.chainIO.GetBestBlock()
			if err != nil {
				err := fmt.Errorf("unable to get best "+
					"block: %v", err)
				peerLog.Errorf(err.Error())

				newChanReq.err <- err
				continue
			}
			chainEvents, err := p.server.chainArb.SubscribeChannelEvents(
				*chanPoint,
			)
			if err != nil {
				err := fmt.Errorf("unable to subscribe to "+
					"chain events: %v", err)
				peerLog.Errorf(err.Error())

				newChanReq.err <- err
				continue
			}

			// We'll query the localChanCfg of the new channel to determine the
			// minimum HTLC value that can be forwarded. For the maximum HTLC
			// value that can be forwarded and fees we'll use the default
			// values, as they currently are always set to the default values
			// at initial channel creation. Note that the maximum HTLC value
			// defaults to the cap on the total value of outstanding HTLCs.
			fwdMinHtlc := lnChan.FwdMinHtlc()
			defaultPolicy := p.server.cc.routingPolicy
			forwardingPolicy := &htlcswitch.ForwardingPolicy{
				MinHTLCOut:    fwdMinHtlc,
				MaxHTLC:       newChan.LocalChanCfg.MaxPendingAmount,
				BaseFee:       defaultPolicy.BaseFee,
				FeeRate:       defaultPolicy.FeeRate,
				TimeLockDelta: defaultPolicy.TimeLockDelta,
			}

			// If we've reached this point, there are two possible scenarios.
			// If the channel was in the active channels map as nil, then it
			// was loaded from disk and we need to send reestablish. Else,
			// it was not loaded from disk and we don't need to send
			// reestablish as this is a fresh channel.
			shouldReestablish := ok

			// Create the link and add it to the switch.
			err = p.addLink(
				chanPoint, lnChan, forwardingPolicy,
				chainEvents, currentHeight, shouldReestablish,
			)
			if err != nil {
				err := fmt.Errorf("can't register new channel "+
					"link(%v) with NodeKey(%x)", chanPoint,
					p.PubKey())
				peerLog.Errorf(err.Error())

				newChanReq.err <- err
				continue
			}

			close(newChanReq.err)

		// We've just received a local request to close an active
		// channel. If will either kick of a cooperative channel
		// closure negotiation, or be a notification of a breached
		// contract that should be abandoned.
		case req := <-p.localCloseChanReqs:
			p.handleLocalCloseReq(req)

		// We've received a link failure from a link that was added to
		// the switch. This will initiate the teardown of the link, and
		// initiate any on-chain closures if necessary.
		case failure := <-p.linkFailures:
			p.handleLinkFailure(failure)

		// We've received a new cooperative channel closure related
		// message from the remote peer, we'll use this message to
		// advance the chan closer state machine.
		case closeMsg := <-p.chanCloseMsgs:
			// We'll now fetch the matching closing state machine
			// in order to continue, or finalize the channel
			// closure process.
			chanCloser, err := p.fetchActiveChanCloser(closeMsg.cid)
			if err != nil {
				// If the channel is not known to us, we'll
				// simply ignore this message.
				if err == ErrChannelNotFound {
					continue
				}

				peerLog.Errorf("Unable to respond to remote "+
					"close msg: %v", err)

				errMsg := &lnwire.Error{
					ChanID: closeMsg.cid,
					Data:   lnwire.ErrorData(err.Error()),
				}
				p.queueMsg(errMsg, nil)
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

		// The channel reannounce delay has elapsed, broadcast the
		// reenabled channel updates to the network. This should only
		// fire once, so we set the reenableTimeout channel to nil to
		// mark it for garbage collection. If the peer is torn down
		// before firing, reenabling will not be attempted.
		// TODO(conner): consolidate reenables timers inside chan status
		// manager
		case <-reenableTimeout:
			p.reenableActiveChannels()

			// Since this channel will never fire again during the
			// lifecycle of the peer, we nil the channel to mark it
			// eligible for garbage collection, and make this
			// explicitly ineligible to receive in future calls to
			// select. This also shaves a few CPU cycles since the
			// select will ignore this case entirely.
			reenableTimeout = nil

		case <-p.quit:

			// As, we've been signalled to exit, we'll reset all
			// our active channel back to their default state.
			p.activeChanMtx.Lock()
			for _, channel := range p.activeChannels {
				// If the channel is nil, continue as it's a pending channel.
				if channel == nil {
					continue
				}

				channel.ResetState()
			}
			p.activeChanMtx.Unlock()

			break out
		}
	}
}

// reenableActiveChannels searches the index of channels maintained with this
// peer, and reenables each public, non-pending channel. This is done at the
// gossip level by broadcasting a new ChannelUpdate with the disabled bit unset.
// No message will be sent if the channel is already enabled.
func (p *peer) reenableActiveChannels() {
	// First, filter all known channels with this peer for ones that are
	// both public and not pending.
	var activePublicChans []wire.OutPoint
	p.activeChanMtx.RLock()
	for chanID, lnChan := range p.activeChannels {
		// If the lnChan is nil, continue as this is a pending channel.
		if lnChan == nil {
			continue
		}

		dbChan := lnChan.State()
		isPublic := dbChan.ChannelFlags&lnwire.FFAnnounceChannel != 0
		if !isPublic || dbChan.IsPending {
			continue
		}

		// We'll also skip any channels added during this peer's
		// lifecycle since they haven't waited out the timeout. Their
		// first announcement will be enabled, and the chan status
		// manager will begin monitoring them passively since they exist
		// in the database.
		if _, ok := p.addedChannels[chanID]; ok {
			continue
		}

		activePublicChans = append(
			activePublicChans, dbChan.FundingOutpoint,
		)
	}
	p.activeChanMtx.RUnlock()

	// For each of the public, non-pending channels, set the channel
	// disabled bit to false and send out a new ChannelUpdate. If this
	// channel is already active, the update won't be sent.
	for _, chanPoint := range activePublicChans {
		err := p.server.chanStatusMgr.RequestEnable(chanPoint)
		if err != nil {
			srvrLog.Errorf("Unable to enable channel %v: %v",
				chanPoint, err)
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

	// If the channel isn't in the map or the channel is nil, return
	// ErrChannelNotFound as the channel is pending.
	if !ok || channel == nil {
		return nil, ErrChannelNotFound
	}

	// We'll attempt to look up the matching state machine, if we can't
	// find one then this means that the remote party is initiating a
	// cooperative channel closure.
	chanCloser, ok := p.activeChanCloses[chanID]
	if !ok {
		// If we need to create a chan closer for the first time, then
		// we'll check to ensure that the channel is even in the proper
		// state to allow a co-op channel closure.
		if len(channel.ActiveHtlcs()) != 0 {
			return nil, fmt.Errorf("cannot co-op close " +
				"channel w/ active htlcs")
		}

		// We'll create a valid closing state machine in order to respond to the
		// initiated cooperative channel closure. First, we set the delivery
		// script that our funds will be paid out to. If an upfront shutdown script
		// was set, we will use it. Otherwise, we get a fresh delivery script.
		deliveryScript := channel.LocalUpfrontShutdownScript()
		if len(deliveryScript) == 0 {
			var err error
			deliveryScript, err = p.genDeliveryScript()
			if err != nil {
				peerLog.Errorf("unable to gen delivery script: %v", err)
				return nil, fmt.Errorf("close addr unavailable")
			}
		}

		// In order to begin fee negotiations, we'll first compute our
		// target ideal fee-per-kw. We'll set this to a lax value, as
		// we weren't the ones that initiated the channel closure.
		feePerKw, err := p.server.cc.feeEstimator.EstimateFeePerKW(6)
		if err != nil {
			peerLog.Errorf("unable to query fee estimator: %v", err)

			return nil, fmt.Errorf("unable to estimate fee")
		}

		_, startingHeight, err := p.server.cc.chainIO.GetBestBlock()
		if err != nil {
			peerLog.Errorf("unable to obtain best block: %v", err)
			return nil, fmt.Errorf("cannot obtain best block")
		}

		chanCloser = newChannelCloser(
			chanCloseCfg{
				channel:           channel,
				unregisterChannel: p.server.htlcSwitch.RemoveLink,
				broadcastTx:       p.server.cc.wallet.PublishTransaction,
				disableChannel:    p.server.chanStatusMgr.RequestDisable,
				disconnect: func() error {
					return p.server.DisconnectPeer(p.IdentityKey())
				},
				quit: p.quit,
			},
			deliveryScript,
			feePerKw,
			uint32(startingHeight),
			nil,
			false,
		)
		p.activeChanCloses[chanID] = chanCloser
	}

	return chanCloser, nil
}

// chooseDeliveryScript takes two optionally set shutdown scripts and returns
// a suitable script to close out to. This may be nil if neither script is
// set. If both scripts are set, this function will error if they do not match.
func chooseDeliveryScript(upfront,
	requested lnwire.DeliveryAddress) (lnwire.DeliveryAddress, error) {

	// If no upfront upfront shutdown script was provided, return the user
	// requested address (which may be nil).
	if len(upfront) == 0 {
		return requested, nil
	}

	// If an upfront shutdown script was provided, and the user did not request
	// a custom shutdown script, return the upfront address.
	if len(requested) == 0 {
		return upfront, nil
	}

	// If both an upfront shutdown script and a custom close script were
	// provided, error if the user provided shutdown script does not match
	// the upfront shutdown script (because closing out to a different script
	// would violate upfront shutdown).
	if !bytes.Equal(upfront, requested) {
		return nil, errUpfrontShutdownScriptMismatch
	}

	// The user requested script matches the upfront shutdown script, so we
	// can return it without error.
	return upfront, nil
}

// handleLocalCloseReq kicks-off the workflow to execute a cooperative or
// forced unilateral closure of the channel initiated by a local subsystem.
func (p *peer) handleLocalCloseReq(req *htlcswitch.ChanClose) {
	chanID := lnwire.NewChanIDFromOutPoint(req.ChanPoint)

	p.activeChanMtx.RLock()
	channel, ok := p.activeChannels[chanID]
	p.activeChanMtx.RUnlock()

	// Though this function can't be called for pending channels, we still
	// check whether channel is nil for safety.
	if !ok || channel == nil {
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
		// First, we'll choose a delivery address that we'll use to send the
		// funds to in the case of a successful negotiation.

		// An upfront shutdown and user provided script are both optional,
		// but must be equal if both set  (because we cannot serve a request
		// to close out to a script which violates upfront shutdown). Get the
		// appropriate address to close out to (which may be nil if neither
		// are set) and error if they are both set and do not match.
		deliveryScript, err := chooseDeliveryScript(
			channel.LocalUpfrontShutdownScript(), req.DeliveryScript,
		)
		if err != nil {
			peerLog.Errorf("cannot close channel %v: %v", req.ChanPoint, err)
			req.Err <- err
			return
		}

		// If neither an upfront address or a user set address was
		// provided, generate a fresh script.
		if len(deliveryScript) == 0 {
			deliveryScript, err = p.genDeliveryScript()
			if err != nil {
				peerLog.Errorf(err.Error())
				req.Err <- err
				return
			}
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
				disableChannel:    p.server.chanStatusMgr.RequestDisable,
				disconnect: func() error {
					return p.server.DisconnectPeer(p.IdentityKey())
				},
				quit: p.quit,
			},
			deliveryScript,
			req.TargetFeePerKw,
			uint32(startingHeight),
			req,
			true,
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
		p.WipeChannel(req.ChanPoint)
	}
}

// linkFailureReport is sent to the channelManager whenever a link that was
// added to the switch reports a link failure, and is forced to exit. The report
// houses the necessary information to cleanup the channel state, send back the
// error message, and force close if necessary.
type linkFailureReport struct {
	chanPoint   wire.OutPoint
	chanID      lnwire.ChannelID
	shortChanID lnwire.ShortChannelID
	linkErr     htlcswitch.LinkFailureError
}

// handleLinkFailure processes a link failure report when a link in the switch
// fails. It handles facilitates removal of all channel state within the peer,
// force closing the channel depending on severity, and sending the error
// message back to the remote party.
func (p *peer) handleLinkFailure(failure linkFailureReport) {
	// We begin by wiping the link, which will remove it from the switch,
	// such that it won't be attempted used for any more updates.
	//
	// TODO(halseth): should introduce a way to atomically stop/pause the
	// link and cancel back any adds in its mailboxes such that we can
	// safely force close without the link being added again and updates
	// being applied.
	p.WipeChannel(&failure.chanPoint)

	// If the error encountered was severe enough, we'll now force close the
	// channel to prevent readding it to the switch in the future.
	if failure.linkErr.ForceClose {
		peerLog.Warnf("Force closing link(%v)",
			failure.shortChanID)

		closeTx, err := p.server.chainArb.ForceCloseContract(
			failure.chanPoint,
		)
		if err != nil {
			peerLog.Errorf("unable to force close "+
				"link(%v): %v", failure.shortChanID, err)
		} else {
			peerLog.Infof("channel(%v) force "+
				"closed with txid %v",
				failure.shortChanID, closeTx.TxHash())
		}
	}

	// Send an error to the peer, why we failed the channel.
	if failure.linkErr.ShouldSendToPeer() {
		// If SendData is set, send it to the peer. If not, we'll use
		// the standard error messages in the payload. We only include
		// sendData in the cases where the error data does not contain
		// sensitive information.
		data := []byte(failure.linkErr.Error())
		if failure.linkErr.SendData != nil {
			data = failure.linkErr.SendData
		}
		err := p.SendMessage(true, &lnwire.Error{
			ChanID: failure.chanID,
			Data:   data,
		})
		if err != nil {
			peerLog.Errorf("unable to send msg to "+
				"remote peer: %v", err)
		}
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
	p.WipeChannel(chanPoint)

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
		closeReq.Updates <- &pendingUpdate{
			Txid: closingTxid[:],
		}
	}

	go waitForChanToClose(chanCloser.negotiationHeight, notifier, errChan,
		chanPoint, &closingTxid, closingTx.TxOut[0].PkScript, func() {

			// Respond to the local subsystem which requested the
			// channel closure.
			if closeReq != nil {
				closeReq.Updates <- &channelCloseUpdate{
					ClosingTxid: closingTxid[:],
					Success:     true,
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
	closingTxID *chainhash.Hash, closeScript []byte, cb func()) {

	peerLog.Infof("Waiting for confirmation of cooperative close of "+
		"ChannelPoint(%v) with txid: %v", chanPoint,
		closingTxID)

	// TODO(roasbeef): add param for num needed confs
	confNtfn, err := notifier.RegisterConfirmationsNtfn(
		closingTxID, closeScript, 1, bestHeight,
	)
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

// WipeChannel removes the passed channel point from all indexes associated with
// the peer, and the switch.
func (p *peer) WipeChannel(chanPoint *wire.OutPoint) {
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	p.activeChanMtx.Lock()
	delete(p.activeChannels, chanID)
	p.activeChanMtx.Unlock()

	// Instruct the HtlcSwitch to close this link as the channel is no
	// longer active.
	p.server.htlcSwitch.RemoveLink(chanID)
}

// handleInitMsg handles the incoming init message which contains global and
// local features vectors. If feature vectors are incompatible then disconnect.
func (p *peer) handleInitMsg(msg *lnwire.Init) error {
	// First, merge any features from the legacy global features field into
	// those presented in the local features fields.
	err := msg.Features.Merge(msg.GlobalFeatures)
	if err != nil {
		return fmt.Errorf("unable to merge legacy global features: %v",
			err)
	}

	// Then, finalize the remote feature vector providing the flatteneed
	// feature bit namespace.
	p.remoteFeatures = lnwire.NewFeatureVector(
		msg.Features, lnwire.Features,
	)

	// Now that we have their features loaded, we'll ensure that they
	// didn't set any required bits that we don't know of.
	err = feature.ValidateRequired(p.remoteFeatures)
	if err != nil {
		return fmt.Errorf("invalid remote features: %v", err)
	}

	// Ensure the remote party's feature vector contains all transistive
	// dependencies. We know ours are are correct since they are validated
	// during the feature manager's instantiation.
	err = feature.ValidateDeps(p.remoteFeatures)
	if err != nil {
		return fmt.Errorf("invalid remote features: %v", err)
	}

	// Now that we know we understand their requirements, we'll check to
	// see if they don't support anything that we deem to be mandatory.
	switch {
	case !p.remoteFeatures.HasFeature(lnwire.DataLossProtectRequired):
		return fmt.Errorf("data loss protection required")
	}

	return nil
}

// LocalFeatures returns the set of global features that has been advertised by
// the local node. This allows sub-systems that use this interface to gate their
// behavior off the set of negotiated feature bits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) LocalFeatures() *lnwire.FeatureVector {
	return p.features
}

// RemoteFeatures returns the set of global features that has been advertised by
// the remote node. This allows sub-systems that use this interface to gate
// their behavior off the set of negotiated feature bits.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) RemoteFeatures() *lnwire.FeatureVector {
	return p.remoteFeatures
}

// sendInitMsg sends init message to remote peer which contains our currently
// supported local and global features.
func (p *peer) sendInitMsg() error {
	msg := lnwire.NewInitMessage(
		p.legacyFeatures.RawFeatureVector,
		p.features.RawFeatureVector,
	)

	return p.writeMessage(msg)
}

// resendChanSyncMsg will attempt to find a channel sync message for the closed
// channel and resend it to our peer.
func (p *peer) resendChanSyncMsg(cid lnwire.ChannelID) error {
	// If we already re-sent the mssage for this channel, we won't do it
	// again.
	if _, ok := p.resentChanSyncMsg[cid]; ok {
		return nil
	}

	// Check if we have any channel sync messages stored for this channel.
	c, err := p.server.chanDB.FetchClosedChannelForID(cid)
	if err != nil {
		return fmt.Errorf("unable to fetch channel sync messages for "+
			"peer %v: %v", p, err)
	}

	if c.LastChanSyncMsg == nil {
		return fmt.Errorf("no chan sync message stored for channel %v",
			cid)
	}

	if !c.RemotePub.IsEqual(p.IdentityKey()) {
		return fmt.Errorf("ignoring channel reestablish from "+
			"peer=%x", p.IdentityKey())
	}

	peerLog.Debugf("Re-sending channel sync message for channel %v to "+
		"peer %v", cid, p)

	if err := p.SendMessage(true, c.LastChanSyncMsg); err != nil {
		return fmt.Errorf("failed resending channel sync "+
			"message to peer %v: %v", p, err)
	}

	peerLog.Debugf("Re-sent channel sync message for channel %v to peer "+
		"%v", cid, p)

	// Note down that we sent the message, so we won't resend it again for
	// this connection.
	p.resentChanSyncMsg[cid] = struct{}{}

	return nil
}

// SendMessage sends a variadic number of high-priority message to remote peer.
// The first argument denotes if the method should block until the messages have
// been sent to the remote peer or an error is returned, otherwise it returns
// immediately after queuing.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) SendMessage(sync bool, msgs ...lnwire.Message) error {
	return p.sendMessage(sync, true, msgs...)
}

// SendMessageLazy sends a variadic number of low-priority message to remote
// peer. The first argument denotes if the method should block until the
// messages have been sent to the remote peer or an error is returned, otherwise
// it returns immediately after queueing.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) SendMessageLazy(sync bool, msgs ...lnwire.Message) error {
	return p.sendMessage(sync, false, msgs...)
}

// sendMessage queues a variadic number of messages using the passed priority
// to the remote peer. If sync is true, this method will block until the
// messages have been sent to the remote peer or an error is returned, otherwise
// it returns immediately after queueing.
func (p *peer) sendMessage(sync, priority bool, msgs ...lnwire.Message) error {
	// Add all incoming messages to the outgoing queue. A list of error
	// chans is populated for each message if the caller requested a sync
	// send.
	var errChans []chan error
	if sync {
		errChans = make([]chan error, 0, len(msgs))
	}
	for _, msg := range msgs {
		// If a sync send was requested, create an error chan to listen
		// for an ack from the writeHandler.
		var errChan chan error
		if sync {
			errChan = make(chan error, 1)
			errChans = append(errChans, errChan)
		}

		if priority {
			p.queueMsg(msg, errChan)
		} else {
			p.queueMsgLazy(msg, errChan)
		}
	}

	// Wait for all replies from the writeHandler. For async sends, this
	// will be a NOP as the list of error chans is nil.
	for _, errChan := range errChans {
		select {
		case err := <-errChan:
			return err
		case <-p.quit:
			return lnpeer.ErrPeerExiting
		case <-p.server.quit:
			return lnpeer.ErrPeerExiting
		}
	}

	return nil
}

// PubKey returns the pubkey of the peer in compressed serialized format.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) PubKey() [33]byte {
	return p.pubKeyBytes
}

// IdentityKey returns the public key of the remote peer.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) IdentityKey() *btcec.PublicKey {
	return p.addr.IdentityKey
}

// Address returns the network address of the remote peer.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) Address() net.Addr {
	return p.addr.Address
}

// AddNewChannel adds a new channel to the peer. The channel should fail to be
// added if the cancel channel is closed.
//
// NOTE: Part of the lnpeer.Peer interface.
func (p *peer) AddNewChannel(channel *channeldb.OpenChannel,
	cancel <-chan struct{}) error {

	errChan := make(chan error, 1)
	newChanMsg := &newChannelMsg{
		channel: channel,
		err:     errChan,
	}

	select {
	case p.newChannels <- newChanMsg:
	case <-cancel:
		return errors.New("canceled adding new channel")
	case <-p.quit:
		return lnpeer.ErrPeerExiting
	}

	// We pause here to wait for the peer to recognize the new channel
	// before we close the channel barrier corresponding to the channel.
	select {
	case err := <-errChan:
		return err
	case <-p.quit:
		return lnpeer.ErrPeerExiting
	}
}

// StartTime returns the time at which the connection was established if the
// peer started successfully, and zero otherwise.
func (p *peer) StartTime() time.Time {
	return p.startTime
}

// LinkUpdater is an interface implemented by most messages in BOLT 2 that are
// allowed to update the channel state.
type LinkUpdater interface {
	// TargetChanID returns the channel id of the link for which this
	// message is intended.
	TargetChanID() lnwire.ChannelID
}

// TODO(roasbeef): make all start/stop mutexes a CAS
