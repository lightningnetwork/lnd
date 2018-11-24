package discovery

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/time/rate"
)

// syncerState is an enum that represents the current state of the
// gossipSyncer.  As the syncer is a state machine, we'll gate our actions
// based off of the current state and the next incoming message.
type syncerState uint32

const (
	// syncingChans is the default state of the gossipSyncer. We start in
	// this state when a new peer first connects and we don't yet know if
	// we're fully synchronized.
	syncingChans syncerState = iota

	// waitingQueryRangeReply is the second main phase of the gossipSyncer.
	// We enter this state after we send out our first QueryChannelRange
	// reply. We'll stay in this state until the remote party sends us a
	// ReplyShortChanIDsEnd message that indicates they've responded to our
	// query entirely. After this state, we'll transition to
	// waitingQueryChanReply after we send out requests for all the new
	// chan ID's to us.
	waitingQueryRangeReply

	// queryNewChannels is the third main phase of the gossipSyncer.  In
	// this phase we'll send out all of our QueryShortChanIDs messages in
	// response to the new channels that we don't yet know about.
	queryNewChannels

	// waitingQueryChanReply is the fourth main phase of the gossipSyncer.
	// We enter this phase once we've sent off a query chink to the remote
	// peer.  We'll stay in this phase until we receive a
	// ReplyShortChanIDsEnd message which indicates that the remote party
	// has responded to all of our requests.
	waitingQueryChanReply

	// chansSynced is the terminal stage of the gossipSyncer. Once we enter
	// this phase, we'll send out our update horizon, which filters out the
	// set of channel updates that we're interested in. In this state,
	// we'll be able to accept any outgoing messages from the
	// AuthenticatedGossiper, and decide if we should forward them to our
	// target peer based on its update horizon.
	chansSynced
)

const (
	// DefaultMaxUndelayedQueryReplies specifies how many gossip queries we
	// will respond to immediately before starting to delay responses.
	DefaultMaxUndelayedQueryReplies = 10

	// DefaultDelayedQueryReplyInterval is the length of time we will wait
	// before responding to gossip queries after replying to
	// maxUndelayedQueryReplies queries.
	DefaultDelayedQueryReplyInterval = 5 * time.Second
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

	default:
		return "UNKNOWN STATE"
	}
}

var (
	// encodingTypeToChunkSize maps an encoding type, to the max number of
	// short chan ID's using the encoding type that we can fit into a
	// single message safely.
	encodingTypeToChunkSize = map[lnwire.ShortChanIDEncoding]int32{
		lnwire.EncodingSortedPlain: 8000,
	}

	// ErrGossipSyncerExiting signals that the syncer has been killed.
	ErrGossipSyncerExiting = errors.New("gossip syncer exiting")
)

const (
	// chanRangeQueryBuffer is the number of blocks back that we'll go when
	// asking the remote peer for their any channels they know of beyond
	// our highest known channel ID.
	chanRangeQueryBuffer = 144
)

