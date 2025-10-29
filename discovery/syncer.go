package discovery

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/time/rate"
)

// SyncerType encapsulates the different types of syncing mechanisms for a
// gossip syncer.
type SyncerType uint8

const (
	// ActiveSync denotes that a gossip syncer:
	//
	// 1. Should not attempt to synchronize with the remote peer for
	//    missing channels.
	// 2. Should respond to queries from the remote peer.
	// 3. Should receive new updates from the remote peer.
	//
	// They are started in a chansSynced state in order to accomplish their
	// responsibilities above.
	ActiveSync SyncerType = iota

	// PassiveSync denotes that a gossip syncer:
	//
	// 1. Should not attempt to synchronize with the remote peer for
	//    missing channels.
	// 2. Should respond to queries from the remote peer.
	// 3. Should not receive new updates from the remote peer.
	//
	// They are started in a chansSynced state in order to accomplish their
	// responsibilities above.
	PassiveSync

	// PinnedSync denotes an ActiveSync that doesn't count towards the
	// default active syncer limits and is always active throughout the
	// duration of the peer's connection. Each pinned syncer will begin by
	// performing a historical sync to ensure we are well synchronized with
	// their routing table.
	PinnedSync
)

const (
	// defaultTimestampQueueSize is the size of the timestamp range queue
	// used.
	defaultTimestampQueueSize = 1
)

// String returns a human readable string describing the target SyncerType.
func (t SyncerType) String() string {
	switch t {
	case ActiveSync:
		return "ActiveSync"
	case PassiveSync:
		return "PassiveSync"
	case PinnedSync:
		return "PinnedSync"
	default:
		return fmt.Sprintf("unknown sync type %d", t)
	}
}

// IsActiveSync returns true if the SyncerType should set a GossipTimestampRange
// allowing new gossip messages to be received from the peer.
func (t SyncerType) IsActiveSync() bool {
	switch t {
	case ActiveSync, PinnedSync:
		return true
	default:
		return false
	}
}

// syncerState is an enum that represents the current state of the GossipSyncer.
// As the syncer is a state machine, we'll gate our actions based off of the
// current state and the next incoming message.
type syncerState uint32

const (
	// syncingChans is the default state of the GossipSyncer. We start in
	// this state when a new peer first connects and we don't yet know if
	// we're fully synchronized.
	syncingChans syncerState = iota

	// waitingQueryRangeReply is the second main phase of the GossipSyncer.
	// We enter this state after we send out our first QueryChannelRange
	// reply. We'll stay in this state until the remote party sends us a
	// ReplyShortChanIDsEnd message that indicates they've responded to our
	// query entirely. After this state, we'll transition to
	// waitingQueryChanReply after we send out requests for all the new
	// chan ID's to us.
	waitingQueryRangeReply

	// queryNewChannels is the third main phase of the GossipSyncer.  In
	// this phase we'll send out all of our QueryShortChanIDs messages in
	// response to the new channels that we don't yet know about.
	queryNewChannels

	// waitingQueryChanReply is the fourth main phase of the GossipSyncer.
	// We enter this phase once we've sent off a query chink to the remote
	// peer.  We'll stay in this phase until we receive a
	// ReplyShortChanIDsEnd message which indicates that the remote party
	// has responded to all of our requests.
	waitingQueryChanReply

	// chansSynced is the terminal stage of the GossipSyncer. Once we enter
	// this phase, we'll send out our update horizon, which filters out the
	// set of channel updates that we're interested in. In this state,
	// we'll be able to accept any outgoing messages from the
	// AuthenticatedGossiper, and decide if we should forward them to our
	// target peer based on its update horizon.
	chansSynced

	// syncerIdle is a state in which the gossip syncer can handle external
	// requests to transition or perform historical syncs. It is used as the
	// initial state for pinned syncers, as well as a fallthrough case for
	// chansSynced allowing fully synced peers to facilitate requests.
	syncerIdle
)

// String returns a human readable string describing the target syncerState.
func (s syncerState) String() string {
	switch s {
	case syncingChans:
		return "syncingChans"

	case waitingQueryRangeReply:
		return "waitingQueryRangeReply"

	case queryNewChannels:
		return "queryNewChannels"

	case waitingQueryChanReply:
		return "waitingQueryChanReply"

	case chansSynced:
		return "chansSynced"

	case syncerIdle:
		return "syncerIdle"

	default:
		return "UNKNOWN STATE"
	}
}

const (
	// maxQueryChanRangeReplies specifies the default limit of replies to
	// process for a single QueryChannelRange request.
	maxQueryChanRangeReplies = 500

	// maxQueryChanRangeRepliesZlibFactor specifies the factor applied to
	// the maximum number of replies allowed for zlib encoded replies.
	maxQueryChanRangeRepliesZlibFactor = 4

	// chanRangeQueryBuffer is the number of blocks back that we'll go when
	// asking the remote peer for their any channels they know of beyond
	// our highest known channel ID.
	chanRangeQueryBuffer = 144

	// syncTransitionTimeout is the default timeout in which we'll wait up
	// to when attempting to perform a sync transition.
	syncTransitionTimeout = 5 * time.Second

	// requestBatchSize is the maximum number of channels we will query the
	// remote peer for in a QueryShortChanIDs message.
	requestBatchSize = 500

	// syncerBufferSize is the size of the syncer's buffers.
	syncerBufferSize = 50
)

var (
	// encodingTypeToChunkSize maps an encoding type, to the max number of
	// short chan ID's using the encoding type that we can fit into a
	// single message safely.
	encodingTypeToChunkSize = map[lnwire.QueryEncoding]int32{
		lnwire.EncodingSortedPlain: 8000,
	}

	// ErrGossipSyncerExiting signals that the syncer has been killed.
	ErrGossipSyncerExiting = errors.New("gossip syncer exiting")

	// ErrSyncTransitionTimeout is an error returned when we've timed out
	// attempting to perform a sync transition.
	ErrSyncTransitionTimeout = errors.New("timed out attempting to " +
		"transition sync type")

	// zeroTimestamp is the timestamp we'll use when we want to indicate to
	// peers that we do not want to receive any new graph updates.
	zeroTimestamp time.Time
)

// syncTransitionReq encapsulates a request for a gossip syncer sync transition.
type syncTransitionReq struct {
	newSyncType SyncerType
	errChan     chan error
}

// historicalSyncReq encapsulates a request for a gossip syncer to perform a
// historical sync.
type historicalSyncReq struct {
	// doneChan is a channel that serves as a signal and is closed to ensure
	// the historical sync is attempted by the time we return to the caller.
	doneChan chan struct{}
}

