package gossipsync

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// settle advances fake time in slices, letting every actor quiesce after
// each one. A single long sleep would skip past timers that only get armed
// by handlers of earlier timers.
func settle(d time.Duration, slices int) {
	for range slices {
		time.Sleep(d / time.Duration(slices))
		synctest.Wait()
	}
}

// TestSimInitialSync runs two real nodes in a bubble: one with an empty
// graph, one with a few hundred channels. The empty node's initial
// historical sync must fetch every channel and mark the graph synced.
func TestSimInitialSync(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cfg := simConfig{numActive: 1, seed: 1}
		alice := newSimNode(t, 1, cfg)
		bob := newSimNode(t, 2, cfg)
		seedGraph(bob, rand.New(rand.NewPCG(1, 1)), 300)

		connect(t, alice, bob, nil, nil)
		settle(time.Minute, 10)

		require.Equal(t, bob.graph.scids(), alice.graph.scids())
		require.True(t, alice.mgr.IsGraphSynced())

		// Bob has nothing to learn from Alice, so his sync completes
		// too, with no SCID queries.
		require.True(t, bob.mgr.IsGraphSynced())
	})
}

// opKind is a workload operation.
type opKind uint8

const (
	opConnect opKind = iota
	opDisconnect
	opFaults
	opAdvance
	opFlood
)

// op is one workload step. Node 0 is the node under test; the rest are
// honest nodes followed by byzantine peers.
type op struct {
	kind opKind

	// a and b are the nodes involved.
	a, b int

	// drop, dup and delay are fault percentages for opFaults.
	drop, dup, delay int

	// d is how long opAdvance moves time forward.
	d time.Duration
}

// String describes the step, for failure reports.
func (o op) String() string {
	switch o.kind {
	case opConnect:
		return fmt.Sprintf("connect(%d,%d)", o.a, o.b)
	case opDisconnect:
		return fmt.Sprintf("disconnect(%d,%d)", o.a, o.b)
	case opFaults:
		return fmt.Sprintf("faults(%d->%d drop=%d dup=%d delay=%d)",
			o.a, o.b, o.drop, o.dup, o.delay)
	case opAdvance:
		return fmt.Sprintf("advance(%v)", o.d)
	case opFlood:
		return fmt.Sprintf("flood(%d)", o.a)
	}

	return "?"
}

// scenario is a drawn workload.
type scenario struct {
	seed      uint64
	numActive int
	seeds     []int
	byz       []byzMode
	ops       []op
}

// String describes the scenario, for failure reports.
func (s scenario) String() string {
	return fmt.Sprintf("seed=%d numActive=%d seeds=%v byz=%v ops=%v",
		s.seed, s.numActive, s.seeds, s.byz, s.ops)
}

// world is a set of simulated nodes and the links between them.
type world struct {
	t     *testing.T
	nodes []*simNode

	// links holds the link from node i to node j.
	links map[[2]int]*link

	// seeded is each node's graph before the workload ran.
	seeded [][]lnwire.ShortChannelID

	// completedWith lists the peers node 0 completed a historical sync
	// with. It is written from node 0's manager actor.
	completedMu   sync.Mutex
	completedWith []route.Vertex

	// faults are the faults of each directed pair. They stick to the
	// pair, so a reconnect keeps them until heal clears them.
	faults map[[2]int]linkFaults

	// dropped records the directed pairs whose faults ever dropped a
	// message. It is written from link senders.
	droppedMu sync.Mutex
	dropped   map[[2]int]bool
}

// countingFaults wraps f to record in the world when it drops a message.
func (w *world) countingFaults(pair [2]int, f linkFaults) linkFaults {
	return func(msg lnwire.Message) (linkAction, time.Duration) {
		action, d := f(msg)
		if action == dropMsg {
			w.droppedMu.Lock()
			w.dropped[pair] = true
			w.droppedMu.Unlock()
		}

		return action, d
	}
}

// numHonest returns how many nodes are honest.
func (w *world) honest(i int) bool {
	return w.nodes[i].byz == nil
}

// connected reports whether nodes i and j are connected.
func (w *world) connected(i, j int) bool {
	_, ok := w.links[[2]int{i, j}]
	return ok
}

