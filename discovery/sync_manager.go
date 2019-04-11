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

	// activeSyncers is the set of all syncers for which we are currently
	// receiving graph updates from. The number of possible active syncers
	// is bounded by NumActiveSyncers.
	activeSyncers map[routing.Vertex]*GossipSyncer

	// inactiveSyncers is the set of all syncers for which we are not
	// currently receiving new graph updates from.
	inactiveSyncers map[routing.Vertex]*GossipSyncer

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

		m.Lock()
		defer m.Unlock()

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

	log.Debugf("Rotating active GossipSyncer(%x) with GossipSyncer(%x)",
		activeSyncer.cfg.peerPub, candidate.cfg.peerPub)

	// Otherwise, we'll attempt to transition each syncer to their
	// respective new sync type. We'll avoid performing the transition with
	// the lock as it can potentially stall the SyncManager due to the
	// syncTransitionTimeout.
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
func (m *SyncManager) transitionActiveSyncer(s *GossipSyncer) error {
	log.Debugf("Transitioning active GossipSyncer(%x) to passive",
		s.cfg.peerPub)

	if err := s.ProcessSyncTransition(PassiveSync); err != nil {
		return err
	}

	m.Lock()
	delete(m.activeSyncers, s.cfg.peerPub)
	m.inactiveSyncers[s.cfg.peerPub] = s
	m.Unlock()

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
	delete(m.inactiveSyncers, s.cfg.peerPub)
	m.activeSyncers[s.cfg.peerPub] = s
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
	m.Lock()
	defer m.Unlock()

	// If we already have a syncer, then we'll exit early as we don't want
	// to override it.
	nodeID := routing.Vertex(peer.PubKey())
	if _, ok := m.gossipSyncer(nodeID); ok {
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
		batchSize:     requestBatchSize,
		sendToPeer: func(msgs ...lnwire.Message) error {
			return peer.SendMessageLazy(false, msgs...)
		},
	})

	// If we've yet to reach our desired number of active syncers, then
	// we'll use this one.
	if len(m.activeSyncers) < m.cfg.NumActiveSyncers {
		s.setSyncType(ActiveSync)
		m.activeSyncers[s.cfg.peerPub] = s
	} else {
		s.setSyncType(PassiveSync)
		m.inactiveSyncers[s.cfg.peerPub] = s
	}

	// Gossip syncers are initialized by default in a chansSynced state so
	// that they can reply to any peer queries or
	// handle any sync transitions.
	s.setSyncState(chansSynced)
	s.Start()

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

	// Otherwise, we'll need to dequeue it from our pending active syncers
	// queue and find a new one to replace it, if any.
	delete(m.activeSyncers, s.cfg.peerPub)
	newActiveSyncer := m.chooseRandomSyncer(nil, false)
	m.Unlock()
	if newActiveSyncer == nil {
		return
	}

	if err := m.transitionPassiveSyncer(newActiveSyncer); err != nil {
		log.Errorf("Unable to transition passive GossipSyncer(%x): %v",
			newActiveSyncer.cfg.peerPub, err)
		return
	}

	log.Debugf("Replaced active GossipSyncer(%v) with GossipSyncer(%x)",
		peer, newActiveSyncer.cfg.peerPub)
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