// gossipSyncerCfg is a struct that packages all the information a GossipSyncer
// needs to carry out its duties.
type gossipSyncerCfg struct {
	// chainHash is the chain that this syncer is responsible for.
	chainHash chainhash.Hash

	// peerPub is the public key of the peer we're syncing with, serialized
	// in compressed format.
	peerPub [33]byte

	// channelSeries is the primary interface that we'll use to generate
	// our queries and respond to the queries of the remote peer.
	channelSeries ChannelGraphTimeSeries

	// encodingType is the current encoding type we're aware of. Requests
	// with different encoding types will be rejected.
	encodingType lnwire.QueryEncoding

	// chunkSize is the max number of short chan IDs using the syncer's
	// encoding type that we can fit into a single message safely.
	chunkSize int32

	// batchSize is the max number of channels the syncer will query from
	// the remote node in a single QueryShortChanIDs request.
	batchSize int32

	// sendMsg sends a variadic number of messages to the remote peer.
	// The boolean indicates whether this method should be blocked or not
	// while waiting for sends to be written to the wire.
	sendMsg func(context.Context, bool, ...lnwire.Message) error

	// noSyncChannels will prevent the GossipSyncer from spawning a
	// channelGraphSyncer, meaning we will not try to reconcile unknown
	// channels with the remote peer.
	noSyncChannels bool

	// noReplyQueries will prevent the GossipSyncer from spawning a
	// replyHandler, meaning we will not reply to queries from our remote
	// peer.
	noReplyQueries bool

	// noTimestampQueryOption will prevent the GossipSyncer from querying
	// timestamps of announcement messages from the peer, and it will
	// prevent it from responding to timestamp queries.
	noTimestampQueryOption bool

	// ignoreHistoricalFilters will prevent syncers from replying with
	// historical data when the remote peer sets a gossip_timestamp_range.
	// This prevents ranges with old start times from causing us to dump the
	// graph on connect.
	ignoreHistoricalFilters bool

	// bestHeight returns the latest height known of the chain.
	bestHeight func() uint32

	// markGraphSynced updates the SyncManager's perception of whether we
	// have completed at least one historical sync.
	markGraphSynced func()

	// maxQueryChanRangeReplies is the maximum number of replies we'll allow
	// for a single QueryChannelRange request.
	maxQueryChanRangeReplies uint32

	// isStillZombieChannel takes the timestamps of the latest channel
	// updates for a channel and returns true if the channel should be
	// considered a zombie based on these timestamps.
	isStillZombieChannel func(time.Time, time.Time) bool

	// timestampQueueSize is the size of the timestamp range queue. If not
	// set, defaults to the global timestampQueueSize constant.
	timestampQueueSize int

	// msgBytesPerSecond is the allotted bandwidth rate, expressed in
	// bytes/second that this gossip syncer can consume. Once we exceed this
	// rate, message sending will block until we're below the rate.
	msgBytesPerSecond uint64
}

// GossipSyncer is a struct that handles synchronizing the channel graph state
// with a remote peer. The GossipSyncer implements a state machine that will
// progressively ensure we're synchronized with the channel state of the remote
// node. Once both nodes have been synchronized, we'll use an update filter to
// filter out which messages should be sent to a remote peer based on their
// update horizon. If the update horizon isn't specified, then we won't send
// them any channel updates at all.
type GossipSyncer struct {
	started sync.Once
	stopped sync.Once

	// state is the current state of the GossipSyncer.
	//
	// NOTE: This variable MUST be used atomically.
	state uint32

	// syncType denotes the SyncerType the gossip syncer is currently
	// exercising.
	//
	// NOTE: This variable MUST be used atomically.
	syncType uint32

	// remoteUpdateHorizon is the update horizon of the remote peer. We'll
	// use this to properly filter out any messages.
	remoteUpdateHorizon *lnwire.GossipTimestampRange

	// localUpdateHorizon is our local update horizon, we'll use this to
	// determine if we've already sent out our update.
	localUpdateHorizon *lnwire.GossipTimestampRange

	// syncTransitions is a channel through which new sync type transition
	// requests will be sent through. These requests should only be handled
	// when the gossip syncer is in a chansSynced state to ensure its state
	// machine behaves as expected.
	syncTransitionReqs chan *syncTransitionReq

	// historicalSyncReqs is a channel that serves as a signal for the
	// gossip syncer to perform a historical sync. These can only be done
	// once the gossip syncer is in a chansSynced state to ensure its state
	// machine behaves as expected.
	historicalSyncReqs chan *historicalSyncReq

	// genHistoricalChanRangeQuery when true signals to the gossip syncer
	// that it should request the remote peer for all of its known channel
	// IDs starting from the genesis block of the chain. This can only
	// happen if the gossip syncer receives a request to attempt a
	// historical sync. It can be unset if the syncer ever transitions from
	// PassiveSync to ActiveSync.
	genHistoricalChanRangeQuery bool

	// gossipMsgs is a channel that all responses to our queries from the
	// target peer will be sent over, these will be read by the
	// channelGraphSyncer.
	gossipMsgs chan lnwire.Message

	// queryMsgs is a channel that all queries from the remote peer will be
	// received over, these will be read by the replyHandler.
	queryMsgs chan lnwire.Message

	// curQueryRangeMsg keeps track of the latest QueryChannelRange message
	// we've sent to a peer to ensure we've consumed all expected replies.
	// This field is primarily used within the waitingQueryChanReply state.
	curQueryRangeMsg *lnwire.QueryChannelRange

	// prevReplyChannelRange keeps track of the previous ReplyChannelRange
	// message we've received from a peer to ensure they've fully replied to
	// our query by ensuring they covered our requested block range. This
	// field is primarily used within the waitingQueryChanReply state.
	prevReplyChannelRange *lnwire.ReplyChannelRange

	// bufferedChanRangeReplies is used in the waitingQueryChanReply to
	// buffer all the chunked response to our query.
	bufferedChanRangeReplies []graphdb.ChannelUpdateInfo

	// numChanRangeRepliesRcvd is used to track the number of replies
	// received as part of a QueryChannelRange. This field is primarily used
	// within the waitingQueryChanReply state.
	numChanRangeRepliesRcvd uint32

	// newChansToQuery is used to pass the set of channels we should query
	// for from the waitingQueryChanReply state to the queryNewChannels
	// state.
	newChansToQuery []lnwire.ShortChannelID

	cfg gossipSyncerCfg

	// syncedSignal is a channel that, if set, will be closed when the
	// GossipSyncer reaches its terminal chansSynced state.
	syncedSignal chan struct{}

	// syncerSema is used to more finely control the syncer's ability to
	// respond to gossip timestamp range messages.
	syncerSema chan struct{}

	// timestampRangeQueue is a buffered channel for queuing timestamp range
	// messages that need to be processed asynchronously. This prevents the
	// gossiper from blocking when ApplyGossipFilter is called.
	timestampRangeQueue chan *lnwire.GossipTimestampRange

	// isSendingBacklog is an atomic flag that indicates whether a goroutine
	// is currently sending the backlog of messages. This ensures only one
	// goroutine is active at a time.
	isSendingBacklog atomic.Bool

	sync.Mutex

	// cg is a helper that encapsulates a wait group and quit channel and
	// allows contexts that either block or cancel on those depending on
	// the use case.
	cg *fn.ContextGuard

	// rateLimiter dictates the frequency with which we will reply to gossip
	// queries to this peer.
	rateLimiter *rate.Limiter
}

// newGossipSyncer returns a new instance of the GossipSyncer populated using
// the passed config.
func newGossipSyncer(cfg gossipSyncerCfg, sema chan struct{}) *GossipSyncer {
	// Use the configured queue size if set, otherwise use the default.
	queueSize := cfg.timestampQueueSize
	if queueSize == 0 {
		queueSize = defaultTimestampQueueSize
	}

	bytesPerSecond := cfg.msgBytesPerSecond
	if bytesPerSecond == 0 {
		bytesPerSecond = DefaultPeerMsgBytesPerSecond
	}
	bytesBurst := 2 * bytesPerSecond

	// We'll use this rate limiter to limit this single peer.
	rateLimiter := rate.NewLimiter(
		rate.Limit(bytesPerSecond), int(bytesBurst),
	)

	return &GossipSyncer{
		cfg:                cfg,
		syncTransitionReqs: make(chan *syncTransitionReq),
		historicalSyncReqs: make(chan *historicalSyncReq),
		gossipMsgs:         make(chan lnwire.Message, syncerBufferSize),
		queryMsgs:          make(chan lnwire.Message, syncerBufferSize),
		timestampRangeQueue: make(
			chan *lnwire.GossipTimestampRange, queueSize,
		),
		syncerSema:  sema,
		cg:          fn.NewContextGuard(),
		rateLimiter: rateLimiter,
	}
}

