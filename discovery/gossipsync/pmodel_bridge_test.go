package gossipsync

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// This file replays executions of the P manager contract (pmodel/src/
// manager.p) into the real ManagerState.
//
// The bridge is decision-aligned rather than lockstep. For every input, the
// model records each choice it made (the session it chose and the set it was
// allowed to choose from) and each forced action, then an observable
// snapshot. The bridge feeds the same input to the Go manager with its random
// source steered so that Go picks the same session from Go's own candidate
// list, and then compares only the observable snapshot, never the order of
// the Go outbox.
//
// Go's candidate list for a pick is discovered by exercising Go itself:
// ProcessEvent is pure, so the bridge reruns the transition once per index
// the pick could return and reads which session each index lands on. A pick
// whose candidate set differs from the model's allowed set is a real
// disagreement, and the bridge fails naming both sets.

// pmodelTraceDirs returns the directories holding model traces: the
// checked-in corpus, plus a fresh recording from pmodel/check.sh when
// GOSSIPSYNC_PMODEL_TRACES points at one.
func pmodelTraceDirs() []string {
	dirs := []string{filepath.Join("pmodel", "traces")}
	if dir := os.Getenv("GOSSIPSYNC_PMODEL_TRACES"); dir != "" {
		dirs = append(dirs, dir)
	}

	return dirs
}

// pmodelTraceFiles returns every trace file whose name starts with prefix.
func pmodelTraceFiles(t *testing.T, prefix string) []string {
	t.Helper()

	var files []string
	for _, dir := range pmodelTraceDirs() {
		found, err := filepath.Glob(filepath.Join(
			dir, prefix+"*.trace",
		))
		require.NoError(t, err)
		files = append(files, found...)
	}

	return files
}

// readTraceLines returns the non-empty lines of a trace file.
func readTraceLines(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	require.NoError(t, scanner.Err())

	return lines
}

// traceFields parses the "k=v" pairs of a trace line.
func traceFields(t *testing.T, s string) map[string]string {
	t.Helper()

	out := make(map[string]string)
	for _, kv := range strings.Fields(s) {
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok, "bad trace field %q", kv)
		out[k] = v
	}

	return out
}

// traceInt parses an integer trace field.
func traceInt(t *testing.T, f map[string]string, key string) int {
	t.Helper()

	v, ok := f[key]
	require.True(t, ok, "missing trace field %q", key)
	n, err := strconv.Atoi(v)
	require.NoError(t, err, "bad trace field %s=%q", key, v)

	return n
}

// traceInts parses a comma-separated list, where "-" is the empty list.
func traceInts(t *testing.T, v string) []int {
	t.Helper()

	if v == "-" || v == "" {
		return nil
	}

	var out []int
	for _, s := range strings.Split(v, ",") {
		n, err := strconv.Atoi(s)
		require.NoError(t, err, "bad trace list %q", v)
		out = append(out, n)
	}

	return out
}

// joinInts renders a list the way the models print it.
func joinInts[T ~uint64 | ~int](xs []T) string {
	if len(xs) == 0 {
		return "-"
	}

	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.FormatUint(uint64(x), 10)
	}

	return strings.Join(parts, ",")
}

// mDecision is a choice or a forced action of the model's manager.
type mDecision struct {
	// kind is "start", "promote" or "demote".
	kind string

	// session is the session the model chose, or was forced to act on.
	session SessionID

	// from is the set the model chose from, empty if forced.
	from []SessionID

	// forced is set for an action the contract requires with no choice.
	forced bool
}

// mStep is one input the model's manager handled.
type mStep struct {
	input     string
	decisions []mDecision
	snap      string
}

// mTrace is one execution of the manager model.
type mTrace struct {
	numActive int
	steps     []mStep
}

// parseManagerTraces reads the manager traces in a file.
func parseManagerTraces(t *testing.T, path string) []mTrace {
	t.Helper()

	var (
		traces []mTrace
		cur    *mTrace
	)
	for _, line := range readTraceLines(t, path) {
		kind, rest, _ := strings.Cut(line, " ")
		if kind != "begin" {
			require.NotNil(t, cur, "line before begin: %q", line)
		}

		var step *mStep
		if cur != nil && len(cur.steps) > 0 {
			step = &cur.steps[len(cur.steps)-1]
		}

		switch kind {
		case "begin":
			f := traceFields(t, rest)
			traces = append(traces, mTrace{
				numActive: traceInt(t, f, "num_active"),
			})
			cur = &traces[len(traces)-1]

		case "in":
			cur.steps = append(cur.steps, mStep{input: rest})

		case "choice", "force":
			require.NotNil(t, step, "decision before input")
			f := traceFields(t, rest)
			d := mDecision{
				kind:    f["kind"],
				session: SessionID(traceInt(t, f, "session")),
				forced:  kind == "force",
			}
			for _, s := range traceInts(t, f["from"]) {
				d.from = append(d.from, SessionID(s))
			}
			step.decisions = append(step.decisions, d)

		case "snap":
			require.NotNil(t, step, "snapshot before input")
			step.snap = rest

		default:
			t.Fatalf("unknown manager trace line %q", line)
		}
	}

	// A run cut off by the checker's step bound can end on an input
	// whose snapshot was never printed. There is nothing to compare for
	// it, so it is dropped.
	for i := range traces {
		steps := traces[i].steps
		if n := len(steps); n > 0 && steps[n-1].snap == "" {
			traces[i].steps = steps[:n-1]
		}
	}

	return traces
}