// connect joins nodes i and j. Byzantine nodes only ever connect to node 0.
func (w *world) connect(i, j int) {
	if i == j || w.connected(i, j) {
		return
	}
	if !w.honest(i) || !w.honest(j) {
		if i != 0 && j != 0 {
			return
		}
	}

	mk := func(from, to int) *link {
		return &link{
			from: w.nodes[from], to: w.nodes[to],
			faults: w.faults[[2]int{from, to}],
			signal: make(chan struct{}, 1),
			sent:   make(map[string]int),
		}
	}
	ij, ji := mk(i, j), mk(j, i)

	for _, pair := range [][2]int{{i, j}, {j, i}} {
		from, to := w.nodes[pair[0]], w.nodes[pair[1]]
		out := w.linkFor(pair, ij, ji)
		if from.byz != nil {
			from.byz.mu.Lock()
			from.byz.out = out
			from.byz.mu.Unlock()

			continue
		}

		err := from.mgr.InitSyncState(w.t.Context(), out)
		require.NoError(w.t, err, "connect %v to %v", from.pub, to.pub)
	}

	w.links[[2]int{i, j}], w.links[[2]int{j, i}] = ij, ji
	for _, l := range []*link{ij, ji} {
		go l.run(w.t.Context())
		w.t.Cleanup(l.close)
	}

	// A flooding peer starts as soon as it connects.
	for _, n := range []*simNode{w.nodes[i], w.nodes[j]} {
		if n.byz != nil && n.byz.mode == byzFlood {
			n.byz.flood(100)
		}
	}
}

// linkFor returns the link that carries messages for pair.
func (w *world) linkFor(pair [2]int, ij, ji *link) *link {
	if ij.from == w.nodes[pair[0]] {
		return ij
	}

	return ji
}

// disconnect tears down the connection between nodes i and j.
func (w *world) disconnect(i, j int) {
	if !w.connected(i, j) {
		return
	}

	for _, pair := range [][2]int{{i, j}, {j, i}} {
		w.links[pair].close()
		delete(w.links, pair)

		from, to := w.nodes[pair[0]], w.nodes[pair[1]]
		if from.byz != nil {
			continue
		}
		from.mgr.PruneSyncState(to.pub)
	}
}

// apply runs one workload step.
func (w *world) apply(o op) {
	switch o.kind {
	case opConnect:
		w.connect(o.a, o.b)

	case opDisconnect:
		w.disconnect(o.a, o.b)

	case opFaults:
		pair := [2]int{o.a, o.b}
		w.faults[pair] = w.countingFaults(pair, seededFaults(
			uint64(o.a*31+o.b), o.drop, o.dup, o.delay,
		))
		if l, ok := w.links[pair]; ok {
			l.setFaults(w.faults[pair])
		}

	case opAdvance:
		settle(o.d, 4)

	case opFlood:
		if b := w.nodes[o.a].byz; b != nil && b.mode == byzFlood {
			b.flood(200)
		}
	}
}

// heal clears every fault and connects node 0 to every other node, then
// lets the world run long enough for every timer to have fired many times.
func (w *world) heal() {
	clear(w.faults)
	for _, l := range w.links {
		l.setFaults(nil)
	}
	for j := 1; j < len(w.nodes); j++ {
		w.connect(0, j)
	}

	settle(6*time.Hour, 72)
}

// wantsFrom reports whether honest node j currently has node 0 as an active
// or pinned syncer, which means node 0 forwards it live gossip.
func (w *world) wantsFrom(j int) bool {
	t, ok := w.nodes[j].mgr.SyncType(w.t.Context(), w.nodes[0].pub)
	return ok && t.wantsGossip()
}

