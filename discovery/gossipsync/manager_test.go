package gossipsync

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// managerHarness drives the pure manager with random events, and keeps its
// own model of the world built only from the manager's outbox, which it
// checks against the manager's state after every event.
type managerHarness struct {
	t     *rapid.T
	env   *ManagerEnv
	state *ManagerState

	// pinned is the fixed set of pinned peers.
	pinned map[route.Vertex]bool

	// live maps each live session to its peer, from SpawnPeer and
	// StopPeer.
	live map[SessionID]route.Vertex

	// roles tracks each live session's role, from SetSyncType.
	roles map[SessionID]SyncType

	// inflight tracks started attempts, from StartHistoricalSync.
	inflight map[AttemptID]SessionID

	// failedAt mirrors the manager's failure epochs, from the outcomes
	// we delivered, and epoch mirrors its tick count.
	failedAt map[route.Vertex]uint64
	epoch    uint64

	// published counts PublishGraphSynced.
	published int
}

// apply feeds one event to the manager, interprets the outbox into the
// model, and checks every invariant.
func (h *managerHarness) apply(ev ManagerEvent) {
	t := h.t

	next, out, err := protofsm.ApplyEvents(
		context.Background(), ManagerMachineState(h.state), ev, h.env,
	)
	require.NoError(t, err)
	h.state = next.(*ManagerState)

	// A tick opens its epoch before it picks a peer.
	if _, ok := ev.(*HistoricalTick); ok {
		h.epoch++
	}

	// A completion is the only thing that may publish the graph as
	// synced.
	var completes bool
	if o, ok := ev.(*HistoricalSyncOutcome); ok {
		s, known := h.inflight[o.Attempt]
		completes = known && s == o.Session &&
			o.Outcome.Kind == OutcomeCompleted
	}

	// The outcome's attempt is no longer in flight.
	if o, ok := ev.(*HistoricalSyncOutcome); ok {
		if s, known := h.inflight[o.Attempt]; known && s == o.Session {
			delete(h.inflight, o.Attempt)
			kind := o.Outcome.Kind
			pub := h.live[s]
			failed := kind != OutcomeCompleted
			if failed && !h.pinned[pub] {
				h.failedAt[pub] = h.epoch
			}
		}
	}

	for _, o := range out {
		switch o := o.(type) {
		case *SpawnPeer:
			_, dup := h.live[o.Session]
			require.False(t, dup, "session spawned twice")
			h.live[o.Session] = o.Peer
			h.roles[o.Session] = PassiveSync

		case *StopPeer:
			_, ok := h.live[o.Session]
			require.True(t, ok, "stopped an unknown session")
			delete(h.live, o.Session)
			delete(h.roles, o.Session)
			for a, s := range h.inflight {
				if s == o.Session {
					delete(h.inflight, a)
				}
			}

		case *TellSyncer:
			pub, ok := h.live[o.Session]
			require.True(t, ok, "told an unknown session")

			switch e := o.Event.(type) {
			case *SetSyncType:
				h.roles[o.Session] = e.Type

			case *StartHistoricalSync:
				for _, s := range h.inflight {
					require.NotEqual(t, o.Session, s,
						"two attempts on one session")
				}
				if !h.pinned[pub] {
					e, failed := h.failedAt[pub]
					require.False(t, failed && e == h.epoch,
						"attempt on a peer that "+
							"failed this epoch")
				}
				h.inflight[e.Attempt] = o.Session
			}

		case *PublishGraphSynced:
			require.True(t, completes, "published without a "+
				"completion")
			h.published++

		case *ResetHistoricalTimer:
		}
	}

	h.checkInvariants()
}

// checkInvariants asserts the manager's state agrees with the model built
// from its outbox, and holds the manager's own invariants.
func (h *managerHarness) checkInvariants() {
	t, m := h.t, h.state

	require.LessOrEqual(t, h.published, 1)
	require.Equal(t, h.published == 1, m.IsGraphSynced())

	var active, nonPinned, eligible int
	for s, pub := range h.live {
		role := m.Role(pub)
		require.True(t, role.IsSome())
		require.Equal(t, h.roles[s], role.UnwrapOr(PassiveSync),
			"state and outbox disagree on a role")

		if h.pinned[pub] {
			require.Equal(t, PinnedSync, h.roles[s])
			continue
		}

		require.NotEqual(t, PinnedSync, h.roles[s])
		nonPinned++
		if h.roles[s] == ActiveSync {
			active++
		}

		busy := false
		for _, is := range h.inflight {
			busy = busy || is == s
		}
		e, failed := h.failedAt[pub]
		if !busy && !(failed && e == h.epoch) {
			eligible++
		}
	}
	require.Len(t, m.members, len(h.live))

	n := h.env.NumActiveSyncers
	require.LessOrEqual(t, active, n)

	// The stall #11173 fixed: while the graph isn't synced, an eligible
	// peer means a tracked historical sync is running.
	if !m.IsGraphSynced() && n > 0 && eligible > 0 {
		require.True(t, m.tracked.IsSome(),
			"graph unsynced with an eligible peer and no "+
				"historical sync")
	}

	// Once synced, the active quota is as full as the peers allow.
	if m.IsGraphSynced() {
		require.Equal(t, min(n, nonPinned), active,
			"active quota not filled")
	}
}

