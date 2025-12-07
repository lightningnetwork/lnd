package discovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
	"golang.org/x/time/rate"
)

const (
	// DefaultSyncerRotationInterval is the default interval in which we'll
	// rotate a single active syncer.
	DefaultSyncerRotationInterval = 20 * time.Minute

	// DefaultHistoricalSyncInterval is the default interval in which we'll
	// force a historical sync to ensure we have as much of the public
	// network as possible.
	DefaultHistoricalSyncInterval = time.Hour

	// DefaultFilterConcurrency is the default maximum number of concurrent
	// gossip filter applications that can be processed.
	DefaultFilterConcurrency = 5

	// DefaultMsgBytesBurst is the allotted burst in bytes we'll permit.
	// This is the most that can be sent in a given go. Requests beyond
	// this, will block indefinitely. Once tokens (bytes are depleted),
	// they'll be refilled at the DefaultMsgBytesPerSecond rate.
	DefaultMsgBytesBurst = 2 * 1000 * 1_024

	// DefaultMsgBytesPerSecond is the max bytes/s we'll permit for outgoing
	// messages. Once tokens (bytes) have been taken from the bucket,
	// they'll be refilled at this rate.
	DefaultMsgBytesPerSecond = 1000 * 1_024

	// DefaultPeerMsgBytesPerSecond is the max bytes/s we'll permit for
	// outgoing messages for a single peer. Once tokens (bytes) have been
	// taken from the bucket, they'll be refilled at this rate.
	DefaultPeerMsgBytesPerSecond = 50 * 1_024

	// assumedMsgSize is the assumed size of a message if we can't compute
	// its serialized size. This comes out to 1 KB.
	assumedMsgSize = 1_024
)

var (
	// ErrSyncManagerExiting is an error returned when we attempt to
	// start/stop a gossip syncer for a connected/disconnected peer, but the
	// SyncManager has already been stopped.
	ErrSyncManagerExiting = errors.New("sync manager exiting")
)

// newSyncer in an internal message we'll use within the SyncManager to signal
// that we should create a GossipSyncer for a newly connected peer.
type newSyncer struct {
	// peer is the newly connected peer.
	peer lnpeer.Peer

	// doneChan serves as a signal to the caller that the SyncManager's
	// internal state correctly reflects the stale active syncer.
	doneChan chan struct{}
}

// staleSyncer is an internal message we'll use within the SyncManager to signal
// that a peer has disconnected and its GossipSyncer should be removed.
type staleSyncer struct {
	// peer is the peer that has disconnected.
	peer route.Vertex

	// doneChan serves as a signal to the caller that the SyncManager's
	// internal state correctly reflects the stale active syncer. This is
	// needed to ensure we always create a new syncer for a flappy peer
	// after they disconnect if they happened to be an active syncer.
	doneChan chan struct{}
}

