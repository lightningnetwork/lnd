package discovery

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// DefaultSyncerRotationInterval is the default interval in which we'll
	// rotate a single active syncer.
	DefaultSyncerRotationInterval = 20 * time.Minute

	// DefaultHistoricalSyncInterval is the default interval in which we'll
	// force a historical sync to ensure we have as much of the public
	// network as possible.
	DefaultHistoricalSyncInterval = 20 * time.Minute

	// DefaultActiveSyncerTimeout is the default timeout interval in which
	// we'll wait until an active syncer has completed its state machine and
	// reached its final chansSynced state.
	DefaultActiveSyncerTimeout = 5 * time.Minute
)

var (
	// ErrSyncManagerExiting is an error returned when we attempt to
	// start/stop a gossip syncer for a connected/disconnected peer, but the
	// SyncManager has already been stopped.
	ErrSyncManagerExiting = errors.New("sync manager exiting")
)

// staleActiveSyncer is an internal message the SyncManager will use in order to
// handle a peer corresponding to an active syncer being disconnected.
type staleActiveSyncer struct {
	// syncer is the active syncer to be removed.
	syncer *GossipSyncer

	// transitioned, if true, signals that the active GossipSyncer is stale
	// due to being transitioned to a PassiveSync state.
	transitioned bool

	// done serves as a signal to the caller that the SyncManager's internal
	// state correctly reflects the stale active syncer. This is needed to
	// ensure we always create a new syncer for a flappy peer after they
	// disconnect if they happened to be an active syncer.
	done chan struct{}
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

	// RotateTicker is a ticker responsible for notifying the SyncManager
	// when it should rotate its active syncers. A single active syncer with
	// a chansSynced state will be exchanged for a passive syncer in order
	// to ensure we don't keep syncing with the same peers.
	RotateTicker ticker.Ticker

	// HistoricalSyncTicker is a ticker responsible for notifying the
	// SyncManager when it should attempt a historical sync with a gossip
	// sync peer.
	HistoricalSyncTicker ticker.Ticker

	// ActiveSyncerTimeoutTicker is a ticker responsible for notifying the
	// SyncManager when it should attempt to start the next pending
	// activeSyncer due to the current one not completing its state machine
	// within the timeout.
	ActiveSyncerTimeoutTicker ticker.Ticker
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
	start sync.Once
	stop  sync.Once

	cfg SyncManagerCfg

	// historicalSync allows us to perform an initial historical sync only
	// _once_ with a peer during the SyncManager's startup.
	historicalSync sync.Once

	// activeSyncers is the set of all syncers for which we are currently
	// receiving graph updates from. The number of possible active syncers
	// is bounded by NumActiveSyncers.
	activeSyncers map[routing.Vertex]*GossipSyncer

	// inactiveSyncers is the set of all syncers for which we are not
	// currently receiving new graph updates from.
	inactiveSyncers map[routing.Vertex]*GossipSyncer

	// pendingActiveSyncers is a map that tracks our set of pending active
	// syncers. This map will be queried when choosing the next pending
	// active syncer in the queue to ensure it is not stale.
	pendingActiveSyncers map[routing.Vertex]*GossipSyncer

	// pendingActiveSyncerQueue is the list of active syncers which are
	// pending to be started. Syncers will be added to this list through the
	// newActiveSyncers and staleActiveSyncers channels.
	pendingActiveSyncerQueue *list.List

	// newActiveSyncers is a channel that will serve as a signal to the
	// roundRobinHandler to allow it to transition the next pending active
	// syncer in the queue.
	newActiveSyncers chan struct{}

	// staleActiveSyncers is a channel through which we'll send any stale
	// active syncers that should be removed from the round-robin.
	staleActiveSyncers chan *staleActiveSyncer

	sync.Mutex
	wg   sync.WaitGroup
	quit chan struct{}
}

// newSyncManager constructs a new SyncManager backed by the given config.
func newSyncManager(cfg *SyncManagerCfg) *SyncManager {
	return &SyncManager{
		cfg: *cfg,
		activeSyncers: make(
			map[routing.Vertex]*GossipSyncer, cfg.NumActiveSyncers,
		),
		inactiveSyncers:          make(map[routing.Vertex]*GossipSyncer),
		pendingActiveSyncers:     make(map[routing.Vertex]*GossipSyncer),
		pendingActiveSyncerQueue: list.New(),
		newActiveSyncers:         make(chan struct{}),
		staleActiveSyncers:       make(chan *staleActiveSyncer),
		quit:                     make(chan struct{}),
	}
}