// ChannelGraphTimeSeries is an interface that provides time and block based
// querying into our view of the channel graph. New channels will have
// monotonically increasing block heights, and new channel updates will have
// increasing timestamps. Once we connect to a peer, we'll use the methods in
// this interface to determine if we're already in sync, or need to request
// some new information from them.
type ChannelGraphTimeSeries interface {
	// HighestChanID should return the channel ID of the channel we know of
	// that's furthest in the target chain. This channel will have a block
	// height that's close to the current tip of the main chain as we
	// know it.  We'll use this to start our QueryChannelRange dance with
	// the remote node.
	HighestChanID(chain chainhash.Hash) (*lnwire.ShortChannelID, error)

	// UpdatesInHorizon returns all known channel and node updates with an
	// update timestamp between the start time and end time. We'll use this
	// to catch up a remote node to the set of channel updates that they
	// may have missed out on within the target chain.
	UpdatesInHorizon(chain chainhash.Hash,
		startTime time.Time, endTime time.Time) ([]lnwire.Message, error)

	// FilterKnownChanIDs takes a target chain, and a set of channel ID's,
	// and returns a filtered set of chan ID's. This filtered set of chan
	// ID's represents the ID's that we don't know of which were in the
	// passed superSet.
	FilterKnownChanIDs(chain chainhash.Hash,
		superSet []lnwire.ShortChannelID) ([]lnwire.ShortChannelID, error)

	// FilterChannelRange returns the set of channels that we created
	// between the start height and the end height. We'll use this to to a
	// remote peer's QueryChannelRange message.
	FilterChannelRange(chain chainhash.Hash,
		startHeight, endHeight uint32) ([]lnwire.ShortChannelID, error)

	// FetchChanAnns returns a full set of channel announcements as well as
	// their updates that match the set of specified short channel ID's.
	// We'll use this to reply to a QueryShortChanIDs message sent by a
	// remote peer. The response will contain a unique set of
	// ChannelAnnouncements, the latest ChannelUpdate for each of the
	// announcements, and a unique set of NodeAnnouncements.
	FetchChanAnns(chain chainhash.Hash,
		shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error)

	// FetchChanUpdates returns the latest channel update messages for the
	// specified short channel ID. If no channel updates are known for the
	// channel, then an empty slice will be returned.
	FetchChanUpdates(chain chainhash.Hash,
		shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate, error)
}

// gossipSyncerCfg is a struct that packages all the information a gossipSyncer
// needs to carry out its duties.
type gossipSyncerCfg struct {
	// chainHash is the chain that this syncer is responsible for.
	chainHash chainhash.Hash

	// syncChanUpdates is a bool that indicates if we should request a
	// continual channel update stream or not.
	syncChanUpdates bool

	// channelSeries is the primary interface that we'll use to generate
	// our queries and respond to the queries of the remote peer.
	channelSeries ChannelGraphTimeSeries

	// encodingType is the current encoding type we're aware of. Requests
	// with different encoding types will be rejected.
	encodingType lnwire.ShortChanIDEncoding

	// chunkSize is the max number of short chan IDs using the syncer's
	// encoding type that we can fit into a single message safely.
	chunkSize int32

	// sendToPeer is a function closure that should send the set of
	// targeted messages to the peer we've been assigned to sync the graph
	// state from.
	sendToPeer func(...lnwire.Message) error

	// maxUndelayedQueryReplies specifies how many gossip queries we will
	// respond to immediately before starting to delay responses.
	maxUndelayedQueryReplies int

	// delayedQueryReplyInterval is the length of time we will wait before
	// responding to gossip queries after replying to
	// maxUndelayedQueryReplies queries.
	delayedQueryReplyInterval time.Duration
}

