package gossipsync

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ManagerEnv is the read-only environment of the manager.
type ManagerEnv struct {
	// NumActiveSyncers is how many non-pinned peers we keep active. Zero
	// disables historical syncs with non-pinned peers.
	NumActiveSyncers int

	// Rand returns a uniformly random integer in [0, n). Tests pass a
	// seeded source so every pick is reproducible.
	Rand func(n int) int
}

// Name returns a name for the environment, for logging.
//
// NOTE: This implements the protofsm.Environment interface.
func (e *ManagerEnv) Name() string {
	return "gossipsync-manager"
}

// managerTransition is the transition type of the manager.
type managerTransition = protofsm.StateTransition[
	ManagerEvent, ManagerOutbox, *ManagerEnv,
]

// ManagerMachineState is a state of the manager.
type ManagerMachineState = protofsm.State[
	ManagerEvent, ManagerOutbox, *ManagerEnv,
]

// member is a connected peer.
type member struct {
	// pub is the peer's public key.
	pub route.Vertex

	// role is the peer's current sync type.
	role SyncType

	// retry is set when a pinned peer's historical sync failed. The next
	// HistoricalTick starts a new one, so the tick interval is the
	// backoff.
	retry bool
}

// attempt is a historical sync attempt in flight.
type attempt struct {
	// session is the connection the attempt runs on.
	session SessionID
}

// ManagerState is the manager's only state. It decides the role of every
// connected peer and which peer runs each historical sync.
//
// A transition never mutates the state it was called on: it clones the
// state, changes the clone, and returns it as the next state.
type ManagerState struct {
	// members are the connected peers, by session.
	members map[SessionID]member

	// sessions maps a connected peer to its session.
	sessions map[route.Vertex]SessionID

	// pinned is the set of peers the operator pinned.
	pinned map[route.Vertex]struct{}

	// inflight are the historical sync attempts in flight.
	inflight map[AttemptID]attempt

	// tracked is the attempt we fail over when it fails or its peer
	// disconnects, while the graph is not yet synced.
	tracked fn.Option[AttemptID]

	// failedAt records, for each peer whose attempt it broke, the tick
	// epoch in which it did. A peer is not picked again in that epoch, and
	// the next scheduled pick avoids it if any other peer is eligible.
	failedAt map[route.Vertex]uint64

	// epoch counts HistoricalTicks.
	epoch uint64

	// nextSession and nextAttempt are the next IDs to assign.
	nextSession SessionID
	nextAttempt AttemptID

	// graphSynced is set once any historical sync has completed.
	graphSynced bool

	// out collects the outbox of the transition in progress. It is only
	// used on a clone.
	out []ManagerOutbox
}

// NewManagerState returns the manager's initial state.
func NewManagerState() *ManagerState {
	return &ManagerState{
		members:     make(map[SessionID]member),
		sessions:    make(map[route.Vertex]SessionID),
		pinned:      make(map[route.Vertex]struct{}),
		inflight:    make(map[AttemptID]attempt),
		failedAt:    make(map[route.Vertex]uint64),
		nextSession: 1,
		nextAttempt: 1,
	}
}

// String returns a summary of the manager's phase.
func (m *ManagerState) String() string {
	switch {
	case len(m.members) == 0:
		return "WaitingForPeers"
	case !m.graphSynced:
		return "InitialSync"
	default:
		return "Synced"
	}
}

// IsTerminal returns false: the manager runs for the life of the daemon.
func (m *ManagerState) IsTerminal() bool {
	return false
}

// IsGraphSynced reports whether any historical sync has completed.
func (m *ManagerState) IsGraphSynced() bool {
	return m.graphSynced
}

// Role returns the role of a connected peer.
func (m *ManagerState) Role(pub route.Vertex) fn.Option[SyncType] {
	s, ok := m.sessions[pub]
	if !ok {
		return fn.None[SyncType]()
	}

	return fn.Some(m.members[s].role)
}

// Session returns the session of a connected peer.
func (m *ManagerState) Session(pub route.Vertex) fn.Option[SessionID] {
	s, ok := m.sessions[pub]
	if !ok {
		return fn.None[SessionID]()
	}

	return fn.Some(s)
}