// Start starts the SyncManager in order to properly carry out its duties.
func (m *SyncManager) Start() {
	m.start.Do(func() {
		m.wg.Add(2)
		go m.syncerHandler()
		go m.roundRobinHandler()
	})
}

// Stop stops the SyncManager from performing its duties.
func (m *SyncManager) Stop() {
	m.stop.Do(func() {
		close(m.quit)
		m.wg.Wait()

		m.Lock()
		defer m.Unlock()

		for _, syncer := range m.inactiveSyncers {
			syncer.Stop()
		}
		for _, syncer := range m.pendingActiveSyncers {
			syncer.Stop()
		}
		for _, syncer := range m.activeSyncers {
			syncer.Stop()
		}
	})
}

// syncerHandler is the SyncManager's main event loop responsible for:
//
//   1. Finding new peers to receive graph updates from to ensure we don't only
//   receive them from the same set of peers.
//
//   2. Finding new peers to force a historical sync with to ensure we have as
//   much of the public network as possible.
//
// NOTE: This must be run as a goroutine.
func (m *SyncManager) syncerHandler() {
	defer m.wg.Done()

	m.cfg.RotateTicker.Resume()
	defer m.cfg.RotateTicker.Stop()

	m.cfg.HistoricalSyncTicker.Resume()
	defer m.cfg.HistoricalSyncTicker.Stop()

	for {
		select {
		// Our RotateTicker has ticked, so we'll attempt to rotate a
		// single active syncer with a passive one.
		case <-m.cfg.RotateTicker.Ticks():
			m.rotateActiveSyncerCandidate()

		// Our HistoricalSyncTicker has ticked, so we'll randomly select
		// a peer and force a historical sync with them.
		case <-m.cfg.HistoricalSyncTicker.Ticks():
			m.forceHistoricalSync()

		case <-m.quit:
			return
		}
	}
}

// signalNewActiveSyncer sends a signal to the roundRobinHandler to ensure it
// transitions any pending active syncers.
func (m *SyncManager) signalNewActiveSyncer() {
	select {
	case m.newActiveSyncers <- struct{}{}:
	case <-m.quit:
	}
}

// signalStaleActiveSyncer removes the syncer for the given peer from the
// round-robin queue.
func (m *SyncManager) signalStaleActiveSyncer(s *GossipSyncer, transitioned bool) {
	done := make(chan struct{})

	select {
	case m.staleActiveSyncers <- &staleActiveSyncer{
		syncer:       s,
		transitioned: transitioned,
		done:         done,
	}:
	case <-m.quit:
	}

	// Before returning to the caller, we'll wait for the roundRobinHandler
	// to signal us that the SyncManager has correctly updated its internal
	// state after handling the stale active syncer.
	select {
	case <-done:
	case <-m.quit:
	}
}