// checkLiveGossip has node 0 announce a fresh channel, and returns the
// honest neighbors that had node 0 as an active syncer the whole time but
// never received the announcement. A batch dropped for a full responder
// mailbox is the documented behavior for a backed up peer, so a run with a
// drop is not held to this. Neither is a neighbor whose link to node 0 ever
// dropped a message, since the lost message may be the very filter that asks
// node 0 for live gossip.
func (w *world) checkLiveGossip() []string {
	var want []int
	for j := 1; j < len(w.nodes); j++ {
		w.droppedMu.Lock()
		dropped := w.dropped[[2]int{j, 0}]
		w.droppedMu.Unlock()

		if w.honest(j) && !dropped && w.connected(0, j) &&
			w.wantsFrom(j) {

			want = append(want, j)
		}
	}

	scid := lnwire.ShortChannelID{
		BlockHeight: simBestHeight - 1, TxIndex: 9_999_999,
	}
	drops := w.nodes[0].mgr.ForwardDrops()
	w.nodes[0].announce(w.t.Context(), scid)
	settle(30*time.Second, 3)

	if w.nodes[0].mgr.ForwardDrops() != drops {
		return nil
	}

	var violations []string
	for _, j := range want {
		if !w.wantsFrom(j) {
			continue
		}

		w.nodes[j].graph.mu.Lock()
		_, ok := w.nodes[j].graph.chans[scid]
		w.nodes[j].graph.mu.Unlock()
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"node %d has node 0 as an active syncer but "+
					"never got its new channel", j,
			))
		}
	}

	return violations
}

// check returns every oracle the final state violates.
func (w *world) check(sc scenario) []string {
	var violations []string
	fail := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	}

	// Liveness: every honest node with an honest neighbor has finished a
	// historical sync.
	for i, n := range w.nodes {
		if !w.honest(i) || sc.numActive == 0 {
			continue
		}

		hasHonestPeer := false
		for j := range w.nodes {
			if j != i && w.honest(j) && w.connected(i, j) {
				hasHonestPeer = true
			}
		}
		if hasHonestPeer && !n.mgr.IsGraphSynced() {
			fail("node %d has an honest peer but never synced", i)
		}
	}

	// Convergence: every historical sync node 0 completed with an honest
	// peer left it with at least every channel that peer started with.
	// A sync with a peer whose graph is empty, or that lies, legitimately
	// completes without that, and later ticks pick peers at random, so no
	// stronger claim holds within a bounded time.
	//
	// Peers whose link to node 0 ever dropped a message are exempt: an
	// SCID query is answered by a stream with no acknowledgements, so a
	// dropped announcement is simply missing. A real connection never
	// drops a message without closing, which the workload models with
	// disconnects; mid stream drops only model peer misbehavior.
	have := make(map[lnwire.ShortChannelID]struct{})
	for _, scid := range w.nodes[0].graph.scids() {
		have[scid] = struct{}{}
	}
	w.completedMu.Lock()
	completed := slices.Clone(w.completedWith)
	w.completedMu.Unlock()
	for _, pub := range completed {
		j := int(pub[0]) - 1
		w.droppedMu.Lock()
		dropped := w.dropped[[2]int{j, 0}]
		w.droppedMu.Unlock()
		if !w.honest(j) || dropped {
			continue
		}

		missing := 0
		for _, scid := range w.seeded[j] {
			if _, ok := have[scid]; !ok {
				missing++
			}
		}
		if missing > 0 {
			fail("node 0 completed a sync with node %d but lacks "+
				"%d of its %d channels", j, missing,
				len(w.seeded[j]))
		}
	}

	return violations
}