// TestManagerProperties drives the manager through random connects,
// disconnects, ticks and outcomes, including stale ones, and checks its
// invariants after every event.
func TestManagerProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		seed := rapid.Uint64().Draw(rt, "seed")
		rng := rand.New(rand.NewPCG(seed, seed))

		numPeers := rapid.IntRange(1, 8).Draw(rt, "numPeers")
		pubs := make([]route.Vertex, numPeers)
		pinned := make(map[route.Vertex]bool)
		for i := range pubs {
			pubs[i] = route.Vertex{byte(i + 1)}
			pinned[pubs[i]] = rapid.IntRange(0, 4).Draw(
				rt, "pinned",
			) == 0
		}

		h := &managerHarness{
			t: rt,
			env: &ManagerEnv{
				NumActiveSyncers: rapid.IntRange(0, 4).Draw(
					rt, "numActive",
				),
				Rand: rng.IntN,
			},
			state:    NewManagerState(),
			pinned:   pinned,
			live:     make(map[SessionID]route.Vertex),
			roles:    make(map[SessionID]SyncType),
			inflight: make(map[AttemptID]SessionID),
			failedAt: make(map[route.Vertex]uint64),
		}

		kinds := []OutcomeKind{
			OutcomeCompleted, OutcomePeerFault, OutcomeBusy,
			OutcomeLocalFault,
		}

		steps := rapid.IntRange(1, 120).Draw(rt, "steps")
		for range steps {
			action := rapid.SampledFrom([]string{
				"connect", "connect", "disconnect", "rotate",
				"tick", "outcome", "outcome", "stale",
			}).Draw(rt, "action")

			switch action {
			case "connect":
				p := rapid.SampledFrom(pubs).Draw(rt, "peer")
				h.apply(&PeerConnected{
					Peer: p, Pinned: pinned[p],
				})

			case "disconnect":
				p := rapid.SampledFrom(pubs).Draw(rt, "peer")
				h.apply(&PeerDisconnected{Peer: p})

			case "rotate":
				h.apply(&RotateTick{})

			case "tick":
				h.apply(&HistoricalTick{})

			case "outcome":
				if len(h.inflight) == 0 {
					continue
				}
				var ids []AttemptID
				for a := range h.inflight {
					ids = append(ids, a)
				}
				slicesSortAttempts(ids)
				a := rapid.SampledFrom(ids).Draw(rt, "attempt")
				k := rapid.SampledFrom(kinds).Draw(rt, "kind")
				h.apply(&HistoricalSyncOutcome{
					Session: h.inflight[a],
					Attempt: a,
					Outcome: SyncOutcome{
						Kind:   k,
						Reason: errors.New("x"),
					},
				})

			// An outcome for an attempt that ended, or from a
			// session that doesn't own it, changes nothing.
			case "stale":
				a := AttemptID(rapid.Uint64Range(
					0, uint64(h.state.nextAttempt),
				).Draw(rt, "staleAttempt"))
				s := SessionID(rapid.Uint64Range(
					0, uint64(h.state.nextSession),
				).Draw(rt, "staleSession"))
				if is, ok := h.inflight[a]; ok && is == s {
					continue
				}

				before := h.state
				h.apply(&HistoricalSyncOutcome{
					Session: s, Attempt: a,
					Outcome: SyncOutcome{
						Kind: OutcomeCompleted,
					},
				})
				require.Equal(rt, before.inflight,
					h.state.inflight)
				require.Equal(rt, before.graphSynced,
					h.state.graphSynced)
			}
		}
	})
}