// roundRobinHandler is the SyncManager's event loop responsible for managing
// the round-robin queue of our active syncers to ensure they don't overlap and
// request the same set of channels, which significantly reduces bandwidth
// usage.
//
// NOTE: This must be run as a goroutine.
func (m *SyncManager) roundRobinHandler() {
	defer m.wg.Done()

	defer m.cfg.ActiveSyncerTimeoutTicker.Stop()

	var (
		// current will hold the current active syncer we're waiting for
		// to complete its state machine.
		current *GossipSyncer

		// transitionNext will be responsible for containing the signal
		// of when the current active syncer has completed its state
		// machine. This signal allows us to transition the next pending
		// active syncer, if any.
		transitionNext chan struct{}
	)

	// transitionNextSyncer is a helper closure that we'll use to transition
	// the next syncer queued up. If there aren't any, this will act as a
	// NOP.
	transitionNextSyncer := func() {
		m.Lock()
		current = m.nextPendingActiveSyncer()
		m.Unlock()
		for current != nil {
			// Ensure we properly handle a shutdown signal.
			select {
			case <-m.quit:
				return
			default:
			}

			// We'll avoid performing the transition with the lock
			// as it can potentially stall the SyncManager due to
			// the syncTransitionTimeout.
			err := m.transitionPassiveSyncer(current)
			// If we timed out attempting to transition the syncer,
			// we'll re-queue it to retry at a later time and move
			// on to the next.
			if err == ErrSyncTransitionTimeout {
				log.Debugf("Timed out attempting to "+
					"transition pending active "+
					"GossipSyncer(%x)", current.cfg.peerPub)

				m.Lock()
				m.queueActiveSyncer(current)
				current = m.nextPendingActiveSyncer()
				m.Unlock()
				continue
			}
			if err != nil {
				log.Errorf("Unable to transition pending "+
					"active GossipSyncer(%x): %v",
					current.cfg.peerPub, err)

				m.Lock()
				current = m.nextPendingActiveSyncer()
				m.Unlock()
				continue
			}

			// The transition succeeded, so we'll set our signal to
			// know when we should attempt to transition the next
			// pending active syncer in our queue.
			transitionNext = current.ResetSyncedSignal()
			m.cfg.ActiveSyncerTimeoutTicker.Resume()
			return
		}

		transitionNext = nil
		m.cfg.ActiveSyncerTimeoutTicker.Pause()
	}

	for {
		select {
		// A new active syncer signal has been received, which indicates
		// a new pending active syncer has been added to our queue.
		// We'll only attempt to transition it now if we're not already
		// in the middle of transitioning another one. We do this to
		// ensure we don't overlap when requesting channels from
		// different peers.
		case <-m.newActiveSyncers:
			if current == nil {
				transitionNextSyncer()
			}

		// A stale active syncer has been received, so we'll need to
		// remove them from our queue. If we are currently waiting for
		// its state machine to complete, we'll move on to the next
		// active syncer in the queue.
		case staleActiveSyncer := <-m.staleActiveSyncers:
			s := staleActiveSyncer.syncer

			m.Lock()
			// If the syncer has transitioned from an ActiveSync
			// type, rather than disconnecting, we'll include it in
			// the set of inactive syncers.
			if staleActiveSyncer.transitioned {
				m.inactiveSyncers[s.cfg.peerPub] = s
			} else {
				// Otherwise, since the peer is disconnecting,
				// we'll attempt to find a passive syncer that
				// can replace it.
				newActiveSyncer := m.chooseRandomSyncer(nil, false)
				if newActiveSyncer != nil {
					m.queueActiveSyncer(newActiveSyncer)
				}
			}

			// Remove the internal active syncer references for this
			// peer.
			delete(m.pendingActiveSyncers, s.cfg.peerPub)
			delete(m.activeSyncers, s.cfg.peerPub)
			m.Unlock()

			// Signal to the caller that they can now proceed since
			// the SyncManager's state correctly reflects the
			// stale active syncer.
			close(staleActiveSyncer.done)

			// If we're not currently waiting for an active syncer
			// to reach its terminal state, or if we are but we are
			// currently waiting for the peer being
			// disconnected/transitioned, then we'll move on to the
			// next active syncer in our queue.
			if current == nil || (current != nil &&
				current.cfg.peerPub == s.cfg.peerPub) {
				transitionNextSyncer()
			}

		// Our current active syncer has reached its terminal
		// chansSynced state, so we'll proceed to transitioning the next
		// pending active syncer if there is one.
		case <-transitionNext:
			transitionNextSyncer()

		// We've timed out waiting for the current active syncer to
		// reach its terminal chansSynced state, so we'll just
		// move on to the next and avoid retrying as its already been
		// transitioned.
		case <-m.cfg.ActiveSyncerTimeoutTicker.Ticks():
			log.Warnf("Timed out waiting for GossipSyncer(%x) to "+
				"be fully synced", current.cfg.peerPub)
			transitionNextSyncer()

		case <-m.quit:
			return
		}
	}
}

// queueActiveSyncer queues the given pending active gossip syncer to the end of
// the round-robin queue.
func (m *SyncManager) queueActiveSyncer(s *GossipSyncer) {
	log.Debugf("Queueing next pending active GossipSyncer(%x)",
		s.cfg.peerPub)

	delete(m.inactiveSyncers, s.cfg.peerPub)
	m.pendingActiveSyncers[s.cfg.peerPub] = s
	m.pendingActiveSyncerQueue.PushBack(s)
}

// nextPendingActiveSyncer returns the next active syncer pending to be
// transitioned. If there aren't any, then `nil` is returned.
func (m *SyncManager) nextPendingActiveSyncer() *GossipSyncer {
	next := m.pendingActiveSyncerQueue.Front()
	for next != nil {
		s := m.pendingActiveSyncerQueue.Remove(next).(*GossipSyncer)

		// If the next pending active syncer is no longer in our lookup
		// map, then the corresponding peer has disconnected, so we'll
		// skip them.
		if _, ok := m.pendingActiveSyncers[s.cfg.peerPub]; !ok {
			next = m.pendingActiveSyncerQueue.Front()
			continue
		}

		return s
	}

	return nil
}