// SyncManagerCfg contains all of the dependencies required for the SyncManager
// to carry out its duties.
type SyncManagerCfg struct {
	// ChainHash is a hash that indicates the specific network of the active
	// chain.
	ChainHash chainhash.Hash

	// ChanSeries is an interface that provides access to a time series view
	// of the current known channel graph. Each GossipSyncer enabled peer
	// will utilize this in order to create and respond to channel graph
	// time series queries.
	ChanSeries ChannelGraphTimeSeries

	// NumActiveSyncers is the number of peers for which we should have
	// active syncers with. After reaching NumActiveSyncers, any future
	// gossip syncers will be passive.
	NumActiveSyncers int

	// NoTimestampQueries will prevent the GossipSyncer from querying
	// timestamps of announcement messages from the peer and from responding
	// to timestamp queries
	NoTimestampQueries bool

	// RotateTicker is a ticker responsible for notifying the SyncManager
	// when it should rotate its active syncers. A single active syncer with
	// a chansSynced state will be exchanged for a passive syncer in order
	// to ensure we don't keep syncing with the same peers.
	RotateTicker ticker.Ticker

	// HistoricalSyncTicker is a ticker responsible for notifying the
	// SyncManager when it should attempt a historical sync with a gossip
	// sync peer.
	HistoricalSyncTicker ticker.Ticker

	// IgnoreHistoricalFilters will prevent syncers from replying with
	// historical data when the remote peer sets a gossip_timestamp_range.
	// This prevents ranges with old start times from causing us to dump the
	// graph on connect.
	IgnoreHistoricalFilters bool

	// BestHeight returns the latest height known of the chain.
	BestHeight func() uint32

	// PinnedSyncers is a set of peers that will always transition to
	// ActiveSync upon connection. These peers will never transition to
	// PassiveSync.
	PinnedSyncers PinnedSyncers

	// IsStillZombieChannel takes the timestamps of the latest channel
	// updates for a channel and returns true if the channel should be
	// considered a zombie based on these timestamps.
	IsStillZombieChannel func(time.Time, time.Time) bool

	// AllotedMsgBytesPerSecond is the allotted bandwidth rate, expressed in
	// bytes/second that the gossip manager can consume. Once we exceed this
	// rate, message sending will block until we're below the rate.
	AllotedMsgBytesPerSecond uint64

	// AllotedMsgBytesBurst is the amount of burst bytes we'll permit, if
	// we've exceeded the hard upper limit.
	AllotedMsgBytesBurst uint64

	// FilterConcurrency is the maximum number of concurrent gossip filter
	// applications that can be processed. If not set, defaults to 5.
	FilterConcurrency int

	// PeerMsgBytesPerSecond is the allotted bandwidth rate, expressed in
	// bytes/second that a single gossip syncer can consume. Once we exceed
	// this rate, message sending will block until we're below the rate.
	PeerMsgBytesPerSecond uint64
}

// SyncManager is a subsystem of the gossiper that manages the gossip syncers
// for peers currently connected. When a new peer is connected, the manager will
// create its accompanying gossip syncer and determine whether it should have an
// ActiveSync or PassiveSync sync type based on how many other gossip syncers
// are currently active. Any ActiveSync gossip syncers are started in a
// round-robin manner to ensure we're not syncing with multiple peers at the
// same time. The first GossipSyncer registered with the SyncManager will
// attempt a historical sync to ensure we have as much of the public channel
// graph as possible.
type SyncManager struct {
	// initialHistoricalSyncCompleted serves as a barrier when initializing
	// new active GossipSyncers. If 0, the initial historical sync has not
	// completed, so we'll defer initializing any active GossipSyncers. If
	// 1, then we can transition the GossipSyncer immediately. We set up
	// this barrier to ensure we have most of the graph before attempting to
	// accept new updates at tip.
	//
	// NOTE: This must be used atomically.
	initialHistoricalSyncCompleted int32

	start sync.Once
	stop  sync.Once

	cfg SyncManagerCfg

	// newSyncers is a channel we'll use to process requests to create
	// GossipSyncers for newly connected peers.
	newSyncers chan *newSyncer

	// staleSyncers is a channel we'll use to process requests to tear down
	// GossipSyncers for disconnected peers.
	staleSyncers chan *staleSyncer

	// syncersMu guards the read and write access to the activeSyncers and
	// inactiveSyncers maps below.
	syncersMu sync.Mutex

	// activeSyncers is the set of all syncers for which we are currently
	// receiving graph updates from. The number of possible active syncers
	// is bounded by NumActiveSyncers.
	activeSyncers map[route.Vertex]*GossipSyncer

	// inactiveSyncers is the set of all syncers for which we are not
	// currently receiving new graph updates from.
	inactiveSyncers map[route.Vertex]*GossipSyncer

	// pinnedActiveSyncers is the set of all syncers which are pinned into
	// an active sync. Pinned peers performan an initial historical sync on
	// each connection and will continue to receive graph updates for the
	// duration of the connection.
	pinnedActiveSyncers map[route.Vertex]*GossipSyncer

	// gossipFilterSema contains semaphores for the gossip timestamp
	// queries.
	gossipFilterSema chan struct{}

	// rateLimiter dictates the frequency with which we will reply to gossip
	// queries from all peers. This is used to delay responses to peers to
	// prevent DOS vulnerabilities if they are spamming with an unreasonable
	// number of queries.
	rateLimiter *rate.Limiter

	wg   sync.WaitGroup
	quit chan struct{}
}