// Start starts the GossipSyncer and any goroutines that it needs to carry out
// its duties.
func (g *GossipSyncer) Start() {
	g.started.Do(func() {
		log.Debugf("Starting GossipSyncer(%x)", g.cfg.peerPub[:])

		ctx, _ := g.cg.Create(context.Background())

		// TODO(conner): only spawn channelGraphSyncer if remote
		// supports gossip queries, and only spawn replyHandler if we
		// advertise support
		if !g.cfg.noSyncChannels {
			g.cg.WgAdd(1)
			go g.channelGraphSyncer(ctx)
		}
		if !g.cfg.noReplyQueries {
			g.cg.WgAdd(1)
			go g.replyHandler(ctx)
		}

		// Start the timestamp range queue processor to handle gossip
		// filter applications asynchronously.
		if !g.cfg.noTimestampQueryOption {
			g.cg.WgAdd(1)
			go g.processTimestampRangeQueue(ctx)
		}
	})
}

// Stop signals the GossipSyncer for a graceful exit, then waits until it has
// exited.
func (g *GossipSyncer) Stop() {
	g.stopped.Do(func() {
		log.Debugf("Stopping GossipSyncer(%x)", g.cfg.peerPub[:])
		defer log.Debugf("GossipSyncer(%x) stopped", g.cfg.peerPub[:])

		g.cg.Quit()
	})
}

// handleSyncingChans handles the state syncingChans for the GossipSyncer. When
// in this state, we will send a QueryChannelRange msg to our peer and advance
// the syncer's state to waitingQueryRangeReply. Returns an error if a fatal
// error occurs that should cause the goroutine to exit.
func (g *GossipSyncer) handleSyncingChans(ctx context.Context) error {
	// Prepare the query msg.
	queryRangeMsg, err := g.genChanRangeQuery(
		ctx, g.genHistoricalChanRangeQuery,
	)
	if err != nil {
		log.Errorf("Unable to gen chan range query: %v", err)

		// Any error here is likely fatal (context cancelled, db error,
		// etc.), so return it to exit the goroutine cleanly.
		return err
	}

	// Acquire a lock so the following state transition is atomic.
	//
	// NOTE: We must lock the following steps as it's possible we get an
	// immediate response (ReplyChannelRange) after sending the query msg.
	// The response is handled in ProcessQueryMsg, which requires the
	// current state to be waitingQueryRangeReply.
	g.Lock()
	defer g.Unlock()

	// Send the msg to the remote peer, which is non-blocking as
	// `sendToPeer` only queues the msg in Brontide.
	err = g.sendToPeer(ctx, queryRangeMsg)
	if err != nil {
		log.Errorf("Unable to send chan range query: %v", err)

		// Any send error (peer exiting, connection closed, rate
		// limiter signaling exit, etc.) is fatal, so return it to
		// exit the goroutine cleanly.
		return err
	}

	// With the message sent successfully, we'll transition into the next
	// state where we wait for their reply.
	g.setSyncState(waitingQueryRangeReply)

	return nil
}

// channelGraphSyncer is the main goroutine responsible for ensuring that we
// properly channel graph state with the remote peer, and also that we only
// send them messages which actually pass their defined update horizon.
func (g *GossipSyncer) channelGraphSyncer(ctx context.Context) {
	defer g.cg.WgDone()

	for {
		state := g.syncState()
		syncType := g.SyncType()

		log.Debugf("GossipSyncer(%x): state=%v, type=%v",
			g.cfg.peerPub[:], state, syncType)

		switch state {
		// When we're in this state, we're trying to synchronize our
		// view of the network with the remote peer. We'll kick off
		// this sync by asking them for the set of channels they
		// understand, as we'll as responding to any other queries by
		// them.
		case syncingChans:
			err := g.handleSyncingChans(ctx)
			if err != nil {
				log.Debugf("GossipSyncer(%x): exiting due to "+
					"error in syncingChans: %v",
					g.cfg.peerPub[:], err)

				return
			}

		// In this state, we've sent out our initial channel range
		// query and are waiting for the final response from the remote
		// peer before we perform a diff to see with channels they know
		// of that we don't.
		case waitingQueryRangeReply:
			// We'll wait to either process a new message from the
			// remote party, or exit due to the gossiper exiting,
			// or us being signalled to do so.
			select {
			case msg := <-g.gossipMsgs:
				// The remote peer is sending a response to our
				// initial query, we'll collate this response,
				// and see if it's the final one in the series.
				// If so, we can then transition to querying
				// for the new channels.
				queryReply, ok := msg.(*lnwire.ReplyChannelRange)
				if ok {
					err := g.processChanRangeReply(
						ctx, queryReply,
					)
					if err != nil {
						log.Errorf("Unable to "+
							"process chan range "+
							"query: %v", err)
						return
					}
					continue
				}

				log.Warnf("Unexpected message: %T in state=%v",
					msg, state)

			case <-g.cg.Done():
				return

			case <-ctx.Done():
				return
			}

		// We'll enter this state once we've discovered which channels
		// the remote party knows of that we don't yet know of
		// ourselves.
		case queryNewChannels:
			// First, we'll attempt to continue our channel
			// synchronization by continuing to send off another
			// query chunk.
			done, err := g.synchronizeChanIDs(ctx)
			if err != nil {
				log.Debugf("GossipSyncer(%x): exiting due to "+
					"error in queryNewChannels: %v",
					g.cfg.peerPub[:], err)

				return
			}

			// If this wasn't our last query, then we'll need to
			// transition to our waiting state.
			if !done {
				continue
			}

			// If we're fully synchronized, then we can transition
			// to our terminal state.
			g.setSyncState(chansSynced)

			// Ensure that the sync manager becomes aware that the
			// historical sync completed so synced_to_graph is
			// updated over rpc.
			g.cfg.markGraphSynced()

		// In this state, we've just sent off a new query for channels
		// that we don't yet know of. We'll remain in this state until
		// the remote party signals they've responded to our query in
		// totality.
		case waitingQueryChanReply:
			// Once we've sent off our query, we'll wait for either
			// an ending reply, or just another query from the
			// remote peer.
			select {
			case msg := <-g.gossipMsgs:
				// If this is the final reply to one of our
				// queries, then we'll loop back into our query
				// state to send of the remaining query chunks.
				_, ok := msg.(*lnwire.ReplyShortChanIDsEnd)
				if ok {
					g.setSyncState(queryNewChannels)
					continue
				}

				log.Warnf("Unexpected message: %T in state=%v",
					msg, state)

			case <-g.cg.Done():
				return

			case <-ctx.Done():
				return
			}

		// This is our final terminal state where we'll only reply to
		// any further queries by the remote peer.
		case chansSynced:
			g.Lock()
			if g.syncedSignal != nil {
				close(g.syncedSignal)
				g.syncedSignal = nil
			}
			g.Unlock()

			// If we haven't yet sent out our update horizon, and
			// we want to receive real-time channel updates, we'll
			// do so now.
			if g.localUpdateHorizon == nil &&
				syncType.IsActiveSync() {

				err := g.sendGossipTimestampRange(
					ctx, time.Now(), math.MaxUint32,
				)
				if err != nil {
					log.Errorf("Unable to send update "+
						"horizon to %x: %v",
						g.cfg.peerPub, err)
				}
			}
			// With our horizon set, we'll simply reply to any new
			// messages or process any state transitions and exit if
			// needed.
			fallthrough

		// Pinned peers will begin in this state, since they will
		// immediately receive a request to perform a historical sync.
		// Otherwise, we fall through after ending in chansSynced to
		// facilitate new requests.
		case syncerIdle:
			select {
			case req := <-g.syncTransitionReqs:
				req.errChan <- g.handleSyncTransition(ctx, req)

			case req := <-g.historicalSyncReqs:
				g.handleHistoricalSync(req)

			case <-g.cg.Done():
				return

			case <-ctx.Done():
				return
			}
		}
	}
}