// gossipSyncer is a struct that handles synchronizing the channel graph state
// with a remote peer. The gossipSyncer implements a state machine that will
// progressively ensure we're synchronized with the channel state of the remote
// node. Once both nodes have been synchronized, we'll use an update filter to
// filter out which messages should be sent to a remote peer based on their
// update horizon. If the update horizon isn't specified, then we won't send
// them any channel updates at all.
//
// TODO(roasbeef): modify to only sync from one peer at a time?
type gossipSyncer struct {
	started uint32
	stopped uint32

	// remoteUpdateHorizon is the update horizon of the remote peer. We'll
	// use this to properly filter out any messages.
	remoteUpdateHorizon *lnwire.GossipTimestampRange

	// localUpdateHorizon is our local update horizon, we'll use this to
	// determine if we've already sent out our update.
	localUpdateHorizon *lnwire.GossipTimestampRange

	// state is the current state of the gossipSyncer.
	//
	// NOTE: This variable MUST be used atomically.
	state uint32

	// gossipMsgs is a channel that all messages from the target peer will
	// be sent over.
	gossipMsgs chan lnwire.Message

	// bufferedChanRangeReplies is used in the waitingQueryChanReply to
	// buffer all the chunked response to our query.
	bufferedChanRangeReplies []lnwire.ShortChannelID

	// newChansToQuery is used to pass the set of channels we should query
	// for from the waitingQueryChanReply state to the queryNewChannels
	// state.
	newChansToQuery []lnwire.ShortChannelID

	// peerPub is the public key of the peer we're syncing with, serialized
	// in compressed format.
	peerPub [33]byte

	cfg gossipSyncerCfg

	// rateLimiter dictates the frequency with which we will reply to gossip
	// queries from a peer. This is used to delay responses to peers to
	// prevent DOS vulnerabilities if they are spamming with an unreasonable
	// number of queries.
	rateLimiter *rate.Limiter

	sync.Mutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// newGossiperSyncer returns a new instance of the gossipSyncer populated using
// the passed config.
func newGossiperSyncer(cfg gossipSyncerCfg) *gossipSyncer {
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

	return &gossipSyncer{
		cfg:         cfg,
		rateLimiter: rateLimiter,
		gossipMsgs:  make(chan lnwire.Message, 100),
		quit:        make(chan struct{}),
	}
}

// Start starts the gossipSyncer and any goroutines that it needs to carry out
// its duties.
func (g *gossipSyncer) Start() error {
	if !atomic.CompareAndSwapUint32(&g.started, 0, 1) {
		return nil
	}

	log.Debugf("Starting gossipSyncer(%x)", g.peerPub[:])

	g.wg.Add(1)
	go g.channelGraphSyncer()

	return nil
}

// Stop signals the gossipSyncer for a graceful exit, then waits until it has
// exited.
func (g *gossipSyncer) Stop() error {
	if !atomic.CompareAndSwapUint32(&g.stopped, 0, 1) {
		return nil
	}

	close(g.quit)

	g.wg.Wait()

	return nil
}

// channelGraphSyncer is the main goroutine responsible for ensuring that we
// properly channel graph state with the remote peer, and also that we only
// send them messages which actually pass their defined update horizon.
func (g *gossipSyncer) channelGraphSyncer() {
	defer g.wg.Done()

	// TODO(roasbeef): also add ability to force transition back to syncing
	// chans
	//  * needed if we want to sync chan state very few blocks?

	for {
		state := atomic.LoadUint32(&g.state)
		log.Debugf("gossipSyncer(%x): state=%v", g.peerPub[:],
			syncerState(state))

		switch syncerState(state) {
		// When we're in this state, we're trying to synchronize our
		// view of the network with the remote peer. We'll kick off
		// this sync by asking them for the set of channels they
		// understand, as we'll as responding to any other queries by
		// them.
		case syncingChans:
			// If we're in this state, then we'll send the remote
			// peer our opening QueryChannelRange message.
			queryRangeMsg, err := g.genChanRangeQuery()
			if err != nil {
				log.Errorf("unable to gen chan range "+
					"query: %v", err)
				return
			}

			err = g.cfg.sendToPeer(queryRangeMsg)
			if err != nil {
				log.Errorf("unable to send chan range "+
					"query: %v", err)
				return
			}

			// With the message sent successfully, we'll transition
			// into the next state where we wait for their reply.
			atomic.StoreUint32(&g.state, uint32(waitingQueryRangeReply))

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
						log.Errorf("unable to "+
							"process chan range "+
							"query: %v", err)
						return
					}

					continue
				}

				// Otherwise, it's the remote peer performing a
				// query, which we'll attempt to reply to.
				err := g.replyPeerQueries(msg)
				if err != nil && err != ErrGossipSyncerExiting {
					log.Errorf("unable to reply to peer "+
						"query: %v", err)
				}

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
				log.Errorf("unable to sync chan IDs: %v", err)
			}

			// If this wasn't our last query, then we'll need to
			// transition to our waiting state.
			if !done {
				atomic.StoreUint32(&g.state, uint32(waitingQueryChanReply))
				continue
			}

			// If we're fully synchronized, then we can transition
			// to our terminal state.
			atomic.StoreUint32(&g.state, uint32(chansSynced))

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
					atomic.StoreUint32(&g.state, uint32(queryNewChannels))
					continue
				}

				// Otherwise, it's the remote peer performing a
				// query, which we'll attempt to deploy to.
				err := g.replyPeerQueries(msg)
				if err != nil && err != ErrGossipSyncerExiting {
					log.Errorf("unable to reply to peer "+
						"query: %v", err)
				}

			case <-g.quit:
				return
			}

		// This is our final terminal state where we'll only reply to
		// any further queries by the remote peer.
		case chansSynced:
			// If we haven't yet sent out our update horizon, and
			// we want to receive real-time channel updates, we'll
			// do so now.
			if g.localUpdateHorizon == nil && g.cfg.syncChanUpdates {
				// TODO(roasbeef): query DB for most recent
				// update?

				// We'll give an hours room in our update
				// horizon to ensure we don't miss any newer
				// items.
				updateHorizon := time.Now().Add(-time.Hour * 1)
				log.Infof("gossipSyncer(%x): applying "+
					"gossipFilter(start=%v)", g.peerPub[:],
					updateHorizon)

				g.localUpdateHorizon = &lnwire.GossipTimestampRange{
					ChainHash:      g.cfg.chainHash,
					FirstTimestamp: uint32(updateHorizon.Unix()),
					TimestampRange: math.MaxUint32,
				}
				err := g.cfg.sendToPeer(g.localUpdateHorizon)
				if err != nil {
					log.Errorf("unable to send update "+
						"horizon: %v", err)
				}
			}

			// With our horizon set, we'll simply reply to any new
			// message and exit if needed.
			select {
			case msg := <-g.gossipMsgs:
				err := g.replyPeerQueries(msg)
				if err != nil && err != ErrGossipSyncerExiting {
					log.Errorf("unable to reply to peer "+
						"query: %v", err)
				}

			case <-g.quit:
				return
			}
		}
	}
}

