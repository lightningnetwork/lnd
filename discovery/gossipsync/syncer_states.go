package gossipsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

// SyncerState is a state of the peer syncer.
type SyncerState = protofsm.State[SyncerEvent, SyncerOutbox, *SyncerEnv]

// syncerTransition is the transition type of the peer syncer.
type syncerTransition = protofsm.StateTransition[
	SyncerEvent, SyncerOutbox, *SyncerEnv,
]

// syncerEmitted is the emitted event type of the peer syncer.
type syncerEmitted = protofsm.EmittedEvent[SyncerEvent, SyncerOutbox]

// common is carried by every peer syncer state. The sync type is independent
// of the query phase: a role change only means sending a new timestamp
// filter, which BOLT 7 permits at any time.
type common struct {
	// syncType is the peer's current role.
	syncType SyncType

	// timerSeq is the sequence number of the most recently armed reply
	// timer. A ReplyTimerFired with any other number is stale.
	timerSeq uint64
}

// SyncType returns the peer's current role.
func (c common) SyncType() SyncType {
	return c.syncType
}

// Idle is the state of a syncer with no exchange in flight.
type Idle struct {
	common
}

// AwaitingRange is the state of a syncer that sent a query_channel_range and
// is collecting the replies.
type AwaitingRange struct {
	common

	// attempt is the historical sync attempt this query belongs to.
	attempt AttemptID

	// acc collects the replies.
	acc rangeAccumulator
}

// QueryingSCIDs is the state of a syncer that is asking the peer for the
// channels it has that we don't, one batch at a time. Exactly one
// query_short_channel_ids is outstanding.
type QueryingSCIDs struct {
	common

	// attempt is the historical sync attempt these queries belong to.
	attempt AttemptID

	// pending are the SCIDs not yet queried.
	pending []lnwire.ShortChannelID
}

// Draining is the state of a syncer whose attempt failed while the peer may
// still be answering. It absorbs the rest of the abandoned exchange, so that
// no reply can ever be counted toward a later attempt. It returns to Idle when
// the abandoned stream ends or the drain timer fires. Like the reply timer,
// the drain timer is an inactivity timer: every absorbed range reply re-arms
// it.
type Draining struct {
	common

	// rangeQuery is set if the abandoned exchange is a range query, whose
	// remaining replies we absorb until one covers its last block.
	rangeQuery fn.Option[*lnwire.QueryChannelRange]

	// absorbed counts the replies absorbed so far, bounded by the reply
	// budget so a peer can't keep us here.
	absorbed uint32
}

// NewIdle returns the state a syncer starts in. Every syncer starts passive,
// which is what a peer assumes before we send any timestamp filter; the
// manager moves it to another role with SetSyncType, which sends the filter.
func NewIdle() *Idle {
	return &Idle{common: common{syncType: PassiveSync}}
}

// String returns the name of the state.
func (s *Idle) String() string { return "Idle" }

// String returns the name of the state.
func (s *AwaitingRange) String() string { return "AwaitingRange" }

// String returns the name of the state.
func (s *QueryingSCIDs) String() string { return "QueryingSCIDs" }

// String returns the name of the state.
func (s *Draining) String() string { return "Draining" }

// IsTerminal returns false: a syncer lives as long as its peer connection.
func (s *Idle) IsTerminal() bool { return false }

// IsTerminal returns false: a syncer lives as long as its peer connection.
func (s *AwaitingRange) IsTerminal() bool { return false }

// IsTerminal returns false: a syncer lives as long as its peer connection.
func (s *QueryingSCIDs) IsTerminal() bool { return false }

// IsTerminal returns false: a syncer lives as long as its peer connection.
func (s *Draining) IsTerminal() bool { return false }

// emit returns a transition to next that emits the given outbox events.
func emit(next SyncerState, outbox ...SyncerOutbox) (*syncerTransition,
	error) {

	t := &syncerTransition{NextState: next}
	if len(outbox) > 0 {
		t.NewEvents = fn.Some(syncerEmitted{Outbox: outbox})
	}

	return t, nil
}

// busy reports that an attempt could not start.
func busy(a AttemptID) *ReportOutcome {
	return &ReportOutcome{
		Attempt: a,
		Outcome: SyncOutcome{
			Kind: OutcomeBusy,
			Reason: errors.New("a previous exchange with " +
				"the peer is still in flight"),
		},
	}
}

// completed reports that an attempt finished.
func completed(a AttemptID) *ReportOutcome {
	return &ReportOutcome{
		Attempt: a,
		Outcome: SyncOutcome{Kind: OutcomeCompleted},
	}
}

