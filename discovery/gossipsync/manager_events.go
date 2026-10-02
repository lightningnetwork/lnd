package gossipsync

import "github.com/lightningnetwork/lnd/routing/route"

// SessionID identifies one connection to a peer. The manager assigns a fresh
// ID to every connection, so a command or outcome for an earlier connection
// can never be applied to a later one with the same public key.
type SessionID uint64

// ManagerEvent is the sealed set of inputs to the manager state machine.
type ManagerEvent interface {
	managerEvent()
}

// PeerConnected reports a new peer connection that supports gossip queries.
type PeerConnected struct {
	// Peer is the peer's public key.
	Peer route.Vertex

	// Pinned is set if the operator configured the peer to always be an
	// active syncer.
	Pinned bool
}

// PeerDisconnected reports that a peer's connection has closed.
type PeerDisconnected struct {
	// Peer is the peer's public key.
	Peer route.Vertex
}

// RotateTick asks the manager to swap one active syncer for a passive one, so
// that over time we receive new gossip from many different peers.
type RotateTick struct{}

// HistoricalTick asks the manager to run a historical sync with a random
// peer, to pick up anything the active syncers missed.
type HistoricalTick struct{}

// HistoricalSyncOutcome reports how a peer syncer's attempt ended.
type HistoricalSyncOutcome struct {
	// Session is the connection the attempt ran on.
	Session SessionID

	// Attempt is the attempt that ended.
	Attempt AttemptID

	// Outcome says how it ended.
	Outcome SyncOutcome
}

func (*PeerConnected) managerEvent()         {}
func (*PeerDisconnected) managerEvent()      {}
func (*RotateTick) managerEvent()            {}
func (*HistoricalTick) managerEvent()        {}
func (*HistoricalSyncOutcome) managerEvent() {}

// ManagerOutbox is the sealed set of side effects the manager can request.
type ManagerOutbox interface {
	managerOutbox()
}

// SpawnPeer asks for the per-peer actors of a new session to be started.
type SpawnPeer struct {
	// Session is the new session.
	Session SessionID

	// Peer is the peer's public key.
	Peer route.Vertex
}

// StopPeer asks for the per-peer actors of a session to be stopped.
type StopPeer struct {
	// Session is the session to stop.
	Session SessionID
}

// TellSyncer asks for an event to be delivered to a session's peer syncer.
type TellSyncer struct {
	// Session is the target session.
	Session SessionID

	// Event is the event to deliver.
	Event SyncerEvent
}

// PublishGraphSynced asks for the graph to be reported as synced. It is
// emitted once, the first time any historical sync completes.
type PublishGraphSynced struct{}

// ResetHistoricalTimer asks for the historical sync timer to restart its
// period, because a historical sync just started outside of it.
type ResetHistoricalTimer struct{}

func (*SpawnPeer) managerOutbox()            {}
func (*StopPeer) managerOutbox()             {}
func (*TellSyncer) managerOutbox()           {}
func (*PublishGraphSynced) managerOutbox()   {}
func (*ResetHistoricalTimer) managerOutbox() {}