// synchronizeChanIDs is called by the channelGraphSyncer when we need to query
// the remote peer for its known set of channel IDs within a particular block
// range. This method will be called continually until the entire range has
// been queried for with a response received. We'll chunk our requests as
// required to ensure they fit into a single message. We may re-renter this
// state in the case that chunking is required.
func (g *gossipSyncer) synchronizeChanIDs() (bool, error) {
	// If we're in this state yet there are no more new channels to query
	// for, then we'll transition to our final synced state and return true
	// to signal that we're fully synchronized.
	if len(g.newChansToQuery) == 0 {
		log.Infof("gossipSyncer(%x): no more chans to query",
			g.peerPub[:])
		return true, nil
	}

	// Otherwise, we'll issue our next chunked query to receive replies
	// for.
	var queryChunk []lnwire.ShortChannelID

	// If the number of channels to query for is less than the chunk size,
	// then we can issue a single query.
	if int32(len(g.newChansToQuery)) < g.cfg.chunkSize {
		queryChunk = g.newChansToQuery
		g.newChansToQuery = nil

	} else {
		// Otherwise, we'll need to only query for the next chunk.
		// We'll slice into our query chunk, then slide down our main
		// pointer down by the chunk size.
		queryChunk = g.newChansToQuery[:g.cfg.chunkSize]
		g.newChansToQuery = g.newChansToQuery[g.cfg.chunkSize:]
	}

	log.Infof("gossipSyncer(%x): querying for %v new channels",
		g.peerPub[:], len(queryChunk))

	// With our chunk obtained, we'll send over our next query, then return
	// false indicating that we're net yet fully synced.
	err := g.cfg.sendToPeer(&lnwire.QueryShortChanIDs{
		ChainHash:    g.cfg.chainHash,
		EncodingType: lnwire.EncodingSortedPlain,
		ShortChanIDs: queryChunk,
	})

	return false, err
}