// newSyncManager constructs a new SyncManager backed by the given config.
func newSyncManager(cfg *SyncManagerCfg) *SyncManager {
	filterConcurrency := cfg.FilterConcurrency
	if filterConcurrency == 0 {
		filterConcurrency = DefaultFilterConcurrency
	}

	filterSema := make(chan struct{}, filterConcurrency)
	for i := 0; i < filterConcurrency; i++ {
		filterSema <- struct{}{}
	}

	bytesPerSecond := cfg.AllotedMsgBytesPerSecond
	if bytesPerSecond == 0 {
		bytesPerSecond = DefaultMsgBytesPerSecond
	}

	bytesBurst := cfg.AllotedMsgBytesBurst
	if bytesBurst == 0 {
		bytesBurst = DefaultMsgBytesBurst
	}

	// We'll use this rate limiter to limit our total outbound bandwidth for
	// gossip queries peers.
	rateLimiter := rate.NewLimiter(
		rate.Limit(bytesPerSecond), int(bytesBurst),
	)

	return &SyncManager{
		cfg:          *cfg,
		rateLimiter:  rateLimiter,
		newSyncers:   make(chan *newSyncer),
		staleSyncers: make(chan *staleSyncer),
		activeSyncers: make(
			map[route.Vertex]*GossipSyncer, cfg.NumActiveSyncers,
		),
		inactiveSyncers: make(map[route.Vertex]*GossipSyncer),
		pinnedActiveSyncers: make(
			map[route.Vertex]*GossipSyncer, len(cfg.PinnedSyncers),
		),
		gossipFilterSema: filterSema,
		quit:             make(chan struct{}),
	}
}

// Start starts the SyncManager in order to properly carry out its duties.
func (m *SyncManager) Start() {
	m.start.Do(func() {
		m.wg.Add(1)
		go m.syncerHandler()
	})
}

// Stop stops the SyncManager from performing its duties.
func (m *SyncManager) Stop() {
	m.stop.Do(func() {
		log.Debugf("SyncManager is stopping")
		defer log.Debugf("SyncManager stopped")

		close(m.quit)
		m.wg.Wait()

		for _, syncer := range m.inactiveSyncers {
			syncer.Stop()
		}
		for _, syncer := range m.activeSyncers {
			syncer.Stop()
		}
	})
}

// syncerHandler is the SyncManager's main event loop responsible for:
//
// 1. Creating and tearing down GossipSyncers for connected/disconnected peers.

// 2. Finding new peers to receive graph updates from to ensure we don't only
//    receive them from the same set of peers.