// rotateActiveSyncerCandidate rotates a single active syncer. In order to
// achieve this, the active syncer must be in a chansSynced state in order to
// process the sync transition.
func (m *SyncManager) rotateActiveSyncerCandidate() {
	// If we don't have a candidate to rotate with, we can return early.
	m.Lock()
	candidate := m.chooseRandomSyncer(nil, false)
	if candidate == nil {
		m.Unlock()
		log.Debug("No eligible candidate to rotate active syncer")
		return
	}

	// We'll choose an active syncer at random that's within a chansSynced
	// state to rotate.
	var activeSyncer *GossipSyncer
	for _, s := range m.activeSyncers {
		// The active syncer must be in a chansSynced state in order to
		// process sync transitions.
		if s.syncState() != chansSynced {
			continue
		}

		activeSyncer = s
		break
	}
	m.Unlock()

	// If we couldn't find an eligible one, we can return early.
	if activeSyncer == nil {
		log.Debug("No eligible active syncer to rotate")
		return
	}

	// Otherwise, we'll attempt to transition each syncer to their
	// respective new sync type. We'll avoid performing the transition with
	// the lock as it can potentially stall the SyncManager due to the
	// syncTransitionTimeout.
	if err := m.transitionActiveSyncer(activeSyncer); err != nil {
		log.Errorf("Unable to transition active "+
			"GossipSyncer(%x): %v", activeSyncer.cfg.peerPub, err)
		return
	}

	m.Lock()
	m.queueActiveSyncer(candidate)
	m.Unlock()

	m.signalNewActiveSyncer()
}

// transitionActiveSyncer transitions an active syncer to a passive one.
func (m *SyncManager) transitionActiveSyncer(s *GossipSyncer) error {
	log.Debugf("Transitioning active GossipSyncer(%x) to passive",
		s.cfg.peerPub)

	if err := s.ProcessSyncTransition(PassiveSync); err != nil {
		return err
	}

	m.signalStaleActiveSyncer(s, true)

	return nil
}

// transitionPassiveSyncer transitions a passive syncer to an active one.
func (m *SyncManager) transitionPassiveSyncer(s *GossipSyncer) error {
	log.Debugf("Transitioning passive GossipSyncer(%x) to active",
		s.cfg.peerPub)

	if err := s.ProcessSyncTransition(ActiveSync); err != nil {
		return err
	}

	m.Lock()
	m.activeSyncers[s.cfg.peerPub] = s
	delete(m.pendingActiveSyncers, s.cfg.peerPub)
	m.Unlock()

	return nil
}

// forceHistoricalSync chooses a syncer with a remote peer at random and forces
// a historical sync with it.
func (m *SyncManager) forceHistoricalSync() {
	m.Lock()
	defer m.Unlock()

	// We'll choose a random peer with whom we can perform a historical sync
	// with. We'll set useActive to true to make sure we can still do one if
	// we don't happen to have any non-active syncers.
	candidatesChosen := make(map[routing.Vertex]struct{})
	s := m.chooseRandomSyncer(candidatesChosen, true)
	for s != nil {
		// Ensure we properly handle a shutdown signal.
		select {
		case <-m.quit:
			return
		default:
		}

		// Blacklist the candidate to ensure it's not chosen again.
		candidatesChosen[s.cfg.peerPub] = struct{}{}

		err := s.historicalSync()
		if err == nil {
			return
		}

		log.Errorf("Unable to perform historical sync with "+
			"GossipSyncer(%x): %v", s.cfg.peerPub, err)

		s = m.chooseRandomSyncer(candidatesChosen, true)
	}
}