// slicesSortAttempts sorts attempt IDs, so rapid's draw is reproducible.
func slicesSortAttempts(ids []AttemptID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// TestManagerIgnoresDuplicateConnect asserts that a second connection for a
// connected peer leaves the existing session alone.
func TestManagerIgnoresDuplicateConnect(t *testing.T) {
	t.Parallel()

	env := &ManagerEnv{NumActiveSyncers: 1, Rand: func(int) int {
		return 0
	}}
	var state ManagerMachineState = NewManagerState()
	peer := route.Vertex{1}

	state, out, err := protofsm.ApplyEvents(
		t.Context(), state, ManagerEvent(&PeerConnected{Peer: peer}),
		env,
	)
	require.NoError(t, err)
	require.IsType(t, &SpawnPeer{}, out[0])

	session := state.(*ManagerState).Session(peer)

	state, out, err = protofsm.ApplyEvents(
		t.Context(), state, ManagerEvent(&PeerConnected{Peer: peer}),
		env,
	)
	require.NoError(t, err)
	require.Empty(t, out)
	require.Equal(t, session, state.(*ManagerState).Session(peer))
	require.Equal(t, fn.Some(PassiveSync), state.(*ManagerState).Role(peer))
}

// TestManagerBacksOffLocalFault asserts that a local lookup failure doesn't
// restart the attempt against the same peer at once, which would have the
// peer stream its whole channel range to us in a loop while the lookup keeps
// failing. The peer is retried once the next tick opens a new epoch.
func TestManagerBacksOffLocalFault(t *testing.T) {
	t.Parallel()

	env := &ManagerEnv{NumActiveSyncers: 1, Rand: func(int) int {
		return 0
	}}
	apply := func(state ManagerMachineState,
		ev ManagerEvent) (ManagerMachineState, []ManagerOutbox) {

		next, out, err := protofsm.ApplyEvents(
			t.Context(), state, ev, env,
		)
		require.NoError(t, err)

		return next, out
	}
	started := func(out []ManagerOutbox) bool {
		for _, o := range out {
			tell, ok := o.(*TellSyncer)
			if !ok {
				continue
			}
			if _, ok := tell.Event.(*StartHistoricalSync); ok {
				return true
			}
		}

		return false
	}

	var state ManagerMachineState = NewManagerState()
	state, out := apply(state, &PeerConnected{Peer: route.Vertex{1}})
	require.True(t, started(out))

	state, out = apply(state, &HistoricalSyncOutcome{
		Session: 1, Attempt: 1,
		Outcome: SyncOutcome{Kind: OutcomeLocalFault},
	})
	require.False(t, started(out), "local fault retried at once")

	_, out = apply(state, &HistoricalTick{})
	require.True(t, started(out), "local fault not retried next epoch")
}

// TestManagerRetriesPinnedPeer asserts that a pinned peer whose historical
// sync failed runs a new one at the next tick, and not before. Nothing else
// ever picks a pinned peer, so without the retry a node with no active
// syncers would stay unsynced until the peer reconnected.
func TestManagerRetriesPinnedPeer(t *testing.T) {
	t.Parallel()

	env := &ManagerEnv{NumActiveSyncers: 0, Rand: func(int) int {
		return 0
	}}
	apply := func(state ManagerMachineState,
		ev ManagerEvent) (ManagerMachineState, []AttemptID) {

		next, out, err := protofsm.ApplyEvents(
			t.Context(), state, ev, env,
		)
		require.NoError(t, err)

		var started []AttemptID
		for _, o := range out {
			tell, ok := o.(*TellSyncer)
			if !ok {
				continue
			}
			if s, ok := tell.Event.(*StartHistoricalSync); ok {
				started = append(started, s.Attempt)
			}
		}

		return next, started
	}

	var state ManagerMachineState = NewManagerState()
	state, started := apply(state, &PeerConnected{
		Peer: route.Vertex{1}, Pinned: true,
	})
	require.Equal(t, []AttemptID{1}, started)

	state, started = apply(state, &HistoricalSyncOutcome{
		Session: 1, Attempt: 1,
		Outcome: SyncOutcome{Kind: OutcomePeerFault},
	})
	require.Empty(t, started, "pinned peer retried at once")

	state, started = apply(state, &HistoricalTick{})
	require.Equal(t, []AttemptID{2}, started,
		"pinned peer not retried at the next tick")

	// The retry is spent, so a tick while it runs starts nothing.
	state, started = apply(state, &HistoricalTick{})
	require.Empty(t, started)

	// Once a sync completes, later ticks don't restart it.
	state, _ = apply(state, &HistoricalSyncOutcome{
		Session: 1, Attempt: 2,
		Outcome: SyncOutcome{Kind: OutcomeCompleted},
	})
	require.True(t, state.(*ManagerState).IsGraphSynced())

	_, started = apply(state, &HistoricalTick{})
	require.Empty(t, started)
}