// replyHandler is an event loop whose sole purpose is to reply to the remote
// peers queries. Our replyHandler will respond to messages generated by their
// channelGraphSyncer, and vice versa. Each party's channelGraphSyncer drives
// the other's replyHandler, allowing the replyHandler to operate independently
// from the state machine maintained on the same node.
//
// NOTE: This method MUST be run as a goroutine.
func (g *GossipSyncer) replyHandler(ctx context.Context) {
	defer g.cg.WgDone()

	for {
		select {
		case msg := <-g.queryMsgs:
			err := g.replyPeerQueries(ctx, msg)
			switch {
			case err == ErrGossipSyncerExiting:
				return

			case err == lnpeer.ErrPeerExiting:
				return

			case err != nil:
				log.Errorf("Unable to reply to peer "+
					"query: %v", err)
			}

		case <-g.cg.Done():
			return

		case <-ctx.Done():
			return
		}
	}
}

// processTimestampRangeQueue handles timestamp range messages from the queue
// asynchronously. This prevents blocking the gossiper when rate limiting is
// active and multiple peers are trying to apply gossip filters.
func (g *GossipSyncer) processTimestampRangeQueue(ctx context.Context) {
	defer g.cg.WgDone()

	for {
		select {
		case msg := <-g.timestampRangeQueue:
			// Process the timestamp range message. If we hit an
			// error, log it but continue processing to avoid
			// blocking the queue.
			err := g.ApplyGossipFilter(ctx, msg)
			switch {
			case errors.Is(err, ErrGossipSyncerExiting):
				return

			case errors.Is(err, lnpeer.ErrPeerExiting):
				return

			case err != nil:
				log.Errorf("Unable to apply gossip filter: %v",
					err)
			}

		case <-g.cg.Done():
			return

		case <-ctx.Done():
			return
		}
	}
}

// QueueTimestampRange attempts to queue a timestamp range message for
// asynchronous processing. If the queue is full, it returns false to indicate
// the message was dropped.
func (g *GossipSyncer) QueueTimestampRange(
	msg *lnwire.GossipTimestampRange) bool {

	// If timestamp queries are disabled, don't queue the message.
	if g.cfg.noTimestampQueryOption {
		return false
	}

	select {
	case g.timestampRangeQueue <- msg:
		return true

	// Queue is full, drop the message to prevent blocking.
	default:
		log.Warnf("Timestamp range queue full for peer %x, "+
			"dropping message", g.cfg.peerPub[:])
		return false
	}
}

// sendGossipTimestampRange constructs and sets a GossipTimestampRange for the
// syncer and sends it to the remote peer.
func (g *GossipSyncer) sendGossipTimestampRange(ctx context.Context,
	firstTimestamp time.Time, timestampRange uint32) error {

	endTimestamp := firstTimestamp.Add(
		time.Duration(timestampRange) * time.Second,
	)

	log.Infof("GossipSyncer(%x): applying gossipFilter(start=%v, end=%v)",
		g.cfg.peerPub[:], firstTimestamp, endTimestamp)

	localUpdateHorizon := &lnwire.GossipTimestampRange{
		ChainHash:      g.cfg.chainHash,
		FirstTimestamp: uint32(firstTimestamp.Unix()),
		TimestampRange: timestampRange,
	}

	if err := g.sendToPeer(ctx, localUpdateHorizon); err != nil {
		return err
	}

	if firstTimestamp.Equal(zeroTimestamp) && timestampRange == 0 {
		g.localUpdateHorizon = nil
	} else {
		g.localUpdateHorizon = localUpdateHorizon
	}

	return nil
}

// synchronizeChanIDs is called by the channelGraphSyncer when we need to query
// the remote peer for its known set of channel IDs within a particular block
// range. This method will be called continually until the entire range has
// been queried for with a response received. We'll chunk our requests as
// required to ensure they fit into a single message. We may re-renter this
// state in the case that chunking is required. Returns true if synchronization
// is complete, and an error if a fatal error occurs that should cause the
// goroutine to exit.
func (g *GossipSyncer) synchronizeChanIDs(ctx context.Context) (bool, error) {
	// If we're in this state yet there are no more new channels to query
	// for, then we'll transition to our final synced state and return true
	// to signal that we're fully synchronized.
	if len(g.newChansToQuery) == 0 {
		log.Infof("GossipSyncer(%x): no more chans to query",
			g.cfg.peerPub[:])

		return true, nil
	}

	// Otherwise, we'll issue our next chunked query to receive replies
	// for.
	var queryChunk []lnwire.ShortChannelID

	// If the number of channels to query for is less than the chunk size,
	// then we can issue a single query.
	if int32(len(g.newChansToQuery)) < g.cfg.batchSize {
		queryChunk = g.newChansToQuery
		g.newChansToQuery = nil

	} else {
		// Otherwise, we'll need to only query for the next chunk.
		// We'll slice into our query chunk, then slide down our main
		// pointer down by the chunk size.
		queryChunk = g.newChansToQuery[:g.cfg.batchSize]
		g.newChansToQuery = g.newChansToQuery[g.cfg.batchSize:]
	}

	log.Infof("GossipSyncer(%x): querying for %v new channels",
		g.cfg.peerPub[:], len(queryChunk))

	// Change the state before sending the query msg.
	g.setSyncState(waitingQueryChanReply)

	// With our chunk obtained, we'll send over our next query, then return
	// false indicating that we're net yet fully synced.
	err := g.sendToPeer(ctx, &lnwire.QueryShortChanIDs{
		ChainHash:    g.cfg.chainHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: queryChunk,
	})
	if err != nil {
		log.Errorf("Unable to sync chan IDs: %v", err)

		// Any send error (peer exiting, connection closed, rate
		// limiter signaling exit, etc.) is fatal, so return it to
		// exit the goroutine cleanly.
		return false, err
	}

	return false, nil
}

// isLegacyReplyChannelRange determines where a ReplyChannelRange message is
// considered legacy. There was a point where lnd used to include the same query
// over multiple replies, rather than including the portion of the query the
// reply is handling. We'll use this as a way of detecting whether we are
// communicating with a legacy node so we can properly sync with them.
func isLegacyReplyChannelRange(query *lnwire.QueryChannelRange,
	reply *lnwire.ReplyChannelRange) bool {

	return (reply.ChainHash == query.ChainHash &&
		reply.FirstBlockHeight == query.FirstBlockHeight &&
		reply.NumBlocks == query.NumBlocks)
}