// clone returns a deep copy of the state with an empty outbox.
func (m *ManagerState) clone() *ManagerState {
	c := *m
	c.members = maps.Clone(m.members)
	c.sessions = maps.Clone(m.sessions)
	c.pinned = maps.Clone(m.pinned)
	c.inflight = maps.Clone(m.inflight)
	c.failedAt = maps.Clone(m.failedAt)
	c.out = nil

	return &c
}

// emit appends outbox events to the transition in progress.
func (m *ManagerState) emit(o ...ManagerOutbox) {
	m.out = append(m.out, o...)
}

// done wraps the clone into a transition, moving its outbox into the emitted
// events.
func (m *ManagerState) done() (*managerTransition, error) {
	out := m.out
	m.out = nil

	t := &managerTransition{NextState: m}
	if len(out) > 0 {
		t.NewEvents = fn.Some(protofsm.EmittedEvent[
			ManagerEvent, ManagerOutbox,
		]{Outbox: out})
	}

	return t, nil
}

// isPinned reports whether the session's peer is pinned.
func (m *ManagerState) isPinned(s SessionID) bool {
	_, ok := m.pinned[m.members[s].pub]
	return ok
}

// count returns how many non-pinned members have the given role, or every
// non-pinned member if role is None.
func (m *ManagerState) count(role fn.Option[SyncType]) int {
	n := 0
	for s, mem := range m.members {
		if m.isPinned(s) {
			continue
		}
		if role.IsNone() || role.UnwrapOr(0) == mem.role {
			n++
		}
	}

	return n
}

// busy reports whether a session already has an attempt in flight.
func (m *ManagerState) busy(s SessionID) bool {
	for _, a := range m.inflight {
		if a.session == s {
			return true
		}
	}

	return false
}

// failedThisEpoch reports whether the peer broke an attempt in the current
// tick epoch.
func (m *ManagerState) failedThisEpoch(pub route.Vertex) bool {
	e, ok := m.failedAt[pub]
	return ok && e == m.epoch
}

// candidates returns, in ascending session order, the non-pinned sessions
// that match keep.
func (m *ManagerState) candidates(
	keep func(SessionID, member) bool) []SessionID {

	var out []SessionID
	for s, mem := range m.members {
		if !m.isPinned(s) && keep(s, mem) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, cmp.Compare[SessionID])

	return out
}

// pick returns a random element of candidates.
func pick(env *ManagerEnv, candidates []SessionID) fn.Option[SessionID] {
	if len(candidates) == 0 {
		return fn.None[SessionID]()
	}

	return fn.Some(candidates[env.Rand(len(candidates))])
}

// historicalCandidates returns the sessions a historical sync may run on:
// non-pinned, not busy, not failed this epoch, and not excluded.
func (m *ManagerState) historicalCandidates(
	exclude fn.Option[SessionID]) []SessionID {

	return m.candidates(func(s SessionID, mem member) bool {
		return !m.busy(s) && !m.failedThisEpoch(mem.pub) &&
			exclude.UnwrapOr(0) != s
	})
}

// startAttempt starts a historical sync on s. A tracked attempt is the one
// we fail over, and starting it restarts the historical timer.
func (m *ManagerState) startAttempt(s SessionID, tracked bool) {
	a := m.nextAttempt
	m.nextAttempt++
	m.inflight[a] = attempt{session: s}

	m.emit(&TellSyncer{
		Session: s,
		Event:   &StartHistoricalSync{Attempt: a},
	})

	if tracked {
		m.tracked = fn.Some(a)
		m.emit(&ResetHistoricalTimer{})
	}
}

// setRole changes a member's role and tells its syncer.
func (m *ManagerState) setRole(s SessionID, role SyncType) {
	mem := m.members[s]
	if mem.role == role {
		return
	}

	mem.role = role
	m.members[s] = mem
	m.emit(&TellSyncer{Session: s, Event: &SetSyncType{Type: role}})
}

