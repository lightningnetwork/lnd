package gossipsync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/stretchr/testify/require"
)

// This file replays executions of the P syncer contract (pmodel/src/
// syncer.p) into the real peer syncer, NewIdle driven by
// protofsm.ApplyEvents.
//
// The model's chain has pmodelBlocks abstract blocks, and channel c lives
// in block c - 1. The bridge maps abstract block b to the heights
// [b*pmodelBlockSpan, (b+1)*pmodelBlockSpan - 1], so the whole chain is the
// query the syncer sends at a best height of pmodelBlocks*pmodelBlockSpan. A
// reply covering blocks [f, l] becomes a reply_channel_range with
// FirstBlockHeight f*pmodelBlockSpan and NumBlocks (l-f+1)*pmodelBlockSpan,
// carrying one SCID per channel. A corrupted reply has its first height
// shifted by one, which is what the byzantine peer in syncer_test.go does. A
// legacy reply covers blocks [0, pmodelBlocks-1], which echoes the query
// exactly as an old lnd node did.
//
// After every input, the bridge compares the projected outbox: outcomes,
// queries sent with the SCIDs they ask for, timestamp filters, and timer
// arms and disarms, as a multiset. The order within one transition's outbox
// is not compared.

const (
	// pmodelBlocks is the number of blocks in the model's chain.
	pmodelBlocks = 4

	// pmodelBlockSpan is the number of heights one abstract block covers.
	pmodelBlockSpan = 500
)

// sStep is one input the model's syncer handled, and its outbox.
type sStep struct {
	input string
	out   []string
}

// sTrace is one execution of the syncer model.
type sTrace struct {
	chans      []int
	known      []int
	batch      int
	maxReplies int
	lookupErr  bool
	steps      []sStep
}

// parseSyncerTraces reads the syncer traces in a file.
func parseSyncerTraces(t *testing.T, path string) []sTrace {
	t.Helper()

	var (
		traces []sTrace
		cur    *sTrace
	)
	for _, line := range readTraceLines(t, path) {
		kind, rest, _ := strings.Cut(line, " ")
		if kind != "begin" {
			require.NotNil(t, cur, "line before begin: %q", line)
		}

		switch kind {
		case "begin":
			f := traceFields(t, rest)
			traces = append(traces, sTrace{
				chans:      traceInts(t, f["chans"]),
				known:      traceInts(t, f["known"]),
				batch:      traceInt(t, f, "batch"),
				maxReplies: traceInt(t, f, "max_replies"),
				lookupErr:  traceInt(t, f, "lookup_err") == 1,
			})
			cur = &traces[len(traces)-1]

		case "in":
			cur.steps = append(cur.steps, sStep{input: rest})

		case "out":
			require.NotEmpty(t, cur.steps, "output before input")
			step := &cur.steps[len(cur.steps)-1]
			step.out = append(step.out, rest)

		default:
			t.Fatalf("unknown syncer trace line %q", line)
		}
	}

	return traces
}

// pmodelSCID returns the SCID of the model's channel c.
func pmodelSCID(c int) lnwire.ShortChannelID {
	return lnwire.ShortChannelID{
		BlockHeight: uint32((c-1)*pmodelBlockSpan + 7),
		TxIndex:     uint32(c),
	}
}

// syncerEvent converts a trace input into the Go syncer event.
func syncerEvent(t *testing.T, input string) SyncerEvent {
	t.Helper()

	name, rest, _ := strings.Cut(input, " ")
	f := traceFields(t, rest)

	switch name {
	case "start":
		return &StartHistoricalSync{
			Attempt: AttemptID(traceInt(t, f, "attempt")),
		}

	case "role":
		return &SetSyncType{Type: SyncType(traceInt(t, f, "role"))}

	case "timer":
		return &ReplyTimerFired{Seq: uint64(traceInt(t, f, "seq"))}

	case "end":
		return &SCIDsEndReceived{End: &lnwire.ReplyShortChanIDsEnd{
			ChainHash: testChain,
			Complete:  1,
		}}

	case "range":
		first := traceInt(t, f, "first")
		last := traceInt(t, f, "last")
		reply := &lnwire.ReplyChannelRange{
			ChainHash: testChain,
			FirstBlockHeight: uint32(
				first * pmodelBlockSpan,
			),
			NumBlocks: uint32(
				(last - first + 1) * pmodelBlockSpan,
			),
			Complete:     uint8(traceInt(t, f, "complete")),
			EncodingType: lnwire.EncodingSortedPlain,
		}
		for _, c := range traceInts(t, f["chans"]) {
			reply.ShortChanIDs = append(
				reply.ShortChanIDs, pmodelSCID(c),
			)
		}
		if traceInt(t, f, "bad") == 1 {
			reply.FirstBlockHeight++
		}

		return &RangeReplyReceived{Reply: reply}
	}

	t.Fatalf("unknown syncer trace input %q", input)

	return nil
}

