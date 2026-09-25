package gossipsync

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// testChain is the chain hash used by syncer tests.
var testChain = chainhash.Hash{9}

// fakeKnown is a KnownChannels backed by a set of SCIDs we already know.
type fakeKnown struct {
	known map[lnwire.ShortChannelID]struct{}
	err   error
}

// FilterKnownChanIDs returns the SCIDs in superSet we don't know.
func (f *fakeKnown) FilterKnownChanIDs(_ chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	_ func(graphdb.ChannelUpdateInfo) bool) ([]lnwire.ShortChannelID,
	error) {

	if f.err != nil {
		return nil, f.err
	}

	var missing []lnwire.ShortChannelID
	for _, info := range superSet {
		if _, ok := f.known[info.ShortChannelID]; !ok {
			missing = append(missing, info.ShortChannelID)
		}
	}

	return missing, nil
}

// notZombie reports that no channel should stay a zombie.
func notZombie(graphdb.ChannelUpdateInfo) bool {
	return false
}

// testSyncerEnv returns an environment for pure syncer tests.
func testSyncerEnv(graph KnownChannels, batch int,
	noTimestamps bool) *SyncerEnv {

	return &SyncerEnv{
		Peer:               route.Vertex{1},
		ChainHash:          testChain,
		Graph:              graph,
		IsStillZombie:      notZombie,
		BestHeight:         func() uint32 { return 2_000 },
		Now:                func() time.Time { return testNow },
		Limits:             DefaultRangeLimits(),
		BatchSize:          batch,
		ReplyTimeout:       DefaultReplyTimeout,
		DrainTimeout:       DefaultDrainTimeout,
		NoTimestampQueries: noTimestamps,
	}
}

// inbound is a message the model peer will send us, tagged with the query it
// answers.
type inbound struct {
	event   SyncerEvent
	answers lnwire.Message

	// dup is set on a copy injected by the fault model.
	dup bool
}

// modelPeer is an honest peer that answers every query it receives with the
// stream our own responder would produce.
type modelPeer struct {
	graph     []graphdb.BlockChannelRange
	chunkSize int
	inbox     []inbound

	// lastFilter is the last timestamp filter we sent it.
	lastFilter *lnwire.GossipTimestampRange
}

// receive handles a message the syncer sent to the peer.
func (m *modelPeer) receive(t require.TestingT, msg lnwire.Message) {
	switch q := msg.(type) {
	case *lnwire.QueryChannelRange:
		chunker := testChunker(q, m.chunkSize, 1)
		for reply := range chunker.replies(m.graph) {
			m.inbox = append(m.inbox, inbound{
				event:   &RangeReplyReceived{Reply: reply},
				answers: q,
			})
		}

	case *lnwire.QueryShortChanIDs:
		end := &lnwire.ReplyShortChanIDsEnd{
			ChainHash: q.ChainHash,
			Complete:  1,
		}
		m.inbox = append(m.inbox, inbound{
			event:   &SCIDsEndReceived{End: end},
			answers: q,
		})

	case *lnwire.GossipTimestampRange:
		m.lastFilter = q

	default:
		require.Failf(t, "unexpected message", "%T", msg)
	}
}

// syncerHarness drives a pure peer syncer against a model peer, recording
// everything needed to check the invariants.
type syncerHarness struct {
	t     *rapid.T
	env   *SyncerEnv
	state SyncerState
	peer  *modelPeer

	nextAttempt AttemptID
	started     []AttemptID
	outcomes    map[AttemptID][]SyncOutcome
	faulted     map[AttemptID]bool

	// queriedFor collects the SCIDs queried on behalf of each attempt.
	queriedFor map[AttemptID][]lnwire.ShortChannelID

	// queryAttempt maps each query we sent to the attempt it served.
	queryAttempt map[lnwire.Message]AttemptID

	// lastSCIDQuery is the last query_short_channel_ids we sent.
	lastSCIDQuery lnwire.Message

	// lastSeq is the sequence of the last timer we armed.
	lastSeq uint64

	// firstDesync is the attempt in flight, or about to start, when the
	// fault model first made the peer inconsistent, by duplicating a
	// message or corrupting a reply, or zero if it never has.
	//
	// The wire gives queries no IDs, so an inconsistent peer can put an
	// extra end of stream on the wire, which ends a later drain early,
	// after which its replies may be credited to a later attempt. No
	// design can recover the pairing from there, and the cost is bounded:
	// an attempt only ever sees this one peer's answers. The pairing and
	// completeness checks therefore cover everything before the first
	// inconsistency, and a lossy but honest peer (drops, stalls,
	// timeouts) stays fully checked for the whole run.
	firstDesync AttemptID
}