// processChanRangeReply is called each time the gossipSyncer receives a new
// reply to the initial range query to discover new channels that it didn't
// previously know of.
func (g *gossipSyncer) processChanRangeReply(msg *lnwire.ReplyChannelRange) error {
	g.bufferedChanRangeReplies = append(
		g.bufferedChanRangeReplies, msg.ShortChanIDs...,
	)

	log.Infof("gossipSyncer(%x): buffering chan range reply of size=%v",
		g.peerPub[:], len(msg.ShortChanIDs))

	// If this isn't the last response, then we can exit as we've already
	// buffered the latest portion of the streaming reply.
	if msg.Complete == 0 {
		return nil
	}

	log.Infof("gossipSyncer(%x): filtering through %v chans", g.peerPub[:],
		len(g.bufferedChanRangeReplies))

	// Otherwise, this is the final response, so we'll now check to see
	// which channels they know of that we don't.
	newChans, err := g.cfg.channelSeries.FilterKnownChanIDs(
		g.cfg.chainHash, g.bufferedChanRangeReplies,
	)
	if err != nil {
		return fmt.Errorf("unable to filter chan ids: %v", err)
	}

	// As we've received the entirety of the reply, we no longer need to
	// hold on to the set of buffered replies, so we'll let that be garbage
	// collected now.
	g.bufferedChanRangeReplies = nil

	// If there aren't any channels that we don't know of, then we can
	// switch straight to our terminal state.
	if len(newChans) == 0 {
		log.Infof("gossipSyncer(%x): remote peer has no new chans",
			g.peerPub[:])

		atomic.StoreUint32(&g.state, uint32(chansSynced))
		return nil
	}

	// Otherwise, we'll set the set of channels that we need to query for
	// the next state, and also transition our state.
	g.newChansToQuery = newChans
	atomic.StoreUint32(&g.state, uint32(queryNewChannels))

	log.Infof("gossipSyncer(%x): starting query for %v new chans",
		g.peerPub[:], len(newChans))

	return nil
}

// genChanRangeQuery generates the initial message we'll send to the remote
// party when we're kicking off the channel graph synchronization upon
// connection.
func (g *gossipSyncer) genChanRangeQuery() (*lnwire.QueryChannelRange, error) {
	// First, we'll query our channel graph time series for its highest
	// known channel ID.
	newestChan, err := g.cfg.channelSeries.HighestChanID(g.cfg.chainHash)
	if err != nil {
		return nil, err
	}

	// Once we have the chan ID of the newest, we'll obtain the block
	// height of the channel, then subtract our default horizon to ensure
	// we don't miss any channels. By default, we go back 1 day from the
	// newest channel.
	var startHeight uint32
	switch {
	case newestChan.BlockHeight <= chanRangeQueryBuffer:
		fallthrough
	case newestChan.BlockHeight == 0:
		startHeight = 0

	default:
		startHeight = uint32(newestChan.BlockHeight - chanRangeQueryBuffer)
	}

	log.Infof("gossipSyncer(%x): requesting new chans from height=%v "+
		"and %v blocks after", g.peerPub[:], startHeight,
		math.MaxUint32-startHeight)

	// Finally, we'll craft the channel range query, using our starting
	// height, then asking for all known channels to the foreseeable end of
	// the main chain.
	return &lnwire.QueryChannelRange{
		ChainHash:        g.cfg.chainHash,
		FirstBlockHeight: startHeight,
		NumBlocks:        math.MaxUint32 - startHeight,
	}, nil
}