// processChanRangeReply is called each time the GossipSyncer receives a new
// reply to the initial range query to discover new channels that it didn't
// previously know of.
func (g *GossipSyncer) processChanRangeReply(_ context.Context,
	msg *lnwire.ReplyChannelRange) error {

	// isStale returns whether the timestamp is too far into the past.
	isStale := func(timestamp time.Time) bool {
		return time.Since(timestamp) > graph.DefaultChannelPruneExpiry
	}

	// isSkewed returns whether the timestamp is too far into the future.
	isSkewed := func(timestamp time.Time) bool {
		return time.Until(timestamp) > graph.DefaultChannelPruneExpiry
	}

	// If we're not communicating with a legacy node, we'll apply some
	// further constraints on their reply to ensure it satisfies our query.
	if !isLegacyReplyChannelRange(g.curQueryRangeMsg, msg) {
		// The first block should be within our original request.
		if msg.FirstBlockHeight < g.curQueryRangeMsg.FirstBlockHeight {
			return fmt.Errorf("reply includes channels for height "+
				"%v prior to query %v", msg.FirstBlockHeight,
				g.curQueryRangeMsg.FirstBlockHeight)
		}

		// The last block should also be. We don't need to check the
		// intermediate ones because they should already be in sorted
		// order.
		replyLastHeight := msg.LastBlockHeight()
		queryLastHeight := g.curQueryRangeMsg.LastBlockHeight()
		if replyLastHeight > queryLastHeight {
			return fmt.Errorf("reply includes channels for height "+
				"%v after query %v", replyLastHeight,
				queryLastHeight)
		}

		// If we've previously received a reply for this query, look at
		// its last block to ensure the current reply properly follows
		// it.
		if g.prevReplyChannelRange != nil {
			prevReply := g.prevReplyChannelRange
			prevReplyLastHeight := prevReply.LastBlockHeight()

			// The current reply can either start from the previous
			// reply's last block, if there are still more channels
			// for the same block, or the block after.
			if msg.FirstBlockHeight != prevReplyLastHeight &&
				msg.FirstBlockHeight != prevReplyLastHeight+1 {

				return fmt.Errorf("first block of reply %v "+
					"does not continue from last block of "+
					"previous %v", msg.FirstBlockHeight,
					prevReplyLastHeight)
			}
		}
	}

	g.prevReplyChannelRange = msg

	for i, scid := range msg.ShortChanIDs {
		info := graphdb.NewChannelUpdateInfo(
			scid, time.Time{}, time.Time{},
		)

		if len(msg.Timestamps) != 0 {
			t1 := time.Unix(int64(msg.Timestamps[i].Timestamp1), 0)
			info.Node1UpdateTimestamp = t1

			t2 := time.Unix(int64(msg.Timestamps[i].Timestamp2), 0)
			info.Node2UpdateTimestamp = t2

			// Sort out all channels with outdated or skewed
			// timestamps. Both timestamps need to be out of
			// boundaries for us to skip the channel and not query
			// it later on.
			switch {
			case isStale(info.Node1UpdateTimestamp) &&
				isStale(info.Node2UpdateTimestamp):

				continue

			case isSkewed(info.Node1UpdateTimestamp) &&
				isSkewed(info.Node2UpdateTimestamp):

				continue

			case isStale(info.Node1UpdateTimestamp) &&
				isSkewed(info.Node2UpdateTimestamp):

				continue

			case isStale(info.Node2UpdateTimestamp) &&
				isSkewed(info.Node1UpdateTimestamp):

				continue
			}
		}

		g.bufferedChanRangeReplies = append(
			g.bufferedChanRangeReplies, info,
		)
	}

	switch g.cfg.encodingType {
	case lnwire.EncodingSortedPlain:
		g.numChanRangeRepliesRcvd++
	case lnwire.EncodingSortedZlib:
		g.numChanRangeRepliesRcvd += maxQueryChanRangeRepliesZlibFactor
	default:
		return fmt.Errorf("unhandled encoding type %v", g.cfg.encodingType)
	}

	log.Infof("GossipSyncer(%x): buffering chan range reply of size=%v",
		g.cfg.peerPub[:], len(msg.ShortChanIDs))

	// If this isn't the last response and we can continue to receive more,
	// then we can exit as we've already buffered the latest portion of the
	// streaming reply.
	maxReplies := g.cfg.maxQueryChanRangeReplies
	switch {
	// If we're communicating with a legacy node, we'll need to look at the
	// complete field.
	case isLegacyReplyChannelRange(g.curQueryRangeMsg, msg):
		if msg.Complete == 0 && g.numChanRangeRepliesRcvd < maxReplies {
			return nil
		}

	// Otherwise, we'll look at the reply's height range.
	default:
		replyLastHeight := msg.LastBlockHeight()
		queryLastHeight := g.curQueryRangeMsg.LastBlockHeight()

		// TODO(wilmer): This might require some padding if the remote
		// node is not aware of the last height we sent them, i.e., is
		// behind a few blocks from us.
		if replyLastHeight < queryLastHeight &&
			g.numChanRangeRepliesRcvd < maxReplies {

			return nil
		}
	}

	log.Infof("GossipSyncer(%x): filtering through %v chans",
		g.cfg.peerPub[:], len(g.bufferedChanRangeReplies))

	// Otherwise, this is the final response, so we'll now check to see
	// which channels they know of that we don't.
	newChans, err := g.cfg.channelSeries.FilterKnownChanIDs(
		g.cfg.chainHash, g.bufferedChanRangeReplies,
		g.cfg.isStillZombieChannel,
	)
	if err != nil {
		return fmt.Errorf("unable to filter chan ids: %w", err)
	}

	// As we've received the entirety of the reply, we no longer need to
	// hold on to the set of buffered replies or the original query that
	// prompted the replies, so we'll let that be garbage collected now.
	g.curQueryRangeMsg = nil
	g.prevReplyChannelRange = nil
	g.bufferedChanRangeReplies = nil
	g.numChanRangeRepliesRcvd = 0

	// If there aren't any channels that we don't know of, then we can
	// switch straight to our terminal state.
	if len(newChans) == 0 {
		log.Infof("GossipSyncer(%x): remote peer has no new chans",
			g.cfg.peerPub[:])

		g.setSyncState(chansSynced)

		// Ensure that the sync manager becomes aware that the
		// historical sync completed so synced_to_graph is updated over
		// rpc.
		g.cfg.markGraphSynced()
		return nil
	}

	// Otherwise, we'll set the set of channels that we need to query for
	// the next state, and also transition our state.
	g.newChansToQuery = newChans
	g.setSyncState(queryNewChannels)

	log.Infof("GossipSyncer(%x): starting query for %v new chans",
		g.cfg.peerPub[:], len(newChans))

	return nil
}

// genChanRangeQuery generates the initial message we'll send to the remote
// party when we're kicking off the channel graph synchronization upon
// connection. The historicalQuery boolean can be used to generate a query from
// the genesis block of the chain.
func (g *GossipSyncer) genChanRangeQuery(ctx context.Context,
	historicalQuery bool) (*lnwire.QueryChannelRange, error) {

	// First, we'll query our channel graph time series for its highest
	// known channel ID.
	newestChan, err := g.cfg.channelSeries.HighestChanID(
		ctx, g.cfg.chainHash,
	)
	if err != nil {
		return nil, err
	}

	// Once we have the chan ID of the newest, we'll obtain the block height
	// of the channel, then subtract our default horizon to ensure we don't
	// miss any channels. By default, we go back 1 day from the newest
	// channel, unless we're attempting a historical sync, where we'll
	// actually start from the genesis block instead.
	var startHeight uint32
	switch {
	case historicalQuery:
		fallthrough
	case newestChan.BlockHeight <= chanRangeQueryBuffer:
		startHeight = 0
	default:
		startHeight = newestChan.BlockHeight - chanRangeQueryBuffer
	}

	// Determine the number of blocks to request based on our best height.
	// We'll take into account any potential underflows and explicitly set
	// numBlocks to its minimum value of 1 if so.
	bestHeight := g.cfg.bestHeight()
	numBlocks := bestHeight - startHeight
	if int64(numBlocks) < 1 {
		numBlocks = 1
	}

	log.Infof("GossipSyncer(%x): requesting new chans from height=%v "+
		"and %v blocks after", g.cfg.peerPub[:], startHeight, numBlocks)

	// Finally, we'll craft the channel range query, using our starting
	// height, then asking for all known channels to the foreseeable end of
	// the main chain.
	query := &lnwire.QueryChannelRange{
		ChainHash:        g.cfg.chainHash,
		FirstBlockHeight: startHeight,
		NumBlocks:        numBlocks,
	}

	if !g.cfg.noTimestampQueryOption {
		query.QueryOptions = lnwire.NewTimestampQueryOption()
	}

	g.curQueryRangeMsg = query

	return query, nil
}

// replyPeerQueries is called in response to any query by the remote peer.
// We'll examine our state and send back our best response.
func (g *GossipSyncer) replyPeerQueries(ctx context.Context,
	msg lnwire.Message) error {

	switch msg := msg.(type) {

	// In this state, we'll also handle any incoming channel range queries
	// from the remote peer as they're trying to sync their state as well.
	case *lnwire.QueryChannelRange:
		return g.replyChanRangeQuery(ctx, msg)

	// If the remote peer skips straight to requesting new channels that
	// they don't know of, then we'll ensure that we also handle this case.
	case *lnwire.QueryShortChanIDs:
		return g.replyShortChanIDs(ctx, msg)

	default:
		return fmt.Errorf("unknown message: %T", msg)
	}
}

