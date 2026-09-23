package gossipsync

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// This file holds a small reference model of the manager's contract, the Go
// counterpart of pmodel/src/manager.p, written without any of the helpers in
// manager_state.go. TestRefModelManager drives it and the real ManagerState
// with the same inputs. The real manager makes its own random picks. At each
// one, the test finds Go's whole candidate set by rerunning the pure
// transition at every index, and requires it to equal the set the contract
// allows. The reference model then adopts Go's pick. After every input, the
// two must agree on the observable snapshot.
//
// TestManagerProperties already checks the invariants from the outbox. This
// test checks something different: that the Go manager's choices at each
// decision are exactly the contract's, and that the state they lead to is
// the contract's state.

// refPeer is a connected peer in the reference model. retry is set on a
// pinned peer whose attempt failed, for the next tick to retry.
type refPeer struct {
	pub    route.Vertex
	role   SyncType
	pinned bool
	retry  bool
}

// refChooser returns the session a decision lands on. allowed is nil for a
// forced action, whose session is want.
type refChooser func(kind string, allowed []SessionID,
	want SessionID) SessionID

// refManager is the reference model of the manager contract.
type refManager struct {
	quota int

	peers     map[SessionID]refPeer
	sessionOf map[route.Vertex]SessionID
	inFlight  map[AttemptID]SessionID
	tracked   AttemptID
	failedAt  map[route.Vertex]uint64
	epoch     uint64
	synced    bool

	nextSession SessionID
	nextAttempt AttemptID

	// The observable effects of the input being handled.
	started        []AttemptID
	reset, publish bool
}

// newRefManager returns the reference model's initial state.
func newRefManager(quota int) *refManager {
	return &refManager{
		quota:       quota,
		peers:       make(map[SessionID]refPeer),
		sessionOf:   make(map[route.Vertex]SessionID),
		inFlight:    make(map[AttemptID]SessionID),
		failedAt:    make(map[route.Vertex]uint64),
		nextSession: 1,
		nextAttempt: 1,
	}
}

// sessions returns the non-pinned sessions that match keep, sorted.
func (r *refManager) sessions(keep func(SessionID, refPeer) bool) []SessionID {
	var out []SessionID
	for s, p := range r.peers {
		if !p.pinned && keep(s, p) {
			out = append(out, s)
		}
	}
	slices.Sort(out)

	return out
}

// hasAttempt reports whether a session has an attempt in flight.
func (r *refManager) hasAttempt(s SessionID) bool {
	for _, is := range r.inFlight {
		if is == s {
			return true
		}
	}

	return false
}

// failedIn reports whether a peer broke an attempt in the given epoch.
func (r *refManager) failedIn(pub route.Vertex, epoch uint64) bool {
	e, ok := r.failedAt[pub]
	return ok && e == epoch
}

// eligible returns the sessions a historical sync may run on.
func (r *refManager) eligible(exclude SessionID) []SessionID {
	return r.sessions(func(s SessionID, p refPeer) bool {
		return s != exclude && !r.hasAttempt(s) &&
			!r.failedIn(p.pub, r.epoch)
	})
}

// withRole returns the non-pinned sessions with the given role.
func (r *refManager) withRole(role SyncType) []SessionID {
	return r.sessions(func(_ SessionID, p refPeer) bool {
		return p.role == role
	})
}

// start begins an attempt on s.
func (r *refManager) start(s SessionID, tracked bool) {
	a := r.nextAttempt
	r.nextAttempt++
	r.inFlight[a] = s
	r.started = append(r.started, a)
	if tracked {
		r.tracked = a
		r.reset = true
	}
}

// setRole changes a peer's role.
func (r *refManager) setRole(s SessionID, role SyncType) {
	p := r.peers[s]
	p.role = role
	r.peers[s] = p
}

// step applies one input. Every decision goes through choose.
func (r *refManager) step(ev ManagerEvent, choose refChooser) {
	r.started, r.reset, r.publish = nil, false, false

	switch ev := ev.(type) {
	case *PeerConnected:
		r.connect(ev, choose)

	case *PeerDisconnected:
		r.disconnect(ev.Peer)

	case *HistoricalSyncOutcome:
		r.outcome(ev)

	case *HistoricalTick:
		r.tick(choose)

	case *RotateTick:
		active := r.withRole(ActiveSync)
		passive := r.withRole(PassiveSync)
		if len(active) > 0 && len(passive) > 0 {
			a := choose("demote", active, 0)
			p := choose("promote", passive, 0)
			r.setRole(a, PassiveSync)
			r.setRole(p, ActiveSync)
		}
	}

	// Settle the two goals.
	if r.synced {
		for len(r.withRole(ActiveSync)) < r.quota {
			passive := r.withRole(PassiveSync)
			if len(passive) == 0 {
				break
			}
			r.setRole(choose("promote", passive, 0), ActiveSync)
		}

		return
	}

	if r.tracked == 0 && r.quota > 0 {
		if c := r.eligible(0); len(c) > 0 {
			r.start(choose("start", c, 0), true)
		}
	}
}