// replyPeerQueries is called in response to any query by the remote peer.
// We'll examine our state and send back our best response.
func (g *gossipSyncer) replyPeerQueries(msg lnwire.Message) error {
	reservation := g.rateLimiter.Reserve()
	delay := reservation.Delay()

	// If we've already replied a handful of times, we will start to delay
	// responses back to the remote peer. This can help prevent DOS attacks
	// where the remote peer spams us endlessly.
	if delay > 0 {
		log.Infof("gossipSyncer(%x): rate limiting gossip replies, "+
			"responding in %s", g.peerPub[:], delay)

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
func (g *gossipSyncer) replyChanRangeQuery(query *lnwire.QueryChannelRange) error {
	log.Infof("gossipSyncer(%x): filtering chan range: start_height=%v, "+
		"num_blocks=%v", g.peerPub[:], query.FirstBlockHeight,
		query.NumBlocks)

	// Next, we'll consult the time series to obtain the set of known
	// channel ID's that match their query.
	startBlock := query.FirstBlockHeight
	channelRange, err := g.cfg.channelSeries.FilterChannelRange(
		query.ChainHash, startBlock, startBlock+query.NumBlocks,
	)
	if err != nil {
		return err
	}

	// TODO(roasbeef): means can't send max uint above?
	//  * or make internal 64

	numChannels := int32(len(channelRange))
	numChansSent := int32(0)
	for {
		// We'll send our this response in a streaming manner,
		// chunk-by-chunk. We do this as there's a transport message
		// size limit which we'll need to adhere to.
		var channelChunk []lnwire.ShortChannelID

		// We know this is the final chunk, if the difference between
		// the total number of channels, and the number of channels
		// we've sent is less-than-or-equal to the chunk size.
		isFinalChunk := (numChannels - numChansSent) <= g.cfg.chunkSize

		// If this is indeed the last chunk, then we'll send the
		// remainder of the channels.
		if isFinalChunk {
			channelChunk = channelRange[numChansSent:]

			log.Infof("gossipSyncer(%x): sending final chan "+
				"range chunk, size=%v", g.peerPub[:], len(channelChunk))

		} else {
			// Otherwise, we'll only send off a fragment exactly
			// sized to the proper chunk size.
			channelChunk = channelRange[numChansSent : numChansSent+g.cfg.chunkSize]

			log.Infof("gossipSyncer(%x): sending range chunk of "+
				"size=%v", g.peerPub[:], len(channelChunk))
		}

		// With our chunk assembled, we'll now send to the remote peer
		// the current chunk.
		replyChunk := lnwire.ReplyChannelRange{
			QueryChannelRange: *query,
			Complete:          0,
			EncodingType:      g.cfg.encodingType,
			ShortChanIDs:      channelChunk,
		}
		if isFinalChunk {
			replyChunk.Complete = 1
		}
		if err := g.cfg.sendToPeer(&replyChunk); err != nil {
			return err
		}

		// If this was the final chunk, then we'll exit now as our
		// response is now complete.
		if isFinalChunk {
			return nil
		}

		numChansSent += int32(len(channelChunk))
	}
}

// replyShortChanIDs will be dispatched in response to a query by the remote
// node for information concerning a set of short channel ID's. Our response
// will be sent in a streaming chunked manner to ensure that we remain below
// the current transport level message size.
func (g *gossipSyncer) replyShortChanIDs(query *lnwire.QueryShortChanIDs) error {
	// Before responding, we'll check to ensure that the remote peer is
	// querying for the same chain that we're on. If not, we'll send back a
	// response with a complete value of zero to indicate we're on a
	// different chain.
	if g.cfg.chainHash != query.ChainHash {
		log.Warnf("Remote peer requested QueryShortChanIDs for "+
			"chain=%v, we're on chain=%v", g.cfg.chainHash,
			query.ChainHash)

		return g.cfg.sendToPeer(&lnwire.ReplyShortChanIDsEnd{
			ChainHash: query.ChainHash,
			Complete:  0,
		})
	}

	if len(query.ShortChanIDs) == 0 {
		log.Infof("gossipSyncer(%x): ignoring query for blank short chan ID's",
			g.peerPub[:])
		return nil
	}

	log.Infof("gossipSyncer(%x): fetching chan anns for %v chans",
		g.peerPub[:], len(query.ShortChanIDs))

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

	// If we didn't find any messages related to those channel ID's, then
	// we'll send over a reply marking the end of our response, and exit
	// early.
	if len(replyMsgs) == 0 {
		return g.cfg.sendToPeer(&lnwire.ReplyShortChanIDsEnd{
			ChainHash: query.ChainHash,
			Complete:  1,
		})
	}

	// Otherwise, we'll send over our set of messages responding to the
	// query, with the ending message appended to it.
	replyMsgs = append(replyMsgs, &lnwire.ReplyShortChanIDsEnd{
		ChainHash: query.ChainHash,
		Complete:  1,
	})
	return g.cfg.sendToPeer(replyMsgs...)
}

// ApplyGossipFilter applies a gossiper filter sent by the remote node to the
// state machine. Once applied, we'll ensure that we don't forward any messages
// to the peer that aren't within the time range of the filter.
func (g *gossipSyncer) ApplyGossipFilter(filter *lnwire.GossipTimestampRange) error {
	g.Lock()

	g.remoteUpdateHorizon = filter

	startTime := time.Unix(int64(g.remoteUpdateHorizon.FirstTimestamp), 0)
	endTime := startTime.Add(
		time.Duration(g.remoteUpdateHorizon.TimestampRange) * time.Second,
	)

	g.Unlock()

	// Now that the remote peer has applied their filter, we'll query the
	// database for all the messages that are beyond this filter.
	newUpdatestoSend, err := g.cfg.channelSeries.UpdatesInHorizon(
		g.cfg.chainHash, startTime, endTime,
	)
	if err != nil {
		return err
	}

	log.Infof("gossipSyncer(%x): applying new update horizon: start=%v, "+
		"end=%v, backlog_size=%v", g.peerPub[:], startTime, endTime,
		len(newUpdatestoSend))

	// If we don't have any to send, then we can return early.
	if len(newUpdatestoSend) == 0 {
		return nil
	}

	// We'll conclude by launching a goroutine to send out any updates.
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		if err := g.cfg.sendToPeer(newUpdatestoSend...); err != nil {
			log.Errorf("unable to send messages for peer catch "+
				"up: %v", err)
		}
	}()

	return nil
}

// FilterGossipMsgs takes a set of gossip messages, and only send it to a peer
// iff the message is within the bounds of their set gossip filter. If the peer
// doesn't have a gossip filter set, then no messages will be forwarded.
func (g *gossipSyncer) FilterGossipMsgs(msgs ...msgWithSenders) {
	// If the peer doesn't have an update horizon set, then we won't send
	// it any new update messages.
	if g.remoteUpdateHorizon == nil {
		return
	}

	// If we've been signalled to exit, or are exiting, then we'll stop
	// short.
	if atomic.LoadUint32(&g.stopped) == 1 {
		return
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
		return t.After(startTime) && t.Before(endTime)
	}

	msgsToSend := make([]lnwire.Message, 0, len(msgs))
	for _, msg := range msgs {
		// If the target peer is the peer that sent us this message,
		// then we'll exit early as we don't need to filter this
		// message.
		if _, ok := msg.senders[g.peerPub]; ok {
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

	log.Tracef("gossipSyncer(%x): filtered gossip msgs: set=%v, sent=%v",
		g.peerPub[:], len(msgs), len(msgsToSend))

	if len(msgsToSend) == 0 {
		return
	}

	g.cfg.sendToPeer(msgsToSend...)
}

// ProcessQueryMsg is used by outside callers to pass new channel time series
// queries to the internal processing goroutine.
func (g *gossipSyncer) ProcessQueryMsg(msg lnwire.Message, peerQuit <-chan struct{}) {
	select {
	case g.gossipMsgs <- msg:
	case <-peerQuit:
	case <-g.quit:
	}
}

// SyncerState returns the current syncerState of the target gossipSyncer.
func (g *gossipSyncer) SyncState() syncerState {
	return syncerState(atomic.LoadUint32(&g.state))
}