// replyChanRangeQuery will be dispatched in response to a channel range query
// by the remote node. We'll query the channel time series for channels that
// meet the channel range, then chunk our responses to the remote node. We also
// ensure that our final fragment carries the "complete" bit to indicate the
// end of our streaming response.
func (g *GossipSyncer) replyChanRangeQuery(ctx context.Context,
	query *lnwire.QueryChannelRange) error {

	// Before responding, we'll check to ensure that the remote peer is
	// querying for the same chain that we're on. If not, we'll send back a
	// response with a complete value of zero to indicate we're on a
	// different chain.
	if g.cfg.chainHash != query.ChainHash {
		log.Warnf("Remote peer requested QueryChannelRange for "+
			"chain=%v, we're on chain=%v", query.ChainHash,
			g.cfg.chainHash)

		return g.sendToPeerSync(ctx, &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			FirstBlockHeight: query.FirstBlockHeight,
			NumBlocks:        query.NumBlocks,
			Complete:         0,
			EncodingType:     g.cfg.encodingType,
			ShortChanIDs:     nil,
		})
	}

	log.Infof("GossipSyncer(%x): filtering chan range: start_height=%v, "+
		"num_blocks=%v", g.cfg.peerPub[:], query.FirstBlockHeight,
		query.NumBlocks)

	// Check if the query asked for timestamps. We will only serve
	// timestamps if this has not been disabled with
	// noTimestampQueryOption.
	withTimestamps := query.WithTimestamps() &&
		!g.cfg.noTimestampQueryOption

	// Next, we'll consult the time series to obtain the set of known
	// channel ID's that match their query.
	startBlock := query.FirstBlockHeight
	endBlock := query.LastBlockHeight()
	channelRanges, err := g.cfg.channelSeries.FilterChannelRange(
		query.ChainHash, startBlock, endBlock, withTimestamps,
	)
	if err != nil {
		return err
	}

	// TODO(roasbeef): means can't send max uint above?
	//  * or make internal 64

	// We'll send our response in a streaming manner, chunk-by-chunk. We do
	// this as there's a transport message size limit which we'll need to
	// adhere to. We also need to make sure all of our replies cover the
	// expected range of the query.
	sendReplyForChunk := func(channelChunk []graphdb.ChannelUpdateInfo,
		firstHeight, lastHeight uint32, finalChunk bool) error {

		// The number of blocks contained in the current chunk (the
		// total span) is the difference between the last channel ID and
		// the first in the range. We add one as even if all channels
		// returned are in the same block, we need to count that.
		numBlocks := lastHeight - firstHeight + 1
		complete := uint8(0)
		if finalChunk {
			complete = 1
		}

		var timestamps lnwire.Timestamps
		if withTimestamps {
			timestamps = make(lnwire.Timestamps, len(channelChunk))
		}

		scids := make([]lnwire.ShortChannelID, len(channelChunk))
		for i, info := range channelChunk {
			scids[i] = info.ShortChannelID

			if !withTimestamps {
				continue
			}

			timestamps[i].Timestamp1 = uint32(
				info.Node1UpdateTimestamp.Unix(),
			)

			timestamps[i].Timestamp2 = uint32(
				info.Node2UpdateTimestamp.Unix(),
			)
		}

		return g.sendToPeerSync(ctx, &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			NumBlocks:        numBlocks,
			FirstBlockHeight: firstHeight,
			Complete:         complete,
			EncodingType:     g.cfg.encodingType,
			ShortChanIDs:     scids,
			Timestamps:       timestamps,
		})
	}

	var (
		firstHeight  = query.FirstBlockHeight
		lastHeight   uint32
		channelChunk []graphdb.ChannelUpdateInfo
	)

	// chunkSize is the maximum number of SCIDs that we can safely put in a
	// single message. If we also need to include timestamps though, then
	// this number is halved since encoding two timestamps takes the same
	// number of bytes as encoding an SCID.
	chunkSize := g.cfg.chunkSize
	if withTimestamps {
		chunkSize /= 2
	}

	for _, channelRange := range channelRanges {
		channels := channelRange.Channels
		numChannels := int32(len(channels))
		numLeftToAdd := chunkSize - int32(len(channelChunk))

		// Include the current block in the ongoing chunk if it can fit
		// and move on to the next block.
		if numChannels <= numLeftToAdd {
			channelChunk = append(channelChunk, channels...)
			continue
		}

		// Otherwise, we need to send our existing channel chunk as is
		// as its own reply and start a new one for the current block.
		// We'll mark the end of our current chunk as the height before
		// the current block to ensure the whole query range is replied
		// to.
		log.Infof("GossipSyncer(%x): sending range chunk of size=%v",
			g.cfg.peerPub[:], len(channelChunk))

		lastHeight = channelRange.Height - 1
		err := sendReplyForChunk(
			channelChunk, firstHeight, lastHeight, false,
		)
		if err != nil {
			return err
		}

		// With the reply constructed, we'll start tallying channels for
		// our next one keeping in mind our chunk size. This may result
		// in channels for this block being left out from the reply, but
		// this isn't an issue since we'll randomly shuffle them and we
		// assume a historical gossip sync is performed at a later time.
		firstHeight = channelRange.Height
		finalChunkSize := numChannels
		exceedsChunkSize := numChannels > chunkSize
		if exceedsChunkSize {
			rand.Shuffle(len(channels), func(i, j int) {
				channels[i], channels[j] = channels[j], channels[i]
			})
			finalChunkSize = chunkSize
		}
		channelChunk = channels[:finalChunkSize]

		// Sort the chunk once again if we had to shuffle it.
		if exceedsChunkSize {
			sort.Slice(channelChunk, func(i, j int) bool {
				id1 := channelChunk[i].ShortChannelID.ToUint64()
				id2 := channelChunk[j].ShortChannelID.ToUint64()

				return id1 < id2
			})
		}
	}

	// Send the remaining chunk as the final reply.
	log.Infof("GossipSyncer(%x): sending final chan range chunk, size=%v",
		g.cfg.peerPub[:], len(channelChunk))

	return sendReplyForChunk(
		channelChunk, firstHeight, query.LastBlockHeight(), true,
	)
}

// replyShortChanIDs will be dispatched in response to a query by the remote
// node for information concerning a set of short channel ID's. Our response
// will be sent in a streaming chunked manner to ensure that we remain below
// the current transport level message size.
func (g *GossipSyncer) replyShortChanIDs(ctx context.Context,
	query *lnwire.QueryShortChanIDs) error {

	// Before responding, we'll check to ensure that the remote peer is
	// querying for the same chain that we're on. If not, we'll send back a
	// response with a complete value of zero to indicate we're on a
	// different chain.
	if g.cfg.chainHash != query.ChainHash {
		log.Warnf("Remote peer requested QueryShortChanIDs for "+
			"chain=%v, we're on chain=%v", query.ChainHash,
			g.cfg.chainHash)

		return g.sendToPeerSync(ctx, &lnwire.ReplyShortChanIDsEnd{
			ChainHash: query.ChainHash,
			Complete:  0,
		})
	}

	if len(query.ShortChanIDs) == 0 {
		log.Infof("GossipSyncer(%x): ignoring query for blank short chan ID's",
			g.cfg.peerPub[:])
		return nil
	}

	log.Infof("GossipSyncer(%x): fetching chan anns for %v chans",
		g.cfg.peerPub[:], len(query.ShortChanIDs))

	// Now that we know we're on the same chain, we'll query the channel
	// time series for the set of messages that we know of which satisfies
	// the requirement of being a chan ann, chan update, or a node ann
	// related to the set of queried channels.
	replyMsgs, err := g.cfg.channelSeries.FetchChanAnns(
		query.ChainHash, query.ShortChanIDs,
	)
	if err != nil {
		return fmt.Errorf("unable to fetch chan anns for %v..., %w",
			query.ShortChanIDs[0].ToUint64(), err)
	}

	// Reply with any messages related to those channel ID's, we'll write
	// each one individually and synchronously to throttle the sends and
	// perform buffering of responses in the syncer as opposed to the peer.
	for _, msg := range replyMsgs {
		err := g.sendToPeerSync(ctx, msg)
		if err != nil {
			return err
		}
	}

	// Regardless of whether we had any messages to reply with, send over
	// the sentinel message to signal that the stream has terminated.
	return g.sendToPeerSync(ctx, &lnwire.ReplyShortChanIDsEnd{
		ChainHash: query.ChainHash,
		Complete:  1,
	})
}

