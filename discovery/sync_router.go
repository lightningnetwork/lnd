package discovery

import (
	"context"

	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// syncManager is what the gossiper needs from a gossip sync manager. It is
// provided by the SyncManager, and by the actor based syncer in the
// gossipsync package when GossipSyncerV2 is set.
type syncManager interface {
	// Start starts the sync manager.
	Start()

	// Stop stops the sync manager and every peer's syncer.
	Stop()

	// InitSyncState registers a new peer that supports gossip queries,
	// and returns once gossip from it can be routed.
	InitSyncState(peer lnpeer.Peer) error

	// PruneSyncState tears down a disconnected peer's syncer.
	PruneSyncState(peer route.Vertex)

	// IsGraphSynced reports whether a historical sync has completed.
	IsGraphSynced() bool

	// SyncTypeOf returns a connected peer's sync type.
	SyncTypeOf(peer route.Vertex) (SyncerType, bool)

	// deliverQueryMsg routes a gossip query, reply or timestamp filter
	// from a peer to its syncer.
	deliverQueryMsg(ctx context.Context, peer lnpeer.Peer,
		msg lnwire.Message) error

	// forwardBatch offers a batch of remote gossip to every peer with a
	// syncer, each of which sends what its filter admits. It returns the
	// peers the batch was offered to, which the caller must not also
	// broadcast it to.
	forwardBatch(ctx context.Context,
		batch []msgWithSenders) map[route.Vertex]struct{}
}

// A compile-time check that the SyncManager provides a syncManager.
var _ syncManager = (*SyncManager)(nil)

// SyncTypeOf returns a connected peer's sync type.
func (m *SyncManager) SyncTypeOf(peer route.Vertex) (SyncerType, bool) {
	syncer, ok := m.GossipSyncer(peer)
	if !ok {
		return 0, false
	}

	return syncer.SyncType(), true
}

// deliverQueryMsg routes a gossip query, reply or timestamp filter from a
// peer to its GossipSyncer.
func (m *SyncManager) deliverQueryMsg(_ context.Context, peer lnpeer.Peer,
	msg lnwire.Message) error {

	syncer, ok := m.GossipSyncer(peer.PubKey())
	if !ok {
		log.Warnf("Gossip syncer for peer=%x not found",
			peer.PubKey())

		return ErrGossipSyncerNotFound
	}

	// A timestamp filter is queued for asynchronous processing, so rate
	// limiting doesn't block the gossiper. A full queue drops it, but we
	// still report success so the peer isn't disconnected.
	if filter, ok := msg.(*lnwire.GossipTimestampRange); ok {
		if !syncer.QueueTimestampRange(filter) {
			log.Warnf("Unable to queue gossip filter for "+
				"peer=%x: queue full", peer.PubKey())
		}

		return nil
	}

	err := syncer.ProcessQueryMsg(msg, peer.QuitSignal())
	if err != nil {
		log.Errorf("Process query msg from peer %x got %v",
			peer.PubKey(), err)
	}

	return err
}

// forwardBatch filters a batch of remote gossip through every active
// GossipSyncer.
func (m *SyncManager) forwardBatch(ctx context.Context,
	batch []msgWithSenders) map[route.Vertex]struct{} {

	syncers := m.GossipSyncers()

	covered := make(map[route.Vertex]struct{}, len(syncers))
	for pub, syncer := range syncers {
		log.Tracef("Sending messages batch to GossipSyncer(%s)", pub)
		syncer.FilterGossipMsgs(ctx, batch...)

		covered[pub] = struct{}{}
	}

	return covered
}