// managerEvent converts a trace input into the Go manager event.
func managerEvent(t *testing.T, input string) ManagerEvent {
	t.Helper()

	name, rest, _ := strings.Cut(input, " ")
	f := traceFields(t, rest)
	pub := func() route.Vertex {
		return route.Vertex{byte(traceInt(t, f, "pub"))}
	}

	switch name {
	case "connect":
		return &PeerConnected{
			Peer: pub(), Pinned: traceInt(t, f, "pinned") == 1,
		}

	case "disconnect":
		return &PeerDisconnected{Peer: pub()}

	case "hist_tick":
		return &HistoricalTick{}

	case "rotate":
		return &RotateTick{}

	case "outcome":
		return &HistoricalSyncOutcome{
			Session: SessionID(traceInt(t, f, "session")),
			Attempt: AttemptID(traceInt(t, f, "attempt")),
			Outcome: SyncOutcome{
				Kind: OutcomeKind(traceInt(t, f, "kind")),
			},
		}
	}

	t.Fatalf("unknown manager trace input %q", input)

	return nil
}

// pickEffects returns, in outbox order, the sessions the outbox acts on for
// one kind of decision: "start" for a StartHistoricalSync, "promote" for a
// move to ActiveSync, and "demote" for a move to PassiveSync.
func pickEffects(out []ManagerOutbox, kind string) []SessionID {
	var sessions []SessionID
	for _, o := range out {
		tell, ok := o.(*TellSyncer)
		if !ok {
			continue
		}

		var k string
		switch e := tell.Event.(type) {
		case *StartHistoricalSync:
			k = "start"

		case *SetSyncType:
			switch e.Type {
			case ActiveSync:
				k = "promote"
			case PassiveSync:
				k = "demote"
			}
		}

		if k == kind {
			sessions = append(sessions, tell.Session)
		}
	}

	return sessions
}

// renderManagerSnap renders an observable manager snapshot the way the model
// prints it.
func renderManagerSnap(roles map[SessionID]SyncType, synced bool,
	tracked SessionID, inFlight []SessionID, started []AttemptID,
	reset, publish bool) string {

	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}

	sessions := slices.Sorted(func(yield func(SessionID) bool) {
		for s := range roles {
			if !yield(s) {
				return
			}
		}
	})
	var parts []string
	for _, s := range sessions {
		letter := "P"
		switch roles[s] {
		case ActiveSync:
			letter = "A"
		case PinnedSync:
			letter = "X"
		}
		parts = append(parts, fmt.Sprintf("%d:%s", s, letter))
	}
	rolesStr := "-"
	if len(parts) > 0 {
		rolesStr = strings.Join(parts, ",")
	}

	inFlight = slices.Sorted(slices.Values(inFlight))

	return fmt.Sprintf("roles=%s synced=%d tracked=%d inflight=%s "+
		"started=%s reset=%d publish=%d", rolesStr, b(synced), tracked,
		joinInts(inFlight), joinInts(started), b(reset), b(publish))
}

// goManagerSnap projects the Go manager's state, and the outbox of the
// transition that produced it, onto the observable snapshot.
func goManagerSnap(m *ManagerState, out []ManagerOutbox) string {
	roles := make(map[SessionID]SyncType, len(m.members))
	for s, mem := range m.members {
		roles[s] = mem.role
	}

	var inFlight []SessionID
	for _, a := range m.inflight {
		inFlight = append(inFlight, a.session)
	}

	// A tracked attempt that is not in flight is rendered as an
	// impossible session, so the comparison fails on it.
	var tracked SessionID
	m.tracked.WhenSome(func(a AttemptID) {
		info, ok := m.inflight[a]
		tracked = info.session
		if !ok {
			tracked = ^SessionID(0)
		}
	})

	var (
		started        []AttemptID
		reset, publish bool
	)
	for _, o := range out {
		switch o := o.(type) {
		case *TellSyncer:
			if st, ok := o.Event.(*StartHistoricalSync); ok {
				started = append(started, st.Attempt)
			}

		case *ResetHistoricalTimer:
			reset = true

		case *PublishGraphSynced:
			publish = true
		}
	}

	return renderManagerSnap(
		roles, m.graphSynced, tracked, inFlight, started, reset,
		publish,
	)
}

