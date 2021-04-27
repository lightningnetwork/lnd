package discovery

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	// DefaultMaxUndelayedQueryReplies specifies how many gossip queries we
	// will respond to immediately before starting to delay responses.
	DefaultMaxUndelayedQueryReplies = 10

	// DefaultDelayedQueryReplyInterval is the length of time we will wait
	// before responding to gossip queries after replying to
	// maxUndelayedQueryReplies queries.
	DefaultDelayedQueryReplyInterval = 5 * time.Second

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
)

var (
	// encodingTypeToChunkSize maps an encoding type, to the max number of
	// short chan ID's using the encoding type that we can fit into a
	// single message safely.
	encodingTypeToChunkSize = map[lnwire.ShortChanIDEncoding]int32{
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
	encodingType lnwire.ShortChanIDEncoding

	// chunkSize is the max number of short chan IDs using the syncer's
	// encoding type that we can fit into a single message safely.
	chunkSize int32

	// batchSize is the max number of channels the syncer will query from
	// the remote node in a single QueryShortChanIDs request.
	batchSize int32

	// sendToPeer sends a variadic number of messages to the remote peer.
	// This method should not block while waiting for sends to be written
	// to the wire.
	sendToPeer func(...lnwire.Message) error

	// sendToPeerSync sends a variadic number of messages to the remote
	// peer, blocking until all messages have been sent successfully or a
	// write error is encountered.
	sendToPeerSync func(...lnwire.Message) error

	// maxUndelayedQueryReplies specifies how many gossip queries we will
	// respond to immediately before starting to delay responses.
	maxUndelayedQueryReplies int

	// delayedQueryReplyInterval is the length of time we will wait before
	// responding to gossip queries after replying to
	// maxUndelayedQueryReplies queries.
	delayedQueryReplyInterval time.Duration

	// noSyncChannels will prevent the GossipSyncer from spawning a
	// channelGraphSyncer, meaning we will not try to reconcile unknown
	// channels with the remote peer.
	noSyncChannels bool

	// noReplyQueries will prevent the GossipSyncer from spawning a
	// replyHandler, meaning we will not reply to queries from our remote
	// peer.
	noReplyQueries bool

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
	bufferedChanRangeReplies []lnwire.ShortChannelID

	// numChanRangeRepliesRcvd is used to track the number of replies
	// received as part of a QueryChannelRange. This field is primarily used
	// within the waitingQueryChanReply state.
	numChanRangeRepliesRcvd uint32

	// newChansToQuery is used to pass the set of channels we should query
	// for from the waitingQueryChanReply state to the queryNewChannels
	// state.
	newChansToQuery []lnwire.ShortChannelID

	cfg gossipSyncerCfg

	// rateLimiter dictates the frequency with which we will reply to gossip
	// queries from a peer. This is used to delay responses to peers to
	// prevent DOS vulnerabilities if they are spamming with an unreasonable
	// number of queries.
	rateLimiter *rate.Limiter

	// syncedSignal is a channel that, if set, will be closed when the
	// GossipSyncer reaches its terminal chansSynced state.
	syncedSignal chan struct{}

	sync.Mutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// newGossipSyncer returns a new instance of the GossipSyncer populated using
// the passed config.
func newGossipSyncer(cfg gossipSyncerCfg) *GossipSyncer {
	// If no parameter was specified for max undelayed query replies, set it
	// to the default of 5 queries.
	if cfg.maxUndelayedQueryReplies <= 0 {
		cfg.maxUndelayedQueryReplies = DefaultMaxUndelayedQueryReplies
	}

	// If no parameter was specified for delayed query reply interval, set
	// to the default of 5 seconds.
	if cfg.delayedQueryReplyInterval <= 0 {
		cfg.delayedQueryReplyInterval = DefaultDelayedQueryReplyInterval
	}

	// Construct a rate limiter that will govern how frequently we reply to
	// gossip queries from this peer. The limiter will automatically adjust
	// during periods of quiescence, and increase the reply interval under
	// load.
	interval := rate.Every(cfg.delayedQueryReplyInterval)
	rateLimiter := rate.NewLimiter(
		interval, cfg.maxUndelayedQueryReplies,
	)

	return &GossipSyncer{
		cfg:                cfg,
		rateLimiter:        rateLimiter,
		syncTransitionReqs: make(chan *syncTransitionReq),
		historicalSyncReqs: make(chan *historicalSyncReq),
		gossipMsgs:         make(chan lnwire.Message, 100),
		queryMsgs:          make(chan lnwire.Message, 100),
		quit:               make(chan struct{}),
	}
}

// Start starts the GossipSyncer and any goroutines that it needs to carry out
// its duties.
func (g *GossipSyncer) Start() {
	g.started.Do(func() {
		log.Debugf("Starting GossipSyncer(%x)", g.cfg.peerPub[:])

		// TODO(conner): only spawn channelGraphSyncer if remote
		// supports gossip queries, and only spawn replyHandler if we
		// advertise support
		if !g.cfg.noSyncChannels {
			g.wg.Add(1)
			go g.channelGraphSyncer()
		}
		if !g.cfg.noReplyQueries {
			g.wg.Add(1)
			go g.replyHandler()
		}
	})
}

// Stop signals the GossipSyncer for a graceful exit, then waits until it has
// exited.
func (g *GossipSyncer) Stop() {
	g.stopped.Do(func() {
		close(g.quit)
		g.wg.Wait()
	})
}

// channelGraphSyncer is the main goroutine responsible for ensuring that we
// properly channel graph state with the remote peer, and also that we only
// send them messages which actually pass their defined update horizon.
func (g *GossipSyncer) channelGraphSyncer() {
	defer g.wg.Done()

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
			// If we're in this state, then we'll send the remote
			// peer our opening QueryChannelRange message.
			queryRangeMsg, err := g.genChanRangeQuery(
				g.genHistoricalChanRangeQuery,
			)
			if err != nil {
				log.Errorf("Unable to gen chan range "+
					"query: %v", err)
				return
			}

			err = g.cfg.sendToPeer(queryRangeMsg)
			if err != nil {
				log.Errorf("Unable to send chan range "+
					"query: %v", err)
				return
			}

			// With the message sent successfully, we'll transition
			// into the next state where we wait for their reply.
			g.setSyncState(waitingQueryRangeReply)

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
					err := g.processChanRangeReply(queryReply)
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

			case <-g.quit:
				return
			}

		// We'll enter this state once we've discovered which channels
		// the remote party knows of that we don't yet know of
		// ourselves.
		case queryNewChannels:
			// First, we'll attempt to continue our channel
			// synchronization by continuing to send off another
			// query chunk.
			done, err := g.synchronizeChanIDs()
			if err != nil {
				log.Errorf("Unable to sync chan IDs: %v", err)
			}

			// If this wasn't our last query, then we'll need to
			// transition to our waiting state.
			if !done {
				g.setSyncState(waitingQueryChanReply)
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

			case <-g.quit:
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
					time.Now(), math.MaxUint32,
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
				req.errChan <- g.handleSyncTransition(req)

			case req := <-g.historicalSyncReqs:
				g.handleHistoricalSync(req)

			case <-g.quit:
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
func (g *GossipSyncer) replyHandler() {
	defer g.wg.Done()

	for {
		select {
		case msg := <-g.queryMsgs:
			err := g.replyPeerQueries(msg)
			switch {
			case err == ErrGossipSyncerExiting:
				return

			case err == lnpeer.ErrPeerExiting:
				return

			case err != nil:
				log.Errorf("Unable to reply to peer "+
					"query: %v", err)
			}

		case <-g.quit:
			return
		}
	}
}

// sendGossipTimestampRange constructs and sets a GossipTimestampRange for the
// syncer and sends it to the remote peer.
func (g *GossipSyncer) sendGossipTimestampRange(firstTimestamp time.Time,
	timestampRange uint32) error {

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

	if err := g.cfg.sendToPeer(localUpdateHorizon); err != nil {
		return err
	}

	if firstTimestamp == zeroTimestamp && timestampRange == 0 {
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
// state in the case that chunking is required.
func (g *GossipSyncer) synchronizeChanIDs() (bool, error) {
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

	// With our chunk obtained, we'll send over our next query, then return
	// false indicating that we're net yet fully synced.
	err := g.cfg.sendToPeer(&lnwire.QueryShortChanIDs{
		ChainHash:    g.cfg.chainHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: queryChunk,
	})

	return false, err
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
func (g *GossipSyncer) processChanRangeReply(msg *lnwire.ReplyChannelRange) error {
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
	g.bufferedChanRangeReplies = append(
		g.bufferedChanRangeReplies, msg.ShortChanIDs...,
	)
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
	)
	if err != nil {
		return fmt.Errorf("unable to filter chan ids: %v", err)
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
func (g *GossipSyncer) genChanRangeQuery(
	historicalQuery bool) (*lnwire.QueryChannelRange, error) {

	// First, we'll query our channel graph time series for its highest
	// known channel ID.
	newestChan, err := g.cfg.channelSeries.HighestChanID(g.cfg.chainHash)
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
		startHeight = uint32(newestChan.BlockHeight - chanRangeQueryBuffer)
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
	g.curQueryRangeMsg = query

	return query, nil
}

// replyPeerQueries is called in response to any query by the remote peer.
// We'll examine our state and send back our best response.
func (g *GossipSyncer) replyPeerQueries(msg lnwire.Message) error {
	reservation := g.rateLimiter.Reserve()
	delay := reservation.Delay()

	// If we've already replied a handful of times, we will start to delay
	// responses back to the remote peer. This can help prevent DOS attacks
	// where the remote peer spams us endlessly.
	if delay > 0 {
		log.Infof("GossipSyncer(%x): rate limiting gossip replies, "+
			"responding in %s", g.cfg.peerPub[:], delay)

		select {
		case <-time.After(delay):
		case <-g.quit:
			return ErrGossipSyncerExiting
		}
	}

	switch msg := msg.(type) {

	// In this state, we'll also handle any incoming channel range queries
	// from the remote peer as they're trying to sync their state as well.
	case *lnwire.QueryChannelRange:
		return g.replyChanRangeQuery(msg)

	// If the remote peer skips straight to requesting new channels that
	// they don't know of, then we'll ensure that we also handle this case.
	case *lnwire.QueryShortChanIDs:
		return g.replyShortChanIDs(msg)

	default:
		return fmt.Errorf("unknown message: %T", msg)
	}
}

// replyChanRangeQuery will be dispatched in response to a channel range query
// by the remote node. We'll query the channel time series for channels that
// meet the channel range, then chunk our responses to the remote node. We also
// ensure that our final fragment carries the "complete" bit to indicate the
// end of our streaming response.
func (g *GossipSyncer) replyChanRangeQuery(query *lnwire.QueryChannelRange) error {
	// Before responding, we'll check to ensure that the remote peer is
	// querying for the same chain that we're on. If not, we'll send back a
	// response with a complete value of zero to indicate we're on a
	// different chain.
	if g.cfg.chainHash != query.ChainHash {
		log.Warnf("Remote peer requested QueryChannelRange for "+
			"chain=%v, we're on chain=%v", query.ChainHash,
			g.cfg.chainHash)

		return g.cfg.sendToPeerSync(&lnwire.ReplyChannelRange{
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

	// Next, we'll consult the time series to obtain the set of known
	// channel ID's that match their query.
	startBlock := query.FirstBlockHeight
	endBlock := query.LastBlockHeight()
	channelRanges, err := g.cfg.channelSeries.FilterChannelRange(
		query.ChainHash, startBlock, endBlock,
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
	sendReplyForChunk := func(channelChunk []lnwire.ShortChannelID,
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

		return g.cfg.sendToPeerSync(&lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			NumBlocks:        numBlocks,
			FirstBlockHeight: firstHeight,
			Complete:         complete,
			EncodingType:     g.cfg.encodingType,
			ShortChanIDs:     channelChunk,
		})
	}

	var (
		firstHeight  = query.FirstBlockHeight
		lastHeight   uint32
		channelChunk []lnwire.ShortChannelID
	)
	for _, channelRange := range channelRanges {
		channels := channelRange.Channels
		numChannels := int32(len(channels))
		numLeftToAdd := g.cfg.chunkSize - int32(len(channelChunk))

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
		chunkSize := numChannels
		exceedsChunkSize := numChannels > g.cfg.chunkSize
		if exceedsChunkSize {
			rand.Shuffle(len(channels), func(i, j int) {
				channels[i], channels[j] = channels[j], channels[i]
			})
			chunkSize = g.cfg.chunkSize
		}
		channelChunk = channels[:chunkSize]

		// Sort the chunk once again if we had to shuffle it.
		if exceedsChunkSize {
			sort.Slice(channelChunk, func(i, j int) bool {
				return channelChunk[i].ToUint64() <
					channelChunk[j].ToUint64()
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
func (g *GossipSyncer) replyShortChanIDs(query *lnwire.QueryShortChanIDs) error {
	// Before responding, we'll check to ensure that the remote peer is
	// querying for the same chain that we're on. If not, we'll send back a
	// response with a complete value of zero to indicate we're on a
	// different chain.
	if g.cfg.chainHash != query.ChainHash {
		log.Warnf("Remote peer requested QueryShortChanIDs for "+
			"chain=%v, we're on chain=%v", query.ChainHash,
			g.cfg.chainHash)

		return g.cfg.sendToPeerSync(&lnwire.ReplyShortChanIDsEnd{
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
		return fmt.Errorf("unable to fetch chan anns for %v..., %v",
			query.ShortChanIDs[0].ToUint64(), err)
	}

	// Reply with any messages related to those channel ID's, we'll write
	// each one individually and synchronously to throttle the sends and
	// perform buffering of responses in the syncer as opposed to the peer.
	for _, msg := range replyMsgs {
		err := g.cfg.sendToPeerSync(msg)
		if err != nil {
			return err
		}
	}

	// Regardless of whether we had any messages to reply with, send over
	// the sentinel message to signal that the stream has terminated.
	return g.cfg.sendToPeerSync(&lnwire.ReplyShortChanIDsEnd{
		ChainHash: query.ChainHash,
		Complete:  1,
	})
}

// ApplyGossipFilter applies a gossiper filter sent by the remote node to the
// state machine. Once applied, we'll ensure that we don't forward any messages
// to the peer that aren't within the time range of the filter.
func (g *GossipSyncer) ApplyGossipFilter(filter *lnwire.GossipTimestampRange) error {
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

	// Now that the remote peer has applied their filter, we'll query the
	// database for all the messages that are beyond this filter.
	newUpdatestoSend, err := g.cfg.channelSeries.UpdatesInHorizon(
		g.cfg.chainHash, startTime, endTime,
	)
	if err != nil {
		return err
	}

	log.Infof("GossipSyncer(%x): applying new update horizon: start=%v, "+
		"end=%v, backlog_size=%v", g.cfg.peerPub[:], startTime, endTime,
		len(newUpdatestoSend))

	// If we don't have any to send, then we can return early.
	if len(newUpdatestoSend) == 0 {
		return nil
	}

	// We'll conclude by launching a goroutine to send out any updates.
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		for _, msg := range newUpdatestoSend {
			err := g.cfg.sendToPeerSync(msg)
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
func (g *GossipSyncer) FilterGossipMsgs(msgs ...msgWithSenders) {
	// If the peer doesn't have an update horizon set, then we won't send
	// it any new update messages.
	if g.remoteUpdateHorizon == nil {
		return
	}

	// If we've been signaled to exit, or are exiting, then we'll stop
	// short.
	select {
	case <-g.quit:
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
	chanUpdateIndex := make(map[lnwire.ShortChannelID][]*lnwire.ChannelUpdate)
	for _, msg := range msgs {
		chanUpdate, ok := msg.msg.(*lnwire.ChannelUpdate)
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
		case *lnwire.ChannelAnnouncement:
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
		case *lnwire.ChannelUpdate:
			if passesFilter(msg.Timestamp) {
				msgsToSend = append(msgsToSend, msg)
			}

		// Similarly, we only send node announcements if the update
		// timestamp ifs between our set gossip filter time range.
		case *lnwire.NodeAnnouncement:
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

	g.cfg.sendToPeer(msgsToSend...)
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
		syncState := g.syncState()
		if syncState != waitingQueryRangeReply &&
			syncState != waitingQueryChanReply {
			return fmt.Errorf("received unexpected query reply "+
				"message %T", msg)
		}
		msgChan = g.gossipMsgs

	default:
		msgChan = g.gossipMsgs
	}

	select {
	case msgChan <- msg:
	case <-peerQuit:
	case <-g.quit:
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
	case <-g.quit:
		return ErrGossipSyncerExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-g.quit:
		return ErrGossipSyncerExiting
	}
}

// handleSyncTransition handles a new sync type transition request.
//
// NOTE: The gossip syncer might have another sync state as a result of this
// transition.
func (g *GossipSyncer) handleSyncTransition(req *syncTransitionReq) error {
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

	err := g.sendGossipTimestampRange(firstTimestamp, timestampRange)
	if err != nil {
		return fmt.Errorf("unable to send local update horizon: %v", err)
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
	case <-g.quit:
		return ErrGossiperShuttingDown
	}

	select {
	case <-done:
		return nil
	case <-g.quit:
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