// fillActive promotes random passive peers until the active quota is met.
func (m *ManagerState) fillActive(env *ManagerEnv) {
	for m.count(fn.Some(ActiveSync)) < env.NumActiveSyncers {
		passive := m.candidates(func(_ SessionID, mem member) bool {
			return mem.role == PassiveSync
		})

		if len(passive) == 0 {
			return
		}

		m.setRole(passive[env.Rand(len(passive))], ActiveSync)
	}
}

// ProcessEvent handles a manager event.
//
// NOTE: This implements the protofsm.State interface.
func (m *ManagerState) ProcessEvent(_ context.Context, event ManagerEvent,
	env *ManagerEnv) (*managerTransition, error) {

	next := m.clone()

	switch ev := event.(type) {
	case *PeerConnected:
		next.onConnect(env, ev)

	case *PeerDisconnected:
		next.onDisconnect(ev)

	case *HistoricalSyncOutcome:
		next.onOutcome(ev)

	case *HistoricalTick:
		next.onHistoricalTick(env)

	case *RotateTick:
		next.onRotate(env)

	default:
		return nil, fmt.Errorf("manager: unknown event %T", event)
	}

	next.settle(env)

	return next.done()
}

// settle restores the manager's two standing goals after any event, so no
// individual event handler has to remember them. While the graph is unsynced,
// a tracked historical sync runs whenever some peer is eligible for one. Once
// it is synced, the active quota is as full as the connected peers allow.
func (m *ManagerState) settle(env *ManagerEnv) {
	if m.graphSynced {
		m.fillActive(env)

		return
	}

	if m.tracked.IsNone() && env.NumActiveSyncers > 0 {
		candidates := m.historicalCandidates(fn.None[SessionID]())
		pick(env, candidates).WhenSome(func(s SessionID) {
			m.startAttempt(s, true)
		})
	}
}

// onConnect adds a new peer. Its role, and whether a historical sync runs,
// is left to settle, except for the two cases that depend on the peer
// itself: a pinned peer, and the first peer after we lost every peer.
func (m *ManagerState) onConnect(env *ManagerEnv, ev *PeerConnected) {
	// A second connection for a peer we already track is left alone. The
	// existing session keeps running until the peer disconnects.
	if _, ok := m.sessions[ev.Peer]; ok {
		return
	}

	s := m.nextSession
	m.nextSession++

	// Count the non-pinned peers before adding this one.
	hadNoPeers := m.count(fn.None[SyncType]()) == 0

	m.members[s] = member{pub: ev.Peer, role: PassiveSync}
	m.sessions[ev.Peer] = s
	m.emit(&SpawnPeer{Session: s, Peer: ev.Peer})

	// A pinned peer is always active, and always runs its own historical
	// sync, whatever its history.
	if ev.Pinned {
		m.pinned[ev.Peer] = struct{}{}
		m.setRole(s, PinnedSync)
		m.startAttempt(s, false)

		return
	}

	// Having lost every peer, we may have missed gossip while offline,
	// so the first peer back runs a historical sync. While the graph is
	// unsynced, settle takes care of this instead.
	if m.graphSynced && hadNoPeers && env.NumActiveSyncers > 0 &&
		!m.failedThisEpoch(ev.Peer) {

		m.startAttempt(s, false)
	}
}

// onDisconnect removes a peer and its attempts. Failing over a tracked attempt
// and backfilling an active slot are left to settle.
func (m *ManagerState) onDisconnect(ev *PeerDisconnected) {
	s, ok := m.sessions[ev.Peer]
	if !ok {
		return
	}

	delete(m.sessions, ev.Peer)
	delete(m.members, s)
	delete(m.pinned, ev.Peer)
	m.emit(&StopPeer{Session: s})

	for a, info := range m.inflight {
		if info.session != s {
			continue
		}

		delete(m.inflight, a)
		if m.tracked.UnwrapOr(0) == a {
			m.tracked = fn.None[AttemptID]()
		}
	}
}