// drawScenario draws a workload.
func drawScenario(t *rapid.T) scenario {
	numHonest := rapid.IntRange(2, 4).Draw(t, "numHonest")
	numByz := rapid.IntRange(0, 2).Draw(t, "numByz")

	sc := scenario{
		seed:      rapid.Uint64().Draw(t, "seed"),
		numActive: rapid.IntRange(0, 3).Draw(t, "numActive"),
	}

	// Node 0 starts empty, and every other honest node with its own
	// random graph.
	sc.seeds = append(sc.seeds, 0)
	for range numHonest - 1 {
		sc.seeds = append(sc.seeds,
			rapid.IntRange(0, 200).Draw(t, "channels"))
	}
	for range numByz {
		sc.byz = append(sc.byz, byzMode(rapid.IntRange(
			0, int(numByzModes)-1,
		).Draw(t, "byzMode")))
	}

	n := numHonest + numByz
	nodeIdx := rapid.IntRange(0, n-1)
	numOps := rapid.IntRange(1, 30).Draw(t, "numOps")
	for range numOps {
		o := op{kind: opKind(rapid.IntRange(0, 4).Draw(t, "op"))}
		o.a = nodeIdx.Draw(t, "a")
		o.b = nodeIdx.Draw(t, "b")

		// Most of the action involves the node under test.
		if rapid.Bool().Draw(t, "aroundNode0") {
			if rapid.Bool().Draw(t, "node0First") {
				o.a = 0
			} else {
				o.b = 0
			}
		}

		switch o.kind {
		case opFaults:
			// Half the fault scripts drop nothing, since a
			// dropping link exempts its peer from the
			// convergence and live gossip oracles.
			if rapid.Bool().Draw(t, "drops") {
				o.drop = rapid.IntRange(1, 30).Draw(t, "drop")
			}
			o.dup = rapid.IntRange(0, 20).Draw(t, "dup")
			o.delay = rapid.IntRange(0, 30).Draw(t, "delay")

		case opAdvance:
			o.d = time.Duration(rapid.IntRange(
				1, 180,
			).Draw(t, "minutes")) * time.Minute
		}

		sc.ops = append(sc.ops, o)
	}

	return sc
}

// runScenario runs a workload in a fresh bubble and returns the oracles it
// violated. It reports violations instead of failing the test, so that the
// caller can fail the rapid run, which lets rapid shrink the scenario.
func runScenario(t *testing.T, sc scenario) []string {
	var violations []string

	synctest.Test(t, func(t *testing.T) {
		w := &world{
			t:       t,
			links:   make(map[[2]int]*link),
			faults:  make(map[[2]int]linkFaults),
			dropped: make(map[[2]int]bool),
		}

		limits := RangeLimits{
			MaxReplies:       50,
			MaxSCIDs:         2_000,
			FreshnessHorizon: DefaultFreshnessHorizon,
		}
		cfg := simConfig{
			numActive: sc.numActive,
			seed:      sc.seed,
			limits:    limits,
		}

		rng := rand.New(rand.NewPCG(sc.seed, sc.seed))
		for i, count := range sc.seeds {
			nodeCfg := cfg
			if i == 0 {
				nodeCfg.onOutcome = func(peer route.Vertex,
					o SyncOutcome) {

					if o.Kind != OutcomeCompleted {
						return
					}
					w.completedMu.Lock()
					w.completedWith = append(
						w.completedWith, peer,
					)
					w.completedMu.Unlock()
				}
			}
			n := newSimNode(t, byte(i+1), nodeCfg)
			seedGraph(n, rng, count)
			w.nodes = append(w.nodes, n)
			w.seeded = append(w.seeded, n.graph.scids())
		}
		for i, mode := range sc.byz {
			id := byte(len(sc.seeds) + i + 1)
			w.nodes = append(w.nodes, &simNode{
				pub:   route.Vertex{id},
				graph: newSimGraph(),
				byz:   &byzantine{mode: mode, limits: limits},
			})
			w.seeded = append(w.seeded, nil)
		}

		for _, o := range sc.ops {
			w.apply(o)
		}
		w.heal()

		violations = w.check(sc)
		violations = append(violations, w.checkLiveGossip()...)
	})

	return violations
}

// workloadProperty returns the simulation property: draw a scenario, run it
// in a fresh bubble under t, and fail the rapid run if any oracle broke.
func workloadProperty(t *testing.T) func(*rapid.T) {
	return func(rt *rapid.T) {
		sc := drawScenario(rt)
		if v := runScenario(t, sc); len(v) > 0 {
			rt.Fatalf("scenario %v violated: %v", sc, v)
		}
	}
}

// TestDSTWorkload runs random workloads against complete nodes in synctest
// bubbles: random topologies, connects and disconnects, link faults, time
// passing across every timer, and byzantine peers. After the network heals,
// it checks liveness, convergence and live forwarding.
func TestDSTWorkload(t *testing.T) {
	t.Parallel()

	rapid.Check(t, workloadProperty(t))
}

// FuzzDSTWorkload runs the same property under Go's fuzzer, which evolves
// the bytes rapid draws scenarios from toward new coverage.
func FuzzDSTWorkload(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		rapid.MakeFuzz(workloadProperty(t))(t, data)
	})
}