// renderSyncerOutbox projects a Go syncer outbox the way the model prints
// it.
func renderSyncerOutbox(t *testing.T, env *SyncerEnv,
	out []SyncerOutbox) []string {

	t.Helper()

	var got []string
	for _, o := range out {
		switch o := o.(type) {
		case *SendToPeer:
			for _, msg := range o.Msgs {
				got = append(got, renderSyncerSend(t, msg))
			}

		case *ReportOutcome:
			got = append(got, fmt.Sprintf(
				"outcome attempt=%d kind=%d", o.Attempt,
				o.Outcome.Kind,
			))

		case *ArmReplyTimer:
			drain := 0
			if o.After == env.DrainTimeout {
				drain = 1
			}
			got = append(got, fmt.Sprintf("arm seq=%d drain=%d",
				o.Seq, drain))

		case *DisarmReplyTimer:
			got = append(got, "disarm")

		default:
			t.Fatalf("unknown syncer outbox %T", o)
		}
	}

	return got
}

// renderSyncerSend renders a message the syncer sends to the peer.
func renderSyncerSend(t *testing.T, msg lnwire.Message) string {
	t.Helper()

	switch m := msg.(type) {
	case *lnwire.QueryChannelRange:
		require.EqualValues(t, 0, m.FirstBlockHeight)
		require.EqualValues(t, pmodelBlocks*pmodelBlockSpan,
			m.NumBlocks)

		return "send_range"

	case *lnwire.QueryShortChanIDs:
		ids := make([]int, len(m.ShortChanIDs))
		for i, scid := range m.ShortChanIDs {
			require.Equal(t, pmodelSCID(int(scid.TxIndex)), scid)
			ids[i] = int(scid.TxIndex)
		}

		return "send_scids scids=" + joinInts(ids)

	case *lnwire.GossipTimestampRange:
		wants := 0
		if m.TimestampRange != 0 {
			wants = 1
		}

		return fmt.Sprintf("filter wants=%d", wants)
	}

	t.Fatalf("unknown message %T", msg)

	return ""
}

// replaySyncerTrace replays one model execution into the Go syncer.
func replaySyncerTrace(t *testing.T, name string, trace sTrace) {
	t.Helper()

	graph := &fakeKnown{known: make(map[lnwire.ShortChannelID]struct{})}
	for _, c := range trace.known {
		graph.known[pmodelSCID(c)] = struct{}{}
	}
	if trace.lookupErr {
		graph.err = errors.New("lookup failed")
	}

	env := testSyncerEnv(graph, trace.batch, true)
	env.BestHeight = func() uint32 {
		return pmodelBlocks * pmodelBlockSpan
	}
	env.Limits.MaxReplies = uint32(trace.maxReplies)

	// The drain and reply timeouts default to the same duration, and the
	// projection tells a drain arm from a reply arm by its duration, so
	// the drain timer gets a distinct one here.
	env.DrainTimeout = env.ReplyTimeout + time.Second

	var state SyncerState = NewIdle()
	for i, step := range trace.steps {
		next, out, err := protofsm.ApplyEvents(
			context.Background(), state,
			syncerEvent(t, step.input), env,
		)
		require.NoError(t, err)
		state = next

		want := slices.Sorted(slices.Values(step.out))
		got := slices.Sorted(slices.Values(
			renderSyncerOutbox(t, env, out),
		))
		require.Equal(t, want, got, "%s step %d (%s): outboxes "+
			"differ, now in %v", name, i, step.input, state)
	}
}

// TestPModelSyncerBridge replays executions of the P syncer contract into
// the Go syncer, and requires the same projected outbox after every input.
func TestPModelSyncerBridge(t *testing.T) {
	t.Parallel()

	total := 0
	for _, file := range pmodelTraceFiles(t, "syncer_") {
		traces := parseSyncerTraces(t, file)
		require.NotEmpty(t, traces, "no executions in %s", file)
		for i, trace := range traces {
			name := fmt.Sprintf("%s#%d", filepath.Base(file), i)
			replaySyncerTrace(t, name, trace)
			total++
		}
	}

	require.Positive(t, total, "no syncer model traces found")
	t.Logf("replayed %d syncer model executions", total)
}