// peerFault reports that an attempt failed because of the peer.
func peerFault(a AttemptID, err error) *ReportOutcome {
	return &ReportOutcome{
		Attempt: a,
		Outcome: SyncOutcome{Kind: OutcomePeerFault, Reason: err},
	}
}

// localFault reports that an attempt failed because of a local lookup.
func localFault(a AttemptID, err error) *ReportOutcome {
	return &ReportOutcome{
		Attempt: a,
		Outcome: SyncOutcome{Kind: OutcomeLocalFault, Reason: err},
	}
}

// armTimer bumps c's timer sequence and returns the effect that arms it.
func (c *common) armTimer(env *SyncerEnv, draining bool) *ArmReplyTimer {
	c.timerSeq++

	after := env.ReplyTimeout
	if draining {
		after = env.DrainTimeout
	}

	return &ArmReplyTimer{Seq: c.timerSeq, After: after}
}

// setSyncType handles SetSyncType, which every state accepts the same way.
// It returns the updated common fields and the effects to emit.
func (c common) setSyncType(env *SyncerEnv, ev *SetSyncType) (common,
	[]SyncerOutbox) {

	if ev.Type == c.syncType {
		return c, nil
	}

	// Moving between active and pinned doesn't change what we ask the
	// peer for, so there's nothing to send.
	wanted, had := ev.Type.wantsGossip(), c.syncType.wantsGossip()
	c.syncType = ev.Type
	if wanted == had {
		return c, nil
	}

	return c, []SyncerOutbox{&SendToPeer{
		Msgs: []lnwire.Message{env.timestampFilter(ev.Type)},
	}}
}

// ProcessEvent handles an event in the Idle state.
//
// NOTE: This implements the protofsm.State interface.
func (s *Idle) ProcessEvent(_ context.Context, event SyncerEvent,
	env *SyncerEnv) (*syncerTransition, error) {

	switch ev := event.(type) {
	case *StartHistoricalSync:
		next := &AwaitingRange{
			common:  s.common,
			attempt: ev.Attempt,
		}
		query := env.historicalQuery()
		next.acc = newRangeAccumulator(query)
		timer := next.armTimer(env, false)

		return emit(next, &SendToPeer{
			Msgs: []lnwire.Message{query},
		}, timer)

	case *SetSyncType:
		c, out := s.setSyncType(env, ev)

		return emit(&Idle{common: c}, out...)

	// A reply we didn't ask for, or a timer from an exchange that has
	// already ended, changes nothing.
	case *RangeReplyReceived, *SCIDsEndReceived, *ReplyTimerFired:
		return emit(s)
	}

	return nil, fmt.Errorf("idle: unknown event %T", event)
}

// ProcessEvent handles an event in the AwaitingRange state.
//
// NOTE: This implements the protofsm.State interface.
func (s *AwaitingRange) ProcessEvent(_ context.Context, event SyncerEvent,
	env *SyncerEnv) (*syncerTransition, error) {

	switch ev := event.(type) {
	case *RangeReplyReceived:
		return s.onReply(env, ev.Reply)

	case *ReplyTimerFired:
		if ev.Seq != s.timerSeq {
			return emit(s)
		}

		return s.abandon(env, ErrReplyTimeout)

	case *StartHistoricalSync:
		return emit(s, busy(ev.Attempt))

	case *SetSyncType:
		next := *s
		c, out := s.setSyncType(env, ev)
		next.common = c

		return emit(&next, out...)

	// The peer has no reason to end an SCID query we haven't sent.
	case *SCIDsEndReceived:
		return emit(s)
	}

	return nil, fmt.Errorf("awaiting range: unknown event %T", event)
}

// onReply folds a range reply into the accumulator, and moves on once the
// stream is complete.
func (s *AwaitingRange) onReply(env *SyncerEnv,
	reply *lnwire.ReplyChannelRange) (*syncerTransition, error) {

	acc, done, err := s.acc.add(reply, env.Limits, env.Now())
	if err != nil {
		return s.abandon(env, err)
	}

	if !done {
		next := *s
		next.acc = acc
		timer := next.armTimer(env, false)

		return emit(&next, timer)
	}

	// The stream is complete, so find out which of the peer's channels
	// we're missing.
	missing, err := env.Graph.FilterKnownChanIDs(
		env.ChainHash, acc.chans, env.IsStillZombie,
	)
	if err != nil {
		return emit(
			&Idle{common: s.common}, &DisarmReplyTimer{},
			localFault(s.attempt, err),
		)
	}

	if len(missing) == 0 {
		return emit(
			&Idle{common: s.common}, &DisarmReplyTimer{},
			completed(s.attempt),
		)
	}

	batch, pending := env.nextBatch(missing)
	next := &QueryingSCIDs{
		common:  s.common,
		attempt: s.attempt,
		pending: pending,
	}
	timer := next.armTimer(env, false)

	return emit(next, &SendToPeer{
		Msgs: []lnwire.Message{env.scidQuery(batch)},
	}, timer)
}