//  3. Finding new peers to force a historical sync with to ensure we have as
//     much of the public network as possible.
//
// NOTE: This must be run as a goroutine.
func (m *SyncManager) syncerHandler() {
	defer m.wg.Done()

	m.cfg.RotateTicker.Resume()
	defer m.cfg.RotateTicker.Stop()

	defer m.cfg.HistoricalSyncTicker.Stop()

	var (
		// initialHistoricalSyncer is the syncer we are currently
		// performing an initial historical sync with.
		initialHistoricalSyncer *GossipSyncer

		// initialHistoricalSyncSignal is a signal that will fire once
		// the initial historical sync has been completed. This is
		// crucial to ensure that another historical sync isn't
		// attempted just because the initialHistoricalSyncer was
		// disconnected.
		initialHistoricalSyncSignal chan struct{}
	)

	setInitialHistoricalSyncer := func(s *GossipSyncer) {
		initialHistoricalSyncer = s
		initialHistoricalSyncSignal = s.ResetSyncedSignal()

		// Restart the timer for our new historical sync peer. This will
		// ensure that all initial syncers receive an equivalent
		// duration before attempting the next sync. Without doing so we
		// might attempt two historical sync back to back if a peer
		// disconnects just before the ticker fires.
		m.cfg.HistoricalSyncTicker.Pause()
		m.cfg.HistoricalSyncTicker.Resume()
	}

	for {
		select {
		// A new peer has been connected, so we'll create its
		// accompanying GossipSyncer.
		case newSyncer := <-m.newSyncers:
			// If we already have a syncer, then we'll exit early as
			// we don't want to override it.
			if _, ok := m.GossipSyncer(newSyncer.peer.PubKey()); ok {
				close(newSyncer.doneChan)
				continue
			}

			s := m.createGossipSyncer(newSyncer.peer)

			isPinnedSyncer := m.isPinnedSyncer(s)

			// attemptHistoricalSync determines whether we should
			// attempt an initial historical sync when a new peer
			// connects.
			attemptHistoricalSync := false

			m.syncersMu.Lock()
			switch {
			// For pinned syncers, we will immediately transition
			// the peer into an active (pinned) sync state.
			case isPinnedSyncer:
				attemptHistoricalSync = true
				s.setSyncType(PinnedSync)
				s.setSyncState(syncerIdle)
				m.pinnedActiveSyncers[s.cfg.peerPub] = s

			// Regardless of whether the initial historical sync
			// has completed, we'll re-trigger a historical sync if
			// we no longer have any syncers. This might be
			// necessary if we lost all our peers at one point, and
			// now we finally have one again.
			case len(m.activeSyncers) == 0 &&
				len(m.inactiveSyncers) == 0:

				attemptHistoricalSync =
					m.cfg.NumActiveSyncers > 0
				fallthrough

			// If we've exceeded our total number of active syncers,
			// we'll initialize this GossipSyncer as passive.
			case len(m.activeSyncers) >= m.cfg.NumActiveSyncers:
				fallthrough

			// If the initial historical sync has yet to complete,
			// then we'll declare it as passive and attempt to
			// transition it when the initial historical sync
			// completes.
			case !m.IsGraphSynced():
				s.setSyncType(PassiveSync)
				m.inactiveSyncers[s.cfg.peerPub] = s

			// The initial historical sync has completed, so we can
			// immediately start the GossipSyncer as active.
			default:
				s.setSyncType(ActiveSync)
				m.activeSyncers[s.cfg.peerPub] = s
			}
			m.syncersMu.Unlock()

			s.Start()

			// Once we create the GossipSyncer, we'll signal to the
			// caller that they can proceed since the SyncManager's
			// internal state has been updated.
			close(newSyncer.doneChan)

			// We'll force a historical sync with the first peer we
			// connect to, to ensure we get as much of the graph as
			// possible.
			if !attemptHistoricalSync {
				continue
			}

			log.Debugf("Attempting initial historical sync with "+
				"GossipSyncer(%x)", s.cfg.peerPub)

			if err := s.historicalSync(); err != nil {
				log.Errorf("Unable to attempt initial "+
					"historical sync with "+
					"GossipSyncer(%x): %v", s.cfg.peerPub,
					err)
				continue
			}

			// Once the historical sync has started, we'll get a
			// keep track of the corresponding syncer to properly
			// handle disconnects. We'll also use a signal to know
			// when the historical sync completed.
			if !isPinnedSyncer {
				setInitialHistoricalSyncer(s)
			}

		// An existing peer has disconnected, so we'll tear down its
		// corresponding GossipSyncer.
		case staleSyncer := <-m.staleSyncers:
			// Once the corresponding GossipSyncer has been stopped
			// and removed, we'll signal to the caller that they can
			// proceed since the SyncManager's internal state has
			// been updated.
			m.removeGossipSyncer(staleSyncer.peer)
			close(staleSyncer.doneChan)

			// If we don't have an initialHistoricalSyncer, or we do
			// but it is not the peer being disconnected, then we
			// have nothing left to do and can proceed.
			switch {
			case initialHistoricalSyncer == nil:
				fallthrough
			case staleSyncer.peer != initialHistoricalSyncer.cfg.peerPub:
				fallthrough
			case m.cfg.NumActiveSyncers == 0:
				continue
			}

			// Otherwise, our initialHistoricalSyncer corresponds to
			// the peer being disconnected, so we'll have to find a
			// replacement.
			log.Debug("Finding replacement for initial " +
				"historical sync")

			s := m.forceHistoricalSync()
			if s == nil {
				log.Debug("No eligible replacement found " +
					"for initial historical sync")
				continue
			}

			log.Debugf("Replaced initial historical "+
				"GossipSyncer(%v) with GossipSyncer(%x)",
				staleSyncer.peer, s.cfg.peerPub)

			setInitialHistoricalSyncer(s)

		// Our initial historical sync signal has completed, so we'll
		// nil all of the relevant fields as they're no longer needed.
		case <-initialHistoricalSyncSignal:
			initialHistoricalSyncer = nil
			initialHistoricalSyncSignal = nil

			log.Debug("Initial historical sync completed")

			// With the initial historical sync complete, we can
			// begin receiving new graph updates at tip. We'll
			// determine whether we can have any more active
			// GossipSyncers. If we do, we'll randomly select some
			// that are currently passive to transition.
			m.syncersMu.Lock()
			numActiveLeft := m.cfg.NumActiveSyncers - len(m.activeSyncers)
			if numActiveLeft <= 0 {
				m.syncersMu.Unlock()
				continue
			}

			// We may not even have enough inactive syncers to be
			// transitted. In that case, we will transit all the
			// inactive syncers.
			if len(m.inactiveSyncers) < numActiveLeft {
				numActiveLeft = len(m.inactiveSyncers)
			}

			log.Debugf("Attempting to transition %v passive "+
				"GossipSyncers to active", numActiveLeft)

			for i := 0; i < numActiveLeft; i++ {
				chooseRandomSyncer(
					m.inactiveSyncers, m.transitionPassiveSyncer,
				)
			}

			m.syncersMu.Unlock()

		// Our RotateTicker has ticked, so we'll attempt to rotate a
		// single active syncer with a passive one.
		case <-m.cfg.RotateTicker.Ticks():
			m.rotateActiveSyncerCandidate()

		// Our HistoricalSyncTicker has ticked, so we'll randomly select
		// a peer and force a historical sync with them.
		case <-m.cfg.HistoricalSyncTicker.Ticks():
			// To be extra cautious, gate the forceHistoricalSync
			// call such that it can only execute if we are
			// configured to have a non-zero number of sync peers.
			// This way even if the historical sync ticker manages
			// to tick we can be sure that a historical sync won't
			// accidentally begin.
			if m.cfg.NumActiveSyncers == 0 {
				continue
			}

			// If we don't have a syncer available we have nothing
			// to do.
			s := m.forceHistoricalSync()
			if s == nil {
				continue
			}

			// If we've already completed a historical sync, we'll
			// skip setting the initial historical syncer.
			if m.IsGraphSynced() {
				continue
			}

			// Otherwise, we'll track the peer we've performed a
			// historical sync with in order to handle the case
			// where our previous historical sync peer did not
			// respond to our queries and we haven't ingested as
			// much of the graph as we should.
			setInitialHistoricalSyncer(s)

		case <-m.quit:
			return
		}
	}
}

