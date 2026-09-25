package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/lightningnetwork/lnd/discovery/gossipsync"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// syncManagerV2 adapts the actor based gossipsync.Manager to the gossiper.
type syncManagerV2 struct {
	mgr *gossipsync.Manager

	// system is the actor system the syncer runs in.
	system *actor.ActorSystem

	// timeoutID is the ID of the timeout actor this adapter registered.
	timeoutID string
}

// A compile-time check that syncManagerV2 provides a syncManager.
var _ syncManager = (*syncManagerV2)(nil)

// syncManagerV2Cfg configures the actor based sync manager.
type syncManagerV2Cfg struct {
	chainHash               chainhash.Hash
	chanSeries              ChannelGraphTimeSeries
	bestHeight              func() uint32
	numActiveSyncers        int
	pinnedSyncers           PinnedSyncers
	rotateInterval          time.Duration
	historicalSyncInterval  time.Duration
	noTimestampQueries      bool
	ignoreHistoricalFilters bool
	isStillZombieChannel    func(graphdb.ChannelUpdateInfo) bool
	msgRateBytes            uint64
	msgBurstBytes           uint64
	peerMsgRateBytes        uint64
	filterConcurrency       int
	system                  *actor.ActorSystem
}

// newSyncManagerV2 creates the actor based sync manager, and a timeout actor
// for its timers, in the given actor system.
func newSyncManagerV2(cfg syncManagerV2Cfg) (*syncManagerV2, error) {
	system := cfg.system
	if system == nil {
		system = actor.NewActorSystem()
	}

	const timeoutID = "gossipsync-timeout"
	timeouts := timeout.NewActorWithConfig(timeout.Config{})
	timeoutRef, err := actor.RegisterWithSystem(
		system, timeoutID,
		actor.NewServiceKey[timeout.Msg, timeout.Resp](timeoutID),
		timeouts,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to start timeout actor: %w", err)
	}
	timeouts.Start(timeoutRef)

	filterConcurrency := cfg.filterConcurrency
	if filterConcurrency == 0 {
		filterConcurrency = DefaultFilterConcurrency
	}

	mgr, err := gossipsync.NewManager(gossipsync.Config{
		ChainHash:               cfg.chainHash,
		Graph:                   cfg.chanSeries,
		IsStillZombie:           cfg.isStillZombieChannel,
		BestHeight:              cfg.bestHeight,
		NumActiveSyncers:        cfg.numActiveSyncers,
		PinnedSyncers:           cfg.pinnedSyncers,
		RotateInterval:          cfg.rotateInterval,
		HistoricalSyncInterval:  cfg.historicalSyncInterval,
		NoTimestampQueries:      cfg.noTimestampQueries,
		IgnoreHistoricalFilters: cfg.ignoreHistoricalFilters,
		PeerMsgBytesPerSecond:   cfg.peerMsgRateBytes,
		MsgBytesPerSecond:       cfg.msgRateBytes,
		MsgBurstBytes:           cfg.msgBurstBytes,
		FilterConcurrency:       filterConcurrency,
		System:                  system,
		Timeouts:                timeoutRef,
	})
	if err != nil {
		system.StopAndRemoveActor(timeoutID)

		return nil, err
	}

	return &syncManagerV2{
		mgr:       mgr,
		system:    system,
		timeoutID: timeoutID,
	}, nil
}

// Start starts the sync manager.
func (s *syncManagerV2) Start() {
	s.mgr.Start(context.Background())
}

// Stop stops the sync manager, every peer's actors, and its timeout actor.
func (s *syncManagerV2) Stop() {
	s.mgr.Stop()
	s.system.StopAndRemoveActor(s.timeoutID)
}

// InitSyncState registers a new peer and returns once its actors run.
func (s *syncManagerV2) InitSyncState(peer lnpeer.Peer) error {
	ctx, cancel := lnutils.ContextFromQuit(peer.QuitSignal())
	defer cancel()

	return s.mgr.InitSyncState(ctx, peer)
}

// PruneSyncState tears down a disconnected peer's actors.
func (s *syncManagerV2) PruneSyncState(peer route.Vertex) {
	s.mgr.PruneSyncState(peer)
}

// IsGraphSynced reports whether a historical sync has completed.
func (s *syncManagerV2) IsGraphSynced() bool {
	return s.mgr.IsGraphSynced()
}

// SyncTypeOf returns a connected peer's sync type.
func (s *syncManagerV2) SyncTypeOf(peer route.Vertex) (SyncerType, bool) {
	t, ok := s.mgr.SyncType(context.Background(), peer)
	if !ok {
		return 0, false
	}

	switch t {
	case gossipsync.ActiveSync:
		return ActiveSync, true
	case gossipsync.PinnedSync:
		return PinnedSync, true
	default:
		return PassiveSync, true
	}
}

// deliverQueryMsg routes a gossip query, reply or timestamp filter to the
// peer's actors. It waits while the target mailbox is full, which pushes back
// on this peer's own message stream, and gives up if the peer disconnects.
func (s *syncManagerV2) deliverQueryMsg(_ context.Context, peer lnpeer.Peer,
	msg lnwire.Message) error {

	ctx, cancel := lnutils.ContextFromQuit(peer.QuitSignal())
	defer cancel()

	err := s.mgr.DeliverPeerMsg(ctx, route.Vertex(peer.PubKey()), msg)
	if errors.Is(err, gossipsync.ErrSyncerNotFound) {
		log.Warnf("Gossip syncer for peer=%x not found",
			peer.PubKey())

		return ErrGossipSyncerNotFound
	}

	return err
}

// forwardBatch offers a batch of remote gossip to every peer's responder.
// The gossiper adds every covered peer to each message's senders as soon as
// this returns, which is safe because Forward copies them first.
func (s *syncManagerV2) forwardBatch(ctx context.Context,
	batch []msgWithSenders) map[route.Vertex]struct{} {

	fwd := make([]gossipsync.ForwardMsg, len(batch))
	for i, m := range batch {
		fwd[i] = gossipsync.ForwardMsg{Msg: m.msg, Senders: m.senders}
	}

	return s.mgr.Forward(ctx, fwd)
}