// abandon fails the attempt and drains the rest of the reply stream.
func (s *AwaitingRange) abandon(env *SyncerEnv, reason error) (
	*syncerTransition, error) {

	next := &Draining{
		common:     s.common,
		rangeQuery: fn.Some(s.acc.query),
	}
	timer := next.armTimer(env, true)

	return emit(next, peerFault(s.attempt, reason), timer)
}

// ProcessEvent handles an event in the QueryingSCIDs state.
//
// NOTE: This implements the protofsm.State interface.
func (s *QueryingSCIDs) ProcessEvent(_ context.Context, event SyncerEvent,
	env *SyncerEnv) (*syncerTransition, error) {

	switch ev := event.(type) {
	case *SCIDsEndReceived:
		if len(s.pending) == 0 {
			return emit(
				&Idle{common: s.common}, &DisarmReplyTimer{},
				completed(s.attempt),
			)
		}

		batch, pending := env.nextBatch(s.pending)
		next := *s
		next.pending = pending
		timer := next.armTimer(env, false)

		return emit(&next, &SendToPeer{
			Msgs: []lnwire.Message{env.scidQuery(batch)},
		}, timer)

	case *ReplyTimerFired:
		if ev.Seq != s.timerSeq {
			return emit(s)
		}

		// The peer owes us a reply_short_channel_ids_end, which we'll
		// absorb if it ever arrives.
		next := &Draining{common: s.common}
		timer := next.armTimer(env, true)

		return emit(next, peerFault(s.attempt, ErrReplyTimeout), timer)

	case *StartHistoricalSync:
		return emit(s, busy(ev.Attempt))

	case *SetSyncType:
		next := *s
		c, out := s.setSyncType(env, ev)
		next.common = c

		return emit(&next, out...)

	// A range reply can't belong to this exchange.
	case *RangeReplyReceived:
		return emit(s)
	}

	return nil, fmt.Errorf("querying scids: unknown event %T", event)
}

// ProcessEvent handles an event in the Draining state.
//
// NOTE: This implements the protofsm.State interface.
func (s *Draining) ProcessEvent(_ context.Context, event SyncerEvent,
	env *SyncerEnv) (*syncerTransition, error) {

	idle := func() (*syncerTransition, error) {
		return emit(&Idle{common: s.common}, &DisarmReplyTimer{})
	}

	switch ev := event.(type) {
	case *RangeReplyReceived:
		if s.rangeQuery.IsNone() {
			return emit(s)
		}
		query := s.rangeQuery.UnwrapOr(nil)

		// The abandoned stream ends with the reply that covers the
		// query's last block, or sets Complete. A legacy peer echoes
		// the whole query in every reply, so for it only Complete
		// marks the end, as in rangeAccumulator.complete. The budget
		// bounds a peer that never sends either.
		next := *s
		next.absorbed++
		reply := ev.Reply
		last := query.LastBlockHeight()
		ended := reply.Complete != 0
		if !isLegacyReply(query, reply) {
			ended = ended || reply.LastBlockHeight() >= last
		}
		if ended || next.absorbed >= env.Limits.MaxReplies {
			return idle()
		}

		// Each absorbed reply re-arms the drain timer, so the drain
		// tolerates the same gap between replies as a live exchange.
		// A fixed deadline would let a slow peer's stream outlast it,
		// and the rest would be read as replies to the next attempt.
		timer := next.armTimer(env, true)

		return emit(&next, timer)

	case *SCIDsEndReceived:
		if s.rangeQuery.IsSome() {
			return emit(s)
		}

		return idle()

	case *ReplyTimerFired:
		if ev.Seq != s.timerSeq {
			return emit(s)
		}

		return idle()

	case *StartHistoricalSync:
		return emit(s, busy(ev.Attempt))

	case *SetSyncType:
		next := *s
		c, out := s.setSyncType(env, ev)
		next.common = c

		return emit(&next, out...)
	}

	return nil, fmt.Errorf("draining: unknown event %T", event)
}