// connect adds a peer.
func (r *refManager) connect(ev *PeerConnected, choose refChooser) {
	if _, ok := r.sessionOf[ev.Peer]; ok {
		return
	}

	hadNone := len(r.sessions(func(SessionID, refPeer) bool {
		return true
	})) == 0

	s := r.nextSession
	r.nextSession++
	r.sessionOf[ev.Peer] = s
	r.peers[s] = refPeer{pub: ev.Peer, role: PassiveSync}

	if ev.Pinned {
		r.peers[s] = refPeer{
			pub: ev.Peer, role: PinnedSync, pinned: true,
		}
		r.start(choose("start", nil, s), false)

		return
	}

	if r.synced && hadNone && r.quota > 0 &&
		!r.failedIn(ev.Peer, r.epoch) {

		r.start(choose("start", nil, s), false)
	}
}

// disconnect removes a peer and forgets its attempts.
func (r *refManager) disconnect(pub route.Vertex) {
	s, ok := r.sessionOf[pub]
	if !ok {
		return
	}

	delete(r.sessionOf, pub)
	delete(r.peers, s)
	for a, is := range r.inFlight {
		if is == s {
			delete(r.inFlight, a)
			if r.tracked == a {
				r.tracked = 0
			}
		}
	}
}

// outcome records an attempt's end, if it is in flight on its session.
func (r *refManager) outcome(ev *HistoricalSyncOutcome) {
	s, ok := r.inFlight[ev.Attempt]
	if !ok || s != ev.Session {
		return
	}

	delete(r.inFlight, ev.Attempt)
	if r.tracked == ev.Attempt {
		r.tracked = 0
	}

	if ev.Outcome.Kind == OutcomeCompleted {
		if !r.synced {
			r.synced = true
			r.tracked = 0
			r.publish = true
		}

		return
	}

	// A pinned peer is retried by the next tick instead of backed off.
	p := r.peers[s]
	if p.pinned {
		p.retry = true
		r.peers[s] = p

		return
	}
	r.failedAt[p.pub] = r.epoch
}

// tick opens an epoch, retries every pinned peer marked to retry, and starts
// an attempt on an eligible peer other than the tracked one, preferring
// peers that did not fail in the last epoch.
func (r *refManager) tick(choose refChooser) {
	r.epoch++

	// The retries are forced, one per marked pinned peer with nothing in
	// flight. Ascending order only keeps attempt IDs aligned with Go.
	var retry []SessionID
	for s, p := range r.peers {
		if p.pinned && p.retry && !r.hasAttempt(s) {
			retry = append(retry, s)
		}
	}
	slices.Sort(retry)
	for _, s := range retry {
		p := r.peers[s]
		p.retry = false
		r.peers[s] = p
		r.start(choose("start", nil, s), false)
	}

	if r.quota == 0 {
		return
	}

	var exclude SessionID
	if r.tracked != 0 {
		exclude = r.inFlight[r.tracked]
	}

	c := r.eligible(exclude)
	var preferred []SessionID
	for _, s := range c {
		if !r.failedIn(r.peers[s].pub, r.epoch-1) {
			preferred = append(preferred, s)
		}
	}
	if len(preferred) > 0 {
		c = preferred
	}

	if len(c) > 0 {
		r.start(choose("start", c, 0), !r.synced)
	}
}

// snap renders the reference model's observable snapshot.
func (r *refManager) snap() string {
	roles := make(map[SessionID]SyncType, len(r.peers))
	for s, p := range r.peers {
		roles[s] = p.role
	}

	var inFlight []SessionID
	for _, s := range r.inFlight {
		inFlight = append(inFlight, s)
	}

	var tracked SessionID
	if r.tracked != 0 {
		tracked = r.inFlight[r.tracked]
	}

	return renderManagerSnap(
		roles, r.synced, tracked, inFlight, r.started, r.reset,
		r.publish,
	)
}