// attemptOf returns the attempt the current state is working on, if any.
func attemptOf(s SyncerState) (AttemptID, bool) {
	switch st := s.(type) {
	case *AwaitingRange:
		return st.attempt, true
	case *QueryingSCIDs:
		return st.attempt, true
	}

	return 0, false
}

// apply feeds one event to the syncer, checking table conformance and the
// per-transition invariants, then executes the outbox against the model.
func (h *syncerHarness) apply(ev SyncerEvent, src lnwire.Message) {
	t := h.t

	observe := func(ev SyncerEvent, from, to SyncerState,
		out []SyncerOutbox) {

		require.NoError(t, SyncerTransitions.CheckTransition(
			from, ev, to, out,
		))

		// No reply is ever counted toward an attempt other than the
		// one whose query it answers.
		switch st := from.(type) {
		case *AwaitingRange:
			_, isReply := ev.(*RangeReplyReceived)
			if isReply && src != nil {
				require.Same(t, st.acc.query, src,
					"range reply consumed by the wrong "+
						"attempt")
			}

		// A dropped end can't be told apart from a slow one, so after
		// a drop the pairing is off for the rest of that attempt.
		case *QueryingSCIDs:
			_, isEnd := ev.(*SCIDsEndReceived)
			if isEnd && src != nil && !h.faulted[st.attempt] {
				require.Equal(t, h.lastSCIDQuery, src,
					"scid end consumed by the wrong query")
			}
		}
	}

	next, out, err := protofsm.ApplyEventsObserved(
		context.Background(), h.state, ev, h.env, observe,
	)
	require.NoError(t, err)

	attempt, working := attemptOf(next)
	h.state = next

	for _, o := range out {
		switch o := o.(type) {
		case *SendToPeer:
			for _, msg := range o.Msgs {
				if working {
					h.queryAttempt[msg] = attempt
				}
				q, ok := msg.(*lnwire.QueryShortChanIDs)
				if ok {
					h.lastSCIDQuery = q
					h.queriedFor[attempt] = append(
						h.queriedFor[attempt],
						q.ShortChanIDs...,
					)
				}
				h.peer.receive(t, msg)
			}

		case *ReportOutcome:
			h.outcomes[o.Attempt] = append(
				h.outcomes[o.Attempt], o.Outcome,
			)

		case *ArmReplyTimer:
			require.Greater(t, o.Seq, h.lastSeq)
			h.lastSeq = o.Seq

		case *DisarmReplyTimer:
		}
	}
}

// deliver feeds a message from the peer to the syncer. A duplicate is
// delivered without its source, which exempts it from the source checks.
func (h *syncerHarness) deliver(in inbound) {
	src := in.answers
	if h.firstDesync != 0 {
		src = nil
	}
	h.apply(in.event, src)
}

// markDesync records the first point the peer became inconsistent.
func (h *syncerHarness) markDesync() {
	if h.firstDesync == 0 {
		h.firstDesync = max(h.nextAttempt, 1)
	}
}

// markFaulted records that the attempt answered by src saw a fault.
func (h *syncerHarness) markFaulted(src lnwire.Message) {
	if a, ok := h.queryAttempt[src]; ok {
		h.faulted[a] = true
	}
}

// step performs one randomly drawn action.
func (h *syncerHarness) step() {
	t := h.t
	inbox := &h.peer.inbox

	action := rapid.SampledFrom([]string{
		"start", "setType", "deliver", "deliver", "deliver",
		"deliver", "drop", "dup", "corrupt", "fireTimer",
		"fireStale",
	}).Draw(t, "action")

	switch action {
	case "start":
		h.nextAttempt++
		h.started = append(h.started, h.nextAttempt)
		h.apply(&StartHistoricalSync{Attempt: h.nextAttempt}, nil)

	case "setType":
		st := rapid.SampledFrom([]SyncType{
			PassiveSync, ActiveSync, PinnedSync,
		}).Draw(t, "syncType")
		h.apply(&SetSyncType{Type: st}, nil)

	case "deliver":
		if len(*inbox) == 0 {
			return
		}
		in := (*inbox)[0]
		*inbox = (*inbox)[1:]
		h.deliver(in)

	case "drop":
		if len(*inbox) == 0 {
			return
		}
		h.markFaulted((*inbox)[0].answers)
		*inbox = (*inbox)[1:]

	case "dup":
		if len(*inbox) == 0 {
			return
		}
		h.markFaulted((*inbox)[0].answers)
		copied := (*inbox)[0]
		copied.dup = true
		h.markDesync()
		*inbox = append([]inbound{copied}, *inbox...)

	case "corrupt":
		if len(*inbox) == 0 {
			return
		}
		rr, ok := (*inbox)[0].event.(*RangeReplyReceived)
		if !ok {
			return
		}
		bad := *rr.Reply
		bad.FirstBlockHeight += uint32(rapid.IntRange(1, 5).Draw(
			t, "shift",
		))
		h.markFaulted((*inbox)[0].answers)
		h.markDesync()
		(*inbox)[0].event = &RangeReplyReceived{Reply: &bad}

	case "fireTimer":
		if a, ok := attemptOf(h.state); ok {
			h.faulted[a] = true
		}
		h.fireCurrent()

	case "fireStale":
		if h.lastSeq > 1 {
			h.apply(&ReplyTimerFired{Seq: h.lastSeq - 1}, nil)
		}
	}
}