// isPinnedSyncer returns true if the passed GossipSyncer is one of our pinned
// sync peers.
func (m *SyncManager) isPinnedSyncer(s *GossipSyncer) bool {
	_, isPinnedSyncer := m.cfg.PinnedSyncers[s.cfg.peerPub]
	return isPinnedSyncer
}

// deriveRateLimitReservation will take the current message and derive a
// reservation that can be used to wait on the rate limiter.
func deriveRateLimitReservation(rl *rate.Limiter,
	msg lnwire.Message) (*rate.Reservation, error) {

	var (
		msgSize uint32
		err     error
	)

	// Figure out the serialized size of the message. If we can't easily
	// compute it, then we'll used the assumed msg size.
	if sMsg, ok := msg.(lnwire.SizeableMessage); ok {
		msgSize, err = sMsg.SerializedSize()
		if err != nil {
			return nil, err
		}
	} else {
		log.Warnf("Unable to compute serialized size of %T", msg)

		msgSize = assumedMsgSize
	}

	return rl.ReserveN(time.Now(), int(msgSize)), nil
}

// waitMsgDelay takes a delay, and waits until it has finished.
func waitMsgDelay(ctx context.Context, peerPub [33]byte,
	limitReservation *rate.Reservation, quit <-chan struct{}) error {

	// If we've already replied a handful of times, we will start to delay
	// responses back to the remote peer. This can help prevent DOS attacks
	// where the remote peer spams us endlessly.
	//
	// We skip checking for reservation.OK() here, as during config
	// validation, we ensure that the burst is enough for a single message
	// to be sent.
	delay := limitReservation.Delay()
	if delay > 0 {
		log.Debugf("GossipSyncer(%x): rate limiting gossip replies, "+
			"responding in %s", peerPub, delay)

		select {
		case <-time.After(delay):

		case <-ctx.Done():
			limitReservation.Cancel()

			return ErrGossipSyncerExiting

		case <-quit:
			limitReservation.Cancel()

			return ErrGossipSyncerExiting
		}
	}

	return nil
}