// TestRefModelManager drives the real manager and the reference model with
// the same random workload, checks that every choice the real manager makes
// is one the contract allows, and compares their observable snapshots after
// every input.
func TestRefModelManager(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		numPeers := rapid.IntRange(1, 5).Draw(rt, "numPeers")
		pubs := make([]route.Vertex, numPeers)
		pinned := make(map[route.Vertex]bool)
		for i := range pubs {
			pubs[i] = route.Vertex{byte(i + 1)}
			pinned[pubs[i]] = rapid.IntRange(0, 4).Draw(
				rt, "pinned",
			) == 0
		}

		// draws records the index Go drew at each pick of the step.
		var draws []int
		quota := rapid.IntRange(0, 3).Draw(rt, "numActive")
		env := &ManagerEnv{
			NumActiveSyncers: quota,
			Rand: func(n int) int {
				i := rapid.IntRange(0, n-1).Draw(rt, "pick")
				draws = append(draws, i)

				return i
			},
		}
		ref := newRefManager(quota)
		var state ManagerMachineState = NewManagerState()

		kinds := []OutcomeKind{
			OutcomeCompleted, OutcomePeerFault, OutcomeBusy,
			OutcomeLocalFault,
		}

		steps := rapid.IntRange(1, 100).Draw(rt, "steps")
		for i := range steps {
			var ev ManagerEvent
			switch rapid.IntRange(0, 7).Draw(rt, "op") {
			case 0, 1:
				p := rapid.SampledFrom(pubs).Draw(rt, "peer")
				ev = &PeerConnected{Peer: p, Pinned: pinned[p]}

			case 2:
				p := rapid.SampledFrom(pubs).Draw(rt, "peer")
				ev = &PeerDisconnected{Peer: p}

			case 3:
				ev = &RotateTick{}

			case 4:
				ev = &HistoricalTick{}

			// An outcome for an attempt in flight, from its
			// session.
			case 5, 6:
				ids := slices.Sorted(func(yield func(
					AttemptID) bool) {

					for a := range ref.inFlight {
						if !yield(a) {
							return
						}
					}
				})
				if len(ids) == 0 {
					continue
				}
				a := rapid.SampledFrom(ids).Draw(rt, "attempt")
				ev = &HistoricalSyncOutcome{
					Session: ref.inFlight[a],
					Attempt: a,
					Outcome: SyncOutcome{
						Kind: rapid.SampledFrom(
							kinds,
						).Draw(rt, "kind"),
					},
				}

			// Any other outcome, which is stale unless it
			// happens to name an attempt in flight on its
			// session.
			default:
				ev = &HistoricalSyncOutcome{
					Session: SessionID(rapid.IntRange(
						0, int(ref.nextSession),
					).Draw(rt, "staleSession")),
					Attempt: AttemptID(rapid.IntRange(
						0, int(ref.nextAttempt),
					).Draw(rt, "staleAttempt")),
					Outcome: SyncOutcome{
						Kind: rapid.SampledFrom(
							kinds,
						).Draw(rt, "kind"),
					},
				}
			}

			draws = nil
			prev := state
			next, out, err := protofsm.ApplyEvents(
				context.Background(), state, ev, env,
			)
			require.NoError(rt, err)
			state = next

			// The reference model adopts each of Go's picks,
			// which must be allowed, in the order Go acted on
			// them within each kind.
			effects := map[string][]SessionID{
				"start":   pickEffects(out, "start"),
				"promote": pickEffects(out, "promote"),
				"demote":  pickEffects(out, "demote"),
			}
			var (
				picks   int
				perKind = make(map[string]int)
			)
			choose := func(kind string, allowed []SessionID,
				want SessionID) SessionID {

				ordinal := perKind[kind]
				perKind[kind]++

				// Go's candidate set at a pick must be the
				// contract's allowed set, found by rerunning
				// the pure transition at every index.
				if allowed != nil {
					require.Greater(rt, len(draws), picks,
						"step %d (%T): the contract "+
							"picks a %s, Go did "+
							"not", i, ev, kind)
					list := goCandidates(
						rt, fmt.Sprintf("step %d "+
							"(%T)", i, ev), prev,
						ev, quota, draws[:picks],
						picks, mDecision{kind: kind},
						ordinal,
					)
					picks++
					require.ElementsMatch(rt, allowed, list,
						"step %d (%T): Go's %s "+
							"candidates differ "+
							"from the contract's",
						i, ev, kind)
				}

				got := effects[kind]
				require.NotEmpty(rt, got, "step %d (%T): the "+
					"contract requires a %s, Go did none",
					i, ev, kind)
				s := got[0]
				effects[kind] = got[1:]

				// A pick is already checked against the
				// allowed set above.
				if allowed == nil {
					require.Equal(rt, want, s, "step %d "+
						"(%T): forced %s on the wrong "+
						"session", i, ev, kind)
				}

				return s
			}
			ref.step(ev, choose)
			require.Len(rt, draws, picks, "step %d (%T): Go made "+
				"%d picks, the contract %d", i, ev, len(draws),
				picks)

			for kind, rest := range effects {
				require.Empty(rt, rest, "step %d (%T): Go did "+
					"a %s the contract does not call for",
					i, ev, kind)
			}

			require.Equal(rt, ref.snap(),
				goManagerSnap(state.(*ManagerState), out),
				"step %d (%T): observable snapshots differ",
				i, ev)
		}
	})
}