// fireCurrent fires the current timer. When that ends a drain, the peer
// is modeled as having gone silent for the abandoned exchange, so we drop
// whatever it still owed us.
func (h *syncerHarness) fireCurrent() {
	_, draining := h.state.(*Draining)
	h.apply(&ReplyTimerFired{Seq: h.lastSeq}, nil)

	if _, idle := h.state.(*Idle); draining && idle {
		h.peer.inbox = nil
	}
}

// settle delivers everything the peer owes and fires timers until the
// syncer is idle with nothing left in flight.
func (h *syncerHarness) settle() {
	for range 10_000 {
		_, idle := h.state.(*Idle)
		if idle && len(h.peer.inbox) == 0 {
			return
		}

		if len(h.peer.inbox) > 0 {
			in := h.peer.inbox[0]
			h.peer.inbox = h.peer.inbox[1:]
			h.deliver(in)

			continue
		}

		h.fireCurrent()
	}

	require.FailNow(h.t, "syncer never settled")
}

// expectedMissing returns the SCIDs in the model graph that we don't know.
func expectedMissing(graph []graphdb.BlockChannelRange,
	known map[lnwire.ShortChannelID]struct{}) []lnwire.ShortChannelID {

	var missing []lnwire.ShortChannelID
	for _, b := range graph {
		for _, c := range b.Channels {
			if _, ok := known[c.ShortChannelID]; !ok {
				missing = append(missing, c.ShortChannelID)
			}
		}
	}

	return missing
}

// TestSyncerProperties drives the peer syncer through random interleavings
// of commands, honest replies and injected faults, and checks:
//
//   - every transition is listed in SyncerTransitions,
//   - no reply is consumed by an attempt other than the one it answers,
//   - every started attempt gets exactly one outcome,
//   - a completed attempt that saw no fault queried exactly the channels the
//     peer has and we lack,
//   - the last timestamp filter sent matches the final sync type.
func TestSyncerProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		const maxPerBlock = 6

		graph := genGraph(rt, 1_999, maxPerBlock)

		// Know a random subset of the peer's channels.
		known := make(map[lnwire.ShortChannelID]struct{})
		for _, b := range graph {
			for _, c := range b.Channels {
				if rapid.Bool().Draw(rt, "known") {
					known[c.ShortChannelID] = struct{}{}
				}
			}
		}

		// Chunks are large enough that no block is ever truncated, so
		// an honest stream carries the whole graph.
		chunk := rapid.IntRange(2*maxPerBlock, 60).Draw(rt, "chunk")
		batch := rapid.IntRange(1, 20).Draw(rt, "batch")
		noTS := rapid.Bool().Draw(rt, "noTimestamps")

		env := testSyncerEnv(&fakeKnown{known: known}, batch, noTS)
		peer := &modelPeer{graph: graph, chunkSize: chunk}
		h := &syncerHarness{
			queriedFor: make(
				map[AttemptID][]lnwire.ShortChannelID,
			),
			t:            rt,
			env:          env,
			state:        NewIdle(),
			peer:         peer,
			outcomes:     make(map[AttemptID][]SyncOutcome),
			faulted:      make(map[AttemptID]bool),
			queryAttempt: make(map[lnwire.Message]AttemptID),
		}

		steps := rapid.IntRange(1, 80).Draw(rt, "steps")
		for range steps {
			h.step()
		}
		h.settle()

		want := expectedMissing(graph, known)
		for _, a := range h.started {
			outs := h.outcomes[a]
			require.Len(rt, outs, 1, "attempt %d outcomes", a)

			afterDup := h.firstDesync != 0 && a >= h.firstDesync
			if outs[0].Kind != OutcomeCompleted || h.faulted[a] ||
				afterDup {
				continue
			}

			got := slices.Clone(h.queriedFor[a])
			require.ElementsMatch(rt, want, got,
				"attempt %d queried the wrong channels", a)
		}

		// The filter we last sent reflects the final role.
		st := h.state.(*Idle).SyncType()
		if f := h.peer.lastFilter; f != nil {
			require.Equal(
				rt, st.wantsGossip(), f.TimestampRange != 0,
			)
		} else {
			require.False(rt, st.wantsGossip())
		}
	})
}

