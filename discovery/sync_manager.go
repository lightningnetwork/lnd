package discovery

import (
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
	DefaultHistoricalSyncInterval = time.Hour
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
	peer routing.Vertex

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

	// RotateTicker is a ticker responsible for notifying the SyncManager
	// when it should rotate its active syncers. A single active syncer with
	// a chansSynced state will be exchanged for a passive syncer in order
	// to ensure we don't keep syncing with the same peers.
	RotateTicker ticker.Ticker

	// HistoricalSyncTicker is a ticker responsible for notifying the
	// SyncManager when it should attempt a historical sync with a gossip
	// sync peer.
	HistoricalSyncTicker ticker.Ticker
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
	activeSyncers map[routing.Vertex]*GossipSyncer

	// inactiveSyncers is the set of all syncers for which we are not
	// currently receiving new graph updates from.
	inactiveSyncers map[routing.Vertex]*GossipSyncer

	wg   sync.WaitGroup
	quit chan struct{}
}

// newSyncManager constructs a new SyncManager backed by the given config.
func newSyncManager(cfg *SyncManagerCfg) *SyncManager {
	return &SyncManager{
		cfg:          *cfg,
		newSyncers:   make(chan *newSyncer),
		staleSyncers: make(chan *staleSyncer),
		activeSyncers: make(
			map[routing.Vertex]*GossipSyncer, cfg.NumActiveSyncers,
		),
		inactiveSyncers: make(map[routing.Vertex]*GossipSyncer),
		quit:            make(chan struct{}),
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

// 3. Finding new peers to force a historical sync with to ensure we have as
//    much of the public network as possible.
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

			m.syncersMu.Lock()
			switch {
			// If we've exceeded our total number of active syncers,
			// we'll initialize this GossipSyncer as passive.
			case len(m.activeSyncers) >= m.cfg.NumActiveSyncers:
				s.setSyncType(PassiveSync)
				m.inactiveSyncers[s.cfg.peerPub] = s

			// Otherwise, it should be initialized as active.
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
			var err error
			m.historicalSync.Do(func() {
				log.Infof("Attempting historical sync with "+
					"GossipSyncer(%x)", s.cfg.peerPub)
				err = s.historicalSync()
			})
			if err != nil {
				log.Errorf("Unable to perform historical sync "+
					"with GossipSyncer(%x): %v",
					s.cfg.peerPub, err)

				// Reset historicalSync to ensure it is tried
				// again with a different peer.
				m.historicalSync = sync.Once{}
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

// createGossipSyncer creates the GossipSyncer for a newly connected peer.
func (m *SyncManager) createGossipSyncer(peer lnpeer.Peer) *GossipSyncer {
	nodeID := routing.Vertex(peer.PubKey())
	log.Infof("Creating new GossipSyncer for peer=%x", nodeID[:])

	encoding := lnwire.EncodingSortedPlain
	s := newGossipSyncer(gossipSyncerCfg{
		chainHash:     m.cfg.ChainHash,
		peerPub:       nodeID,
		channelSeries: m.cfg.ChanSeries,
		encodingType:  encoding,
		chunkSize:     encodingTypeToChunkSize[encoding],
		batchSize:     requestBatchSize,
		sendToPeer: func(msgs ...lnwire.Message) error {
			return peer.SendMessageLazy(false, msgs...)
		},
	})

	// Gossip syncers are initialized by default in a PassiveSync type
	// and chansSynced state so that they can reply to any peer queries or
	// handle any sync transitions.
	s.setSyncState(chansSynced)
	s.setSyncType(PassiveSync)
	return s
}

// removeGossipSyncer removes all internal references to the disconnected peer's
// GossipSyncer and stops it. In the event of an active GossipSyncer being
// disconnected, a passive GossipSyncer, if any, will take its place.
func (m *SyncManager) removeGossipSyncer(peer routing.Vertex) {
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

	// Otherwise, we'll need find a new one to replace it, if any.
	delete(m.activeSyncers, peer)
	newActiveSyncer := m.chooseRandomSyncer(nil, false)
	if newActiveSyncer == nil {
		return
	}

	if err := m.transitionPassiveSyncer(newActiveSyncer); err != nil {
		log.Errorf("Unable to transition passive GossipSyncer(%x): %v",
			newActiveSyncer.cfg.peerPub, err)
		return
	}

	log.Debugf("Replaced active GossipSyncer(%x) with GossipSyncer(%x)",
		peer, newActiveSyncer.cfg.peerPub)
}

// rotateActiveSyncerCandidate rotates a single active syncer. In order to
// achieve this, the active syncer must be in a chansSynced state in order to
// process the sync transition.
func (m *SyncManager) rotateActiveSyncerCandidate() {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

	// If we don't have a candidate to rotate with, we can return early.
	candidate := m.chooseRandomSyncer(nil, false)
	if candidate == nil {
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

	// If we couldn't find an eligible one, we can return early.
	if activeSyncer == nil {
		log.Debug("No eligible active syncer to rotate")
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
func (m *SyncManager) forceHistoricalSync() {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

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
func (m *SyncManager) PruneSyncState(peer routing.Vertex) {
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
func (m *SyncManager) GossipSyncer(peer routing.Vertex) (*GossipSyncer, bool) {
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()
	return m.gossipSyncer(peer)
}

// gossipSyncer returns the associated gossip syncer of a peer. The boolean
// returned signals whether there exists a gossip syncer for the peer.
func (m *SyncManager) gossipSyncer(peer routing.Vertex) (*GossipSyncer, bool) {
	syncer, ok := m.inactiveSyncers[peer]
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
	m.syncersMu.Lock()
	defer m.syncersMu.Unlock()

	numSyncers := len(m.inactiveSyncers) + len(m.activeSyncers)
	syncers := make(map[routing.Vertex]*GossipSyncer, numSyncers)

	for _, syncer := range m.inactiveSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}
	for _, syncer := range m.activeSyncers {
		syncers[syncer.cfg.peerPub] = syncer
	}

	return syncers
}