// ApplyGossipFilter applies a gossiper filter sent by the remote node to the
// state machine. Once applied, we'll ensure that we don't forward any messages
// to the peer that aren't within the time range of the filter.
func (g *GossipSyncer) ApplyGossipFilter(ctx context.Context,
	filter *lnwire.GossipTimestampRange) error {

	g.Lock()

	g.remoteUpdateHorizon = filter

	startTime := time.Unix(int64(g.remoteUpdateHorizon.FirstTimestamp), 0)
	endTime := startTime.Add(
		time.Duration(g.remoteUpdateHorizon.TimestampRange) * time.Second,
	)

	g.Unlock()

	// If requested, don't reply with historical gossip data when the remote
	// peer sets their gossip timestamp range.
	if g.cfg.ignoreHistoricalFilters {
		return nil
	}

	// Check if a goroutine is already sending the backlog. If so, return
	// early without attempting to acquire the semaphore.
	if g.isSendingBacklog.Load() {
		log.Debugf("GossipSyncer(%x): skipping ApplyGossipFilter, "+
			"backlog send already in progress", g.cfg.peerPub[:])
		return nil
	}

	select {
	case <-g.syncerSema:
	case <-g.cg.Done():
		return ErrGossipSyncerExiting
	case <-ctx.Done():
		return ctx.Err()
	}

	// We don't put this in a defer because if the goroutine is launched,
	// it needs to be called when the goroutine is stopped.
	returnSema := func() {
		g.syncerSema <- struct{}{}
	}

	// Now that the remote peer has applied their filter, we'll query the
	// database for all the messages that are beyond this filter.
	newUpdatestoSend := g.cfg.channelSeries.UpdatesInHorizon(
		g.cfg.chainHash, startTime, endTime,
	)

	// Create a pull-based iterator so we can check if there are any
	// updates before launching the goroutine.
	next, stop := iter.Pull2(newUpdatestoSend)

	// Check if we have any updates to send by attempting to get the first
	// message.
	firstMsg, firstErr, ok := next()
	if firstErr != nil {
		stop()
		returnSema()
		return firstErr
	}

	log.Infof("GossipSyncer(%x): applying new remote update horizon: "+
		"start=%v, end=%v, has_updates=%v", g.cfg.peerPub[:],
		startTime, endTime, ok)

	// If we don't have any to send, then we can return early.
	if !ok {
		stop()
		returnSema()
		return nil
	}

	// Set the atomic flag to indicate we're starting to send the backlog.
	// If the swap fails, it means another goroutine is already active, so
	// we return early.
	if !g.isSendingBacklog.CompareAndSwap(false, true) {
		returnSema()
		log.Debugf("GossipSyncer(%x): another goroutine already "+
			"sending backlog, skipping", g.cfg.peerPub[:])

		return nil
	}

	// We'll conclude by launching a goroutine to send out any updates.
	// The goroutine takes ownership of the iterator.
	g.cg.WgAdd(1)
	go func() {
		defer g.cg.WgDone()
		defer returnSema()
		defer g.isSendingBacklog.Store(false)
		defer stop()

		// Send the first message we already pulled.
		err := g.sendToPeerSync(ctx, firstMsg)
		switch {
		case errors.Is(err, ErrGossipSyncerExiting):
			return

		case errors.Is(err, lnpeer.ErrPeerExiting):
			return

		case err != nil:
			log.Errorf("Unable to send message for "+
				"peer catch up: %v", err)
		}

		// Continue with the rest of the messages using the same pull
		// iterator.
		for {
			msg, err, ok := next()
			if !ok {
				return
			}

			// If the iterator yielded an error, log it and
			// continue.
			if err != nil {
				log.Errorf("Error fetching update for peer "+
					"catch up: %v", err)
				continue
			}

			err = g.sendToPeerSync(ctx, msg)
			switch {
			case err == ErrGossipSyncerExiting:
				return

			case err == lnpeer.ErrPeerExiting:
				return

			case err != nil:
				log.Errorf("Unable to send message for "+
					"peer catch up: %v", err)
			}
		}
	}()

	return nil
}

// FilterGossipMsgs takes a set of gossip messages, and only send it to a peer
// iff the message is within the bounds of their set gossip filter. If the peer
// doesn't have a gossip filter set, then no messages will be forwarded.
func (g *GossipSyncer) FilterGossipMsgs(ctx context.Context,
	msgs ...msgWithSenders) {

	// If the peer doesn't have an update horizon set, then we won't send
	// it any new update messages.
	if g.remoteUpdateHorizon == nil {
		log.Tracef("GossipSyncer(%x): skipped due to nil "+
			"remoteUpdateHorizon", g.cfg.peerPub[:])
		return
	}

	// If we've been signaled to exit, or are exiting, then we'll stop
	// short.
	select {
	case <-g.cg.Done():
		return
	case <-ctx.Done():
		return
	default:
	}

	// TODO(roasbeef): need to ensure that peer still online...send msg to
	// gossiper on peer termination to signal peer disconnect?

	var err error

	// Before we filter out the messages, we'll construct an index over the
	// set of channel announcements and channel updates. This will allow us
	// to quickly check if we should forward a chan ann, based on the known
	// channel updates for a channel.
	chanUpdateIndex := make(
		map[lnwire.ShortChannelID][]*lnwire.ChannelUpdate1,
	)
	for _, msg := range msgs {
		chanUpdate, ok := msg.msg.(*lnwire.ChannelUpdate1)
		if !ok {
			continue
		}

		chanUpdateIndex[chanUpdate.ShortChannelID] = append(
			chanUpdateIndex[chanUpdate.ShortChannelID], chanUpdate,
		)
	}

	// We'll construct a helper function that we'll us below to determine
	// if a given messages passes the gossip msg filter.
	g.Lock()
	startTime := time.Unix(int64(g.remoteUpdateHorizon.FirstTimestamp), 0)
	endTime := startTime.Add(
		time.Duration(g.remoteUpdateHorizon.TimestampRange) * time.Second,
	)
	g.Unlock()

	passesFilter := func(timeStamp uint32) bool {
		t := time.Unix(int64(timeStamp), 0)
		return t.Equal(startTime) ||
			(t.After(startTime) && t.Before(endTime))
	}

	msgsToSend := make([]lnwire.Message, 0, len(msgs))
	for _, msg := range msgs {
		// If the target peer is the peer that sent us this message,
		// then we'll exit early as we don't need to filter this
		// message.
		if _, ok := msg.senders[g.cfg.peerPub]; ok {
			continue
		}

		switch msg := msg.msg.(type) {

		// For each channel announcement message, we'll only send this
		// message if the channel updates for the channel are between
		// our time range.
		case *lnwire.ChannelAnnouncement1:
			// First, we'll check if the channel updates are in
			// this message batch.
			chanUpdates, ok := chanUpdateIndex[msg.ShortChannelID]
			if !ok {
				// If not, we'll attempt to query the database
				// to see if we know of the updates.
				chanUpdates, err = g.cfg.channelSeries.FetchChanUpdates(
					g.cfg.chainHash, msg.ShortChannelID,
				)
				if err != nil {
					log.Warnf("no channel updates found for "+
						"short_chan_id=%v",
						msg.ShortChannelID)
					continue
				}
			}

			for _, chanUpdate := range chanUpdates {
				if passesFilter(chanUpdate.Timestamp) {
					msgsToSend = append(msgsToSend, msg)
					break
				}
			}

			if len(chanUpdates) == 0 {
				msgsToSend = append(msgsToSend, msg)
			}

		// For each channel update, we'll only send if it the timestamp
		// is between our time range.
		case *lnwire.ChannelUpdate1:
			if passesFilter(msg.Timestamp) {
				msgsToSend = append(msgsToSend, msg)
			}

		// Similarly, we only send node announcements if the update
		// timestamp ifs between our set gossip filter time range.
		case *lnwire.NodeAnnouncement1:
			if passesFilter(msg.Timestamp) {
				msgsToSend = append(msgsToSend, msg)
			}
		}
	}

	log.Tracef("GossipSyncer(%x): filtered gossip msgs: set=%v, sent=%v",
		g.cfg.peerPub[:], len(msgs), len(msgsToSend))

	if len(msgsToSend) == 0 {
		return
	}

	if err = g.sendToPeer(ctx, msgsToSend...); err != nil {
		log.Errorf("unable to send gossip msgs: %v", err)
	}

}