// chooseRandomSyncer returns a random non-active syncer that's eligible for a
// sync transition. A blacklist can be used to skip any previously chosen
// candidates. The useActive boolean can be used to also filter active syncers.
//
// NOTE: It's possible for a nil value to be returned if there are no eligible
// candidate syncers.
//
// NOTE: This method must be called with the syncersMtx lock held.
func (m *SyncManager) chooseRandomSyncer(blacklist map[routing.Vertex]struct{},
	useActive bool) *GossipSyncer {

	eligible := func(s *GossipSyncer) bool {
		// Skip any syncers that exist within the blacklist.
		if blacklist != nil {
			if _, ok := blacklist[s.cfg.peerPub]; ok {
				return false
			}
		}

		// Only syncers in a chansSynced state are viable for sync
		// transitions, so skip any that aren't.
		return s.syncState() == chansSynced
	}

	for _, s := range m.inactiveSyncers {
		if !eligible(s) {
			continue
		}
		return s
	}

	if useActive {
		for _, s := range m.activeSyncers {
			if !eligible(s) {
				continue
			}
			return s
		}
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
func (m *SyncManager) InitSyncState(peer lnpeer.Peer) {
	// If we already have a syncer, then we'll exit early as we don't want
	// to override it.
	nodeID := routing.Vertex(peer.PubKey())
	if _, ok := m.GossipSyncer(nodeID); ok {
		return
	}

	log.Infof("Creating new GossipSyncer for peer=%x", nodeID[:])

	encoding := lnwire.EncodingSortedPlain
	s := newGossipSyncer(gossipSyncerCfg{
		chainHash:     m.cfg.ChainHash,
		peerPub:       nodeID,
		channelSeries: m.cfg.ChanSeries,
		encodingType:  encoding,
		chunkSize:     encodingTypeToChunkSize[encoding],
		sendToPeer: func(msgs ...lnwire.Message) error {
			return peer.SendMessage(false, msgs...)
		},
	})

	// Gossip syncers are initialized by default as passive and in a
	// chansSynced state so that they can reply to any peer queries or
	// handle any sync transitions.
	s.setSyncType(PassiveSync)
	s.setSyncState(chansSynced)
	s.Start()

	m.Lock()
	m.inactiveSyncers[nodeID] = s

	// We'll force a historical sync with the first peer we connect to
	// ensure we get as much of the graph as possible.
	var err error
	m.historicalSync.Do(func() {
		log.Infof("Attempting historical sync with GossipSyncer(%x)",
			s.cfg.peerPub)

		err = s.historicalSync()
	})
	if err != nil {
		log.Errorf("Unable to perform historical sync with "+
			"GossipSyncer(%x): %v", s.cfg.peerPub, err)

		// Reset historicalSync to ensure it is tried again with a
		// different peer.
		m.historicalSync = sync.Once{}
	}

	// If we've yet to reach our desired number of active syncers, then
	// we'll use this one.
	numActiveSyncers := len(m.activeSyncers) + len(m.pendingActiveSyncers)
	if numActiveSyncers < m.cfg.NumActiveSyncers {
		m.queueActiveSyncer(s)
		m.Unlock()
		m.signalNewActiveSyncer()
		return
	}
	m.Unlock()
}

// PruneSyncState is called by outside sub-systems once a peer that we were
// previously connected to has been disconnected. In this case we can stop the
// existing GossipSyncer assigned to the peer and free up resources.
func (m *SyncManager) PruneSyncState(peer routing.Vertex) {
	s, ok := m.GossipSyncer(peer)
	if !ok {
		return
	}

	log.Infof("Removing GossipSyncer for peer=%v", peer)

	// We'll start by stopping the GossipSyncer for the disconnected peer.
	s.Stop()

	// If it's a non-active syncer, then we can just exit now.
	m.Lock()
	if _, ok := m.inactiveSyncers[s.cfg.peerPub]; ok {
		delete(m.inactiveSyncers, s.cfg.peerPub)
		m.Unlock()
		return
	}
	m.Unlock()

	// Otherwise, we'll need to dequeue it from our pending active syncers
	// queue and find a new one to replace it, if any.
	m.signalStaleActiveSyncer(s, false)
}

// GossipSyncer returns the associated gossip syncer of a peer. The boolean
// returned signals whether there exists a gossip syncer for the peer.
func (m *SyncManager) GossipSyncer(peer routing.Vertex) (*GossipSyncer, bool) {
	m.Lock()
	defer m.Unlock()
	return m.gossipSyncer(peer)
}

// gossipSyncer returns the associated gossip syncer of a peer. The boolean
// returned signals whether there exists a gossip syncer for the peer.
func (m *SyncManager) gossipSyncer(peer routing.Vertex) (*GossipSyncer, bool) {
	syncer, ok := m.inactiveSyncers[peer]
	if ok {
		return syncer, true
	}
	syncer, ok = m.pendingActiveSyncers[peer]
	if ok {
		return syncer, true
	}
	syncer, ok = m.activeSyncers[peer]
	if ok {
		return syncer, true
	}
	return nil, false
}

// GossipSyncers returns all of the currently initialized gossip syncers.
func (m *SyncManager) GossipSyncers() map[routing.Vertex]*GossipSyncer {
	m.Lock()
	defer m.Unlock()

	numSyncers := len(m.inactiveSyncers) + len(m.activeSyncers) +
		len(m.inactiveSyncers)
	syncers := make(map[routing.Vertex]*GossipSyncer, numSyncers)

	for _, syncer := range m.inactiveSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}
	for _, syncer := range m.pendingActiveSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}
	for _, syncer := range m.activeSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}

	return syncers
}