// TestSyncerLocalFault asserts that a failed local lookup ends the attempt
// with a local fault rather than blaming the peer.
func TestSyncerLocalFault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := testSyncerEnv(&fakeKnown{err: errors.New("db closed")}, 10, true)

	var state SyncerState = NewIdle()
	state, out, err := protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&StartHistoricalSync{Attempt: 1}), env,
	)
	require.NoError(t, err)
	query := out[0].(*SendToPeer).Msgs[0].(*lnwire.QueryChannelRange)

	final := &lnwire.ReplyChannelRange{
		ChainHash:        testChain,
		FirstBlockHeight: 0,
		NumBlocks:        query.NumBlocks,
		Complete:         1,
		EncodingType:     lnwire.EncodingSortedPlain,
	}
	state, out, err = protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&RangeReplyReceived{Reply: final}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Idle{}, state)
	outcome := out[1].(*ReportOutcome).Outcome
	require.Equal(t, OutcomeLocalFault, outcome.Kind)
}

// TestDrainingLegacyPeer asserts that a legacy peer's abandoned stream is
// drained until the reply that sets Complete. A legacy reply echoes the whole
// query, so it covers the query's last block from the first reply on, and
// ending the drain there would let the rest of the stream reach the next
// attempt, where legacy replies skip the range checks.
func TestDrainingLegacyPeer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := testSyncerEnv(&fakeKnown{}, 10, true)

	var state SyncerState = NewIdle()
	state, out, err := protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&StartHistoricalSync{Attempt: 1}), env,
	)
	require.NoError(t, err)
	query := out[0].(*SendToPeer).Msgs[0].(*lnwire.QueryChannelRange)

	// The attempt times out, which drains the abandoned stream.
	seq := out[1].(*ArmReplyTimer).Seq
	state, _, err = protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&ReplyTimerFired{Seq: seq}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Draining{}, state)

	echo := func(complete uint8) SyncerEvent {
		return &RangeReplyReceived{Reply: &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			FirstBlockHeight: query.FirstBlockHeight,
			NumBlocks:        query.NumBlocks,
			Complete:         complete,
			EncodingType:     lnwire.EncodingSortedPlain,
		}}
	}

	state, _, err = protofsm.ApplyEvents(ctx, state, echo(0), env)
	require.NoError(t, err)
	require.IsType(t, &Draining{}, state, "drain ended before Complete")

	state, _, err = protofsm.ApplyEvents(ctx, state, echo(1), env)
	require.NoError(t, err)
	require.IsType(t, &Idle{}, state)
}

// TestDrainingRearmsTimer asserts that each reply absorbed while draining
// re-arms the drain timer, so a slow peer's abandoned stream is drained in
// full rather than cut off by a fixed deadline and read as replies to the
// next attempt.
func TestDrainingRearmsTimer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := testSyncerEnv(&fakeKnown{}, 10, true)

	var state SyncerState = NewIdle()
	state, out, err := protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&StartHistoricalSync{Attempt: 1}), env,
	)
	require.NoError(t, err)
	query := out[0].(*SendToPeer).Msgs[0].(*lnwire.QueryChannelRange)
	require.Greater(t, query.NumBlocks, uint32(1))

	seq := out[1].(*ArmReplyTimer).Seq
	state, out, err = protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&ReplyTimerFired{Seq: seq}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Draining{}, state)
	drainSeq := out[1].(*ArmReplyTimer).Seq

	// A reply covering only the first block of the abandoned query is
	// absorbed, and arms a fresh drain timer.
	state, out, err = protofsm.ApplyEvents(ctx, state,
		SyncerEvent(&RangeReplyReceived{Reply: &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			FirstBlockHeight: query.FirstBlockHeight,
			NumBlocks:        1,
			EncodingType:     lnwire.EncodingSortedPlain,
		}}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Draining{}, state)
	require.Len(t, out, 1)
	timer := out[0].(*ArmReplyTimer)
	require.Greater(t, timer.Seq, drainSeq)
	require.Equal(t, env.DrainTimeout, timer.After)

	// The superseded drain timer is now stale.
	state, _, err = protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&ReplyTimerFired{Seq: drainSeq}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Draining{}, state, "stale drain timer ended drain")

	state, _, err = protofsm.ApplyEvents(
		ctx, state, SyncerEvent(&ReplyTimerFired{Seq: timer.Seq}), env,
	)
	require.NoError(t, err)
	require.IsType(t, &Idle{}, state)
}