// ProcessQueryMsg is used by outside callers to pass new channel time series
// queries to the internal processing goroutine.
func (g *GossipSyncer) ProcessQueryMsg(msg lnwire.Message, peerQuit <-chan struct{}) error {
	var msgChan chan lnwire.Message
	switch msg.(type) {
	case *lnwire.QueryChannelRange, *lnwire.QueryShortChanIDs:
		msgChan = g.queryMsgs

	// Reply messages should only be expected in states where we're waiting
	// for a reply.
	case *lnwire.ReplyChannelRange, *lnwire.ReplyShortChanIDsEnd:
		g.Lock()
		syncState := g.syncState()
		g.Unlock()

		if syncState != waitingQueryRangeReply &&
			syncState != waitingQueryChanReply {

			return fmt.Errorf("unexpected msg %T received in "+
				"state %v", msg, syncState)
		}
		msgChan = g.gossipMsgs

	default:
		msgChan = g.gossipMsgs
	}

	select {
	case msgChan <- msg:
	case <-peerQuit:
	case <-g.cg.Done():
	}

	return nil
}

// setSyncState sets the gossip syncer's state to the given state.
func (g *GossipSyncer) setSyncState(state syncerState) {
	atomic.StoreUint32(&g.state, uint32(state))
}

// syncState returns the current syncerState of the target GossipSyncer.
func (g *GossipSyncer) syncState() syncerState {
	return syncerState(atomic.LoadUint32(&g.state))
}

// ResetSyncedSignal returns a channel that will be closed in order to serve as
// a signal for when the GossipSyncer has reached its chansSynced state.
func (g *GossipSyncer) ResetSyncedSignal() chan struct{} {
	g.Lock()
	defer g.Unlock()

	syncedSignal := make(chan struct{})

	syncState := syncerState(atomic.LoadUint32(&g.state))
	if syncState == chansSynced {
		close(syncedSignal)
		return syncedSignal
	}

	g.syncedSignal = syncedSignal
	return g.syncedSignal
}

// ProcessSyncTransition sends a request to the gossip syncer to transition its
// sync type to a new one.
//
// NOTE: This can only be done once the gossip syncer has reached its final
// chansSynced state.
func (g *GossipSyncer) ProcessSyncTransition(newSyncType SyncerType) error {
	errChan := make(chan error, 1)
	select {
	case g.syncTransitionReqs <- &syncTransitionReq{
		newSyncType: newSyncType,
		errChan:     errChan,
	}:
	case <-time.After(syncTransitionTimeout):
		return ErrSyncTransitionTimeout
	case <-g.cg.Done():
		return ErrGossipSyncerExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-g.cg.Done():
		return ErrGossipSyncerExiting
	}
}

// handleSyncTransition handles a new sync type transition request.
//
// NOTE: The gossip syncer might have another sync state as a result of this
// transition.
func (g *GossipSyncer) handleSyncTransition(ctx context.Context,
	req *syncTransitionReq) error {

	// Return early from any NOP sync transitions.
	syncType := g.SyncType()
	if syncType == req.newSyncType {
		return nil
	}

	log.Debugf("GossipSyncer(%x): transitioning from %v to %v",
		g.cfg.peerPub, syncType, req.newSyncType)

	var (
		firstTimestamp time.Time
		timestampRange uint32
	)

	switch req.newSyncType {
	// If an active sync has been requested, then we should resume receiving
	// new graph updates from the remote peer.
	case ActiveSync, PinnedSync:
		firstTimestamp = time.Now()
		timestampRange = math.MaxUint32

	// If a PassiveSync transition has been requested, then we should no
	// longer receive any new updates from the remote peer. We can do this
	// by setting our update horizon to a range in the past ensuring no
	// graph updates match the timestamp range.
	case PassiveSync:
		firstTimestamp = zeroTimestamp
		timestampRange = 0

	default:
		return fmt.Errorf("unhandled sync transition %v",
			req.newSyncType)
	}

	err := g.sendGossipTimestampRange(ctx, firstTimestamp, timestampRange)
	if err != nil {
		return fmt.Errorf("unable to send local update horizon: %w",
			err)
	}

	g.setSyncType(req.newSyncType)

	return nil
}

// setSyncType sets the gossip syncer's sync type to the given type.
func (g *GossipSyncer) setSyncType(syncType SyncerType) {
	atomic.StoreUint32(&g.syncType, uint32(syncType))
}

// SyncType returns the current SyncerType of the target GossipSyncer.
func (g *GossipSyncer) SyncType() SyncerType {
	return SyncerType(atomic.LoadUint32(&g.syncType))
}

// historicalSync sends a request to the gossip syncer to perofmr a historical
// sync.
//
// NOTE: This can only be done once the gossip syncer has reached its final
// chansSynced state.
func (g *GossipSyncer) historicalSync() error {
	done := make(chan struct{})

	select {
	case g.historicalSyncReqs <- &historicalSyncReq{
		doneChan: done,
	}:
	case <-time.After(syncTransitionTimeout):
		return ErrSyncTransitionTimeout
	case <-g.cg.Done():
		return ErrGossiperShuttingDown
	}

	select {
	case <-done:
		return nil
	case <-g.cg.Done():
		return ErrGossiperShuttingDown
	}
}

// handleHistoricalSync handles a request to the gossip syncer to perform a
// historical sync.
func (g *GossipSyncer) handleHistoricalSync(req *historicalSyncReq) {
	// We'll go back to our initial syncingChans state in order to request
	// the remote peer to give us all of the channel IDs they know of
	// starting from the genesis block.
	g.genHistoricalChanRangeQuery = true
	g.setSyncState(syncingChans)
	close(req.doneChan)
}

// sendToPeer sends a variadic number of messages to the remote peer. This
// method should not block while waiting for sends to be written to the wire.
func (g *GossipSyncer) sendToPeer(ctx context.Context,
	msgs ...lnwire.Message) error {

	return g.sendMsgRateLimited(ctx, false, msgs...)
}

// sendToPeerSync sends a variadic number of messages to the remote peer,
// blocking until all messages have been sent successfully or a write error is
// encountered.
func (g *GossipSyncer) sendToPeerSync(ctx context.Context,
	msgs ...lnwire.Message) error {

	return g.sendMsgRateLimited(ctx, true, msgs...)
}

// sendMsgRateLimited sends a variadic number of messages to the remote peer,
// applying our per-peer rate limit before each send. The sync boolean
// determines if the send is blocking or not.
func (g *GossipSyncer) sendMsgRateLimited(ctx context.Context, sync bool,
	msgs ...lnwire.Message) error {

	for _, msg := range msgs {
		err := maybeRateLimitMsg(
			ctx, g.rateLimiter, g.cfg.peerPub, msg, g.cg.Done(),
		)
		if err != nil {
			return err
		}

		err = g.cfg.sendMsg(ctx, sync, msg)
		if err != nil {
			return err
		}
	}

	return nil
}