// onOutcome records how an attempt ended. An outcome for an attempt we don't
// know, or from the wrong session, is stale and ignored.
func (m *ManagerState) onOutcome(ev *HistoricalSyncOutcome) {
	info, ok := m.inflight[ev.Attempt]
	if !ok || info.session != ev.Session {
		return
	}

	delete(m.inflight, ev.Attempt)
	if m.tracked.UnwrapOr(0) == ev.Attempt {
		m.tracked = fn.None[AttemptID]()
	}

	switch ev.Outcome.Kind {
	case OutcomeCompleted:
		if !m.graphSynced {
			m.graphSynced = true
			m.tracked = fn.None[AttemptID]()
			m.emit(&PublishGraphSynced{})
		}

	// The peer is skipped for the rest of the epoch. A local fault says
	// nothing about the peer, but it is backed off the same way:
	// otherwise settle would restart the attempt at once, and a lookup
	// that keeps failing would have the peer stream its whole channel
	// range to us in a loop. A pinned peer is never picked by settle or
	// a tick, so instead it is marked to retry at the next tick.
	case OutcomePeerFault, OutcomeBusy, OutcomeLocalFault:
		mem := m.members[info.session]
		if m.isPinned(info.session) {
			mem.retry = true
			m.members[info.session] = mem

			return
		}
		m.failedAt[mem.pub] = m.epoch
	}
}

// onHistoricalTick opens a new epoch, retries the pinned peers whose sync
// failed, then starts a historical sync with a random peer.
//
// Opening the epoch first makes peers that failed in the last epoch eligible
// again, but they are only picked when no other peer is: a failed peer is
// skipped for the next scheduled pick if there is an alternative, and retried
// at that pick if there isn't, so a node with a single peer never waits more
// than one tick.
func (m *ManagerState) onHistoricalTick(env *ManagerEnv) {
	m.epoch++

	// Only failures in this epoch and the last are ever read, so older
	// ones are dropped rather than kept for every peer that ever failed.
	maps.DeleteFunc(m.failedAt, func(_ route.Vertex, e uint64) bool {
		return e+1 < m.epoch
	})

	m.retryPinned()

	if env.NumActiveSyncers == 0 {
		return
	}

	// Skip the peer running the tracked attempt, so a slow initial sync
	// moves to a different peer.
	var exclude fn.Option[SessionID]
	m.tracked.WhenSome(func(a AttemptID) {
		exclude = fn.Some(m.inflight[a].session)
	})

	candidates := m.historicalCandidates(exclude)
	preferred := slices.DeleteFunc(slices.Clone(candidates),
		func(s SessionID) bool {
			e, ok := m.failedAt[m.members[s].pub]
			return ok && e == m.epoch-1
		},
	)
	if len(preferred) > 0 {
		candidates = preferred
	}

	pick(env, candidates).WhenSome(func(s SessionID) {
		m.startAttempt(s, !m.graphSynced)
	})
}

// retryPinned starts a new historical sync on every pinned peer whose last
// one failed. A pinned peer runs its own sync on connect, outside the
// tracked attempt, and nothing else ever picks it, so without this a single
// failure would leave it unsynced until it reconnected. With a quota of zero,
// or only pinned peers connected, that would leave the whole graph unsynced.
func (m *ManagerState) retryPinned() {
	for _, s := range m.candidatesPinned() {
		mem := m.members[s]
		mem.retry = false
		m.members[s] = mem

		m.startAttempt(s, false)
	}
}

// candidatesPinned returns, in ascending session order, the pinned sessions
// marked to retry that have no attempt in flight.
func (m *ManagerState) candidatesPinned() []SessionID {
	var out []SessionID
	for s, mem := range m.members {
		if mem.retry && m.isPinned(s) && !m.busy(s) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, cmp.Compare[SessionID])

	return out
}

// onRotate swaps a random active peer for a random passive one.
func (m *ManagerState) onRotate(env *ManagerEnv) {
	active := m.candidates(func(_ SessionID, mem member) bool {
		return mem.role == ActiveSync
	})
	passive := m.candidates(func(_ SessionID, mem member) bool {
		return mem.role == PassiveSync
	})
	if len(active) == 0 || len(passive) == 0 {
		return
	}

	a := active[env.Rand(len(active))]
	p := passive[env.Rand(len(passive))]
	m.setRole(a, PassiveSync)
	m.setRole(p, ActiveSync)
}