// maybeRateLimitMsg takes a message, and may wait a period of time to rate
// limit the msg.
func maybeRateLimitMsg(ctx context.Context, rl *rate.Limiter, peerPub [33]byte,
	msg lnwire.Message, quit <-chan struct{}) error {

	delay, err := deriveRateLimitReservation(rl, msg)
	if err != nil {
		return nil
	}

	return waitMsgDelay(ctx, peerPub, delay, quit)
}

// sendMessages sends a set of messages to the remote peer.
func (m *SyncManager) sendMessages(ctx context.Context, sync bool,
	peer lnpeer.Peer, nodeID route.Vertex, msgs ...lnwire.Message) error {

	for _, msg := range msgs {
		err := maybeRateLimitMsg(
			ctx, m.rateLimiter, nodeID, msg, m.quit,
		)
		if err != nil {
			return err
		}

		if err := peer.SendMessageLazy(sync, msg); err != nil {
			return err
		}
	}

	return nil
}

// createGossipSyncer creates the GossipSyncer for a newly connected peer.
func (m *SyncManager) createGossipSyncer(peer lnpeer.Peer) *GossipSyncer {
	nodeID := route.Vertex(peer.PubKey())
	log.Infof("Creating new GossipSyncer for peer=%x", nodeID[:])

	encoding := lnwire.EncodingSortedPlain
	s := newGossipSyncer(gossipSyncerCfg{
		chainHash:     m.cfg.ChainHash,
		peerPub:       nodeID,
		channelSeries: m.cfg.ChanSeries,
		encodingType:  encoding,
		chunkSize:     encodingTypeToChunkSize[encoding],
		batchSize:     requestBatchSize,
		sendMsg: func(ctx context.Context, sync bool,
			msgs ...lnwire.Message) error {

			return m.sendMessages(ctx, sync, peer, nodeID, msgs...)
		},
		ignoreHistoricalFilters:  m.cfg.IgnoreHistoricalFilters,
		bestHeight:               m.cfg.BestHeight,
		markGraphSynced:          m.markGraphSynced,
		maxQueryChanRangeReplies: maxQueryChanRangeReplies,
		noTimestampQueryOption:   m.cfg.NoTimestampQueries,
		isStillZombieChannel:     m.cfg.IsStillZombieChannel,
		msgBytesPerSecond:        m.cfg.PeerMsgBytesPerSecond,
	}, m.gossipFilterSema)

	// Gossip syncers are initialized by default in a PassiveSync type
	// and chansSynced state so that they can reply to any peer queries or
	// handle any sync transitions.
	s.setSyncState(chansSynced)
	s.setSyncType(PassiveSync)

	log.Debugf("Created new GossipSyncer[state=%s type=%s] for peer=%x",
		s.syncState(), s.SyncType(), peer.PubKey())

	return s
}