// bridgeT is the part of testing.T and rapid.T the steering helpers use.
type bridgeT interface {
	require.TestingT
	Helper()
	Fatalf(format string, args ...any)
}

// steeredRun applies one event to the Go manager, with the i-th random pick
// returning picks[i], and every pick past the end of picks returning zero.
// It returns the candidate count Go asked for at every pick.
func steeredRun(t bridgeT, state ManagerMachineState, ev ManagerEvent,
	numActive int, picks []int) (*ManagerState, []ManagerOutbox, []int) {

	t.Helper()

	var calls []int
	env := &ManagerEnv{
		NumActiveSyncers: numActive,
		Rand: func(n int) int {
			k := len(calls)
			calls = append(calls, n)
			if k < len(picks) {
				return picks[k]
			}

			return 0
		},
	}

	next, out, err := protofsm.ApplyEvents(
		context.Background(), state, ev, env,
	)
	require.NoError(t, err)

	return next.(*ManagerState), out, calls
}

// goCandidates discovers the Go manager's candidate list for its k-th random
// pick, given the indices of the picks before it. Decision d is the model's
// counterpart, and ordinal is how many decisions of the same kind precede it
// in the step, which is where its effect sits among that kind's effects.
func goCandidates(t bridgeT, where string, state ManagerMachineState,
	ev ManagerEvent, numActive int, prefix []int, k int, d mDecision,
	ordinal int) []SessionID {

	t.Helper()

	_, _, calls := steeredRun(t, state, ev, numActive, prefix)
	if len(calls) <= k {
		t.Fatalf("%s: the model chose a %s from %v, but Go made only "+
			"%d random picks", where, d.kind, d.from, len(calls))
	}

	n := calls[k]
	list := make([]SessionID, n)
	for i := range n {
		picks := append(slices.Clone(prefix), i)
		_, out, _ := steeredRun(t, state, ev, numActive, picks)

		effects := pickEffects(out, d.kind)
		if ordinal >= len(effects) {
			t.Fatalf("%s: Go's pick %d at index %d produced no %s "+
				"effect", where, k, i, d.kind)
		}
		list[i] = effects[ordinal]
	}

	return list
}

// replayManagerTrace replays one model execution into the Go manager.
func replayManagerTrace(t *testing.T, name string, trace mTrace) {
	t.Helper()

	var state ManagerMachineState = NewManagerState()
	for i, step := range trace.steps {
		where := fmt.Sprintf("%s step %d (%s)", name, i, step.input)
		ev := managerEvent(t, step.input)

		// Resolve each of the model's choices to an index into Go's
		// own candidate list.
		var (
			picks   []int
			perKind = make(map[string]int)
		)
		for _, d := range step.decisions {
			ordinal := perKind[d.kind]
			perKind[d.kind]++
			if d.forced {
				continue
			}

			list := goCandidates(
				t, where, state, ev, trace.numActive, picks,
				len(picks), d, ordinal,
			)

			goSet := slices.Sorted(slices.Values(list))
			if !slices.Equal(goSet, d.from) {
				t.Fatalf("%s: %s pick %d: Go's candidates are "+
					"%v, the model's allowed set is %v",
					where, d.kind, len(picks), goSet,
					d.from)
			}

			picks = append(picks, slices.Index(list, d.session))
		}

		next, out, calls := steeredRun(
			t, state, ev, trace.numActive, picks,
		)
		require.Len(t, calls, len(picks), "%s: Go made %d random "+
			"picks, the model made %d choices", where, len(calls),
			len(picks))

		// Every action, chosen or forced, lands on the same session,
		// in the same order within its kind.
		for _, kind := range []string{"start", "promote", "demote"} {
			var want []SessionID
			for _, d := range step.decisions {
				if d.kind == kind {
					want = append(want, d.session)
				}
			}
			require.Equal(t, want, pickEffects(out, kind),
				"%s: %s actions differ", where, kind)
		}

		require.Equal(t, step.snap, goManagerSnap(next, out),
			"%s: observable snapshots differ", where)

		state = next
	}
}

// TestPModelManagerBridge replays executions of the P manager contract into
// the Go manager, steering Go to the model's choices, and requires the same
// observable snapshot after every input.
func TestPModelManagerBridge(t *testing.T) {
	t.Parallel()

	total := 0
	for _, file := range pmodelTraceFiles(t, "manager_") {
		traces := parseManagerTraces(t, file)
		require.NotEmpty(t, traces, "no executions in %s", file)
		for i, trace := range traces {
			name := fmt.Sprintf("%s#%d", filepath.Base(file), i)
			replayManagerTrace(t, name, trace)
			total++
		}
	}

	require.Positive(t, total, "no manager model traces found")
	t.Logf("replayed %d manager model executions", total)
}