// removeGossipSyncer removes all internal references to the disconnected peer's
// GossipSyncer and stops it. In the event of an active GossipSyncer being
// disconnected, a passive GossipSyncer, if any, will take its place.
func (m *SyncManager) removeGossipSyncer(peer route.Vertex) {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

	s, ok := m.gossipSyncer(peer)
	if !ok {
		return
	}

	log.Infof("Removing GossipSyncer for peer=%v", peer)

	// We'll stop the GossipSyncer for the disconnected peer in a goroutine
	// to prevent blocking the SyncManager.
	go s.Stop()

	// If it's a non-active syncer, then we can just exit now.
	if _, ok := m.inactiveSyncers[peer]; ok {
		delete(m.inactiveSyncers, peer)
		return
	}

	// If it's a pinned syncer, then we can just exit as this doesn't
	// affect our active syncer count.
	if _, ok := m.pinnedActiveSyncers[peer]; ok {
		delete(m.pinnedActiveSyncers, peer)
		return
	}

	// Otherwise, we'll need find a new one to replace it, if any.
	delete(m.activeSyncers, peer)
	newActiveSyncer := chooseRandomSyncer(
		m.inactiveSyncers, m.transitionPassiveSyncer,
	)
	if newActiveSyncer == nil {
		return
	}

	log.Debugf("Replaced active GossipSyncer(%v) with GossipSyncer(%x)",
		peer, newActiveSyncer.cfg.peerPub)
}

// rotateActiveSyncerCandidate rotates a single active syncer. In order to
// achieve this, the active syncer must be in a chansSynced state in order to
// process the sync transition.
func (m *SyncManager) rotateActiveSyncerCandidate() {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

	// If we couldn't find an eligible active syncer to rotate, we can
	// return early.
	activeSyncer := chooseRandomSyncer(m.activeSyncers, nil)
	if activeSyncer == nil {
		log.Debug("No eligible active syncer to rotate")
		return
	}

	// Similarly, if we don't have a candidate to rotate with, we can return
	// early as well.
	candidate := chooseRandomSyncer(m.inactiveSyncers, nil)
	if candidate == nil {
		log.Debug("No eligible candidate to rotate active syncer")
		return
	}

	// Otherwise, we'll attempt to transition each syncer to their
	// respective new sync type.
	log.Debugf("Rotating active GossipSyncer(%x) with GossipSyncer(%x)",
		activeSyncer.cfg.peerPub, candidate.cfg.peerPub)

	if err := m.transitionActiveSyncer(activeSyncer); err != nil {
		log.Errorf("Unable to transition active GossipSyncer(%x): %v",
			activeSyncer.cfg.peerPub, err)
		return
	}

	if err := m.transitionPassiveSyncer(candidate); err != nil {
		log.Errorf("Unable to transition passive GossipSyncer(%x): %v",
			activeSyncer.cfg.peerPub, err)
		return
	}
}

// transitionActiveSyncer transitions an active syncer to a passive one.
//
// NOTE: This must be called with the syncersMu lock held.
func (m *SyncManager) transitionActiveSyncer(s *GossipSyncer) error {
	log.Debugf("Transitioning active GossipSyncer(%x) to passive",
		s.cfg.peerPub)

	if err := s.ProcessSyncTransition(PassiveSync); err != nil {
		return err
	}

	delete(m.activeSyncers, s.cfg.peerPub)
	m.inactiveSyncers[s.cfg.peerPub] = s

	return nil
}

// transitionPassiveSyncer transitions a passive syncer to an active one.
//
// NOTE: This must be called with the syncersMu lock held.
func (m *SyncManager) transitionPassiveSyncer(s *GossipSyncer) error {
	log.Debugf("Transitioning passive GossipSyncer(%x) to active",
		s.cfg.peerPub)

	if err := s.ProcessSyncTransition(ActiveSync); err != nil {
		return err
	}

	delete(m.inactiveSyncers, s.cfg.peerPub)
	m.activeSyncers[s.cfg.peerPub] = s

	return nil
}

// forceHistoricalSync chooses a syncer with a remote peer at random and forces
// a historical sync with it.
func (m *SyncManager) forceHistoricalSync() *GossipSyncer {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

	// We'll sample from both sets of active and inactive syncers in the
	// event that we don't have any inactive syncers.
	return chooseRandomSyncer(m.gossipSyncers(), func(s *GossipSyncer) error {
		return s.historicalSync()
	})
}

// chooseRandomSyncer iterates through the set of syncers given and returns the
// first one which was able to successfully perform the action enclosed in the
// function closure.
//
// NOTE: It's possible for a nil value to be returned if there are no eligible
// candidate syncers.
func chooseRandomSyncer(syncers map[route.Vertex]*GossipSyncer,
	action func(*GossipSyncer) error) *GossipSyncer {

	for _, s := range syncers {
		// Only syncers in a chansSynced state are viable for sync
		// transitions, so skip any that aren't.
		if s.syncState() != chansSynced {
			continue
		}

		if action != nil {
			if err := action(s); err != nil {
				log.Debugf("Skipping eligible candidate "+
					"GossipSyncer(%x): %v", s.cfg.peerPub,
					err)
				continue
			}
		}

		return s
	}

	return nil
}

// InitSyncState is called by outside sub-systems when a connection is
// established to a new peer that understands how to perform channel range
// queries. We'll allocate a new GossipSyncer for it, and start any goroutines
// needed to handle new queries. The first GossipSyncer registered with the
// SyncManager will attempt a historical sync to ensure we have as much of the
// public channel graph as possible.
//
// TODO(wilmer): Only mark as ActiveSync if this isn't a channel peer.
func (m *SyncManager) InitSyncState(peer lnpeer.Peer) error {
	done := make(chan struct{})

	select {
	case m.newSyncers <- &newSyncer{
		peer:     peer,
		doneChan: done,
	}:
	case <-m.quit:
		return ErrSyncManagerExiting
	}

	select {
	case <-done:
		return nil
	case <-m.quit:
		return ErrSyncManagerExiting
	}
}

// PruneSyncState is called by outside sub-systems once a peer that we were
// previously connected to has been disconnected. In this case we can stop the
// existing GossipSyncer assigned to the peer and free up resources.
func (m *SyncManager) PruneSyncState(peer route.Vertex) {
	done := make(chan struct{})

	// We avoid returning an error when the SyncManager is stopped since the
	// GossipSyncer will be stopped then anyway.
	select {
	case m.staleSyncers <- &staleSyncer{
		peer:     peer,
		doneChan: done,
	}:
	case <-m.quit:
		return
	}

	select {
	case <-done:
	case <-m.quit:
	}
}

// GossipSyncer returns the associated gossip syncer of a peer. The boolean
// returned signals whether there exists a gossip syncer for the peer.
func (m *SyncManager) GossipSyncer(peer route.Vertex) (*GossipSyncer, bool) {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()
	return m.gossipSyncer(peer)
}

// gossipSyncer returns the associated gossip syncer of a peer. The boolean
// returned signals whether there exists a gossip syncer for the peer.
func (m *SyncManager) gossipSyncer(peer route.Vertex) (*GossipSyncer, bool) {
	syncer, ok := m.inactiveSyncers[peer]
	if ok {
		return syncer, true
	}
	syncer, ok = m.activeSyncers[peer]
	if ok {
		return syncer, true
	}
	syncer, ok = m.pinnedActiveSyncers[peer]
	if ok {
		return syncer, true
	}
	return nil, false
}

// GossipSyncers returns all of the currently initialized gossip syncers.
func (m *SyncManager) GossipSyncers() map[route.Vertex]*GossipSyncer {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()
	return m.gossipSyncers()
}

// gossipSyncers returns all of the currently initialized gossip syncers.
func (m *SyncManager) gossipSyncers() map[route.Vertex]*GossipSyncer {
	numSyncers := len(m.inactiveSyncers) + len(m.activeSyncers)
	syncers := make(map[route.Vertex]*GossipSyncer, numSyncers)

	for _, syncer := range m.inactiveSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}
	for _, syncer := range m.activeSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}

	return syncers
}

// markGraphSynced allows us to report that the initial historical sync has
// completed.
func (m *SyncManager) markGraphSynced() {
	atomic.StoreInt32(&m.initialHistoricalSyncCompleted, 1)
}

// IsGraphSynced determines whether we've completed our initial historical sync.
// The initial historical sync is done to ensure we've ingested as much of the
// public graph as possible.
func (m *SyncManager) IsGraphSynced() bool {
	return atomic.LoadInt32(&m.initialHistoricalSyncCompleted) == 1
}
