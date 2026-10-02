package gossipsync

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/protofsm"
)

// syncerActor drives one peer's syncer state machine. It owns the machine's
// state, applies each event it receives, commits the new state, and then
// carries out the outbox. It is the only goroutine that touches the state.
type syncerActor struct {
	// session is the connection this syncer serves.
	session SessionID

	// state is the machine's current state.
	state SyncerState

	// env is the machine's environment.
	env *SyncerEnv

	// send sends messages to the peer.
	send *sender

	// manager receives this syncer's attempt outcomes.
	manager actor.TellOnlyRef[managerMsg]

	// timeouts runs the reply timer.
	timeouts actor.TellOnlyRef[timeout.Msg]

	// self is this actor's own ref, which the reply timer calls back. It
	// is set right after the actor is registered, before the manager
	// sends it any command, and no transition arms a timer until the
	// manager has sent one.
	self actor.TellOnlyRef[syncerMsg]

	// courier delivers outcomes to the manager without blocking.
	courier courier

	// timerID names this session's reply timer. Arming a timer under the
	// same ID replaces the previous one.
	timerID timeout.ID
}

// newSyncerActor returns the behavior of a new peer syncer.
func newSyncerActor(session SessionID, env *SyncerEnv, send *sender,
	manager actor.TellOnlyRef[managerMsg],
	timeouts actor.TellOnlyRef[timeout.Msg]) *syncerActor {

	prefix := fmt.Sprintf("gossipsync/%v/%d", env.Peer, session)

	return &syncerActor{
		session:  session,
		state:    NewIdle(),
		env:      env,
		send:     send,
		manager:  manager,
		timeouts: timeouts,
		courier: courier{
			timeouts: timeouts,
			prefix:   prefix + "/outcome",
		},
		timerID: timeout.ID(prefix + "/reply"),
	}
}

// Receive applies an event to the syncer's state machine.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (s *syncerActor) Receive(ctx context.Context,
	msg syncerMsg) fn.Result[Ack] {

	m, ok := msg.(*syncerEventMsg)
	if !ok {
		return fn.Err[Ack](fmt.Errorf("unknown message %T", msg))
	}

	next, outbox, err := protofsm.ApplyEvents(ctx, s.state, m.event, s.env)
	if err != nil {
		// Transitions only return errors for events they don't know,
		// which is a bug, so we keep the current state and report it.
		log.Errorf("GossipSyncer(%v): %v", s.env.Peer, err)

		return fn.Err[Ack](err)
	}

	log.Tracef("GossipSyncer(%v): %v --%T--> %v", s.env.Peer, s.state,
		m.event, next)

	s.state = next

	for _, out := range outbox {
		s.dispatch(ctx, out)
	}

	return fn.Ok(Ack{})
}

// dispatch carries out one outbox event.
func (s *syncerActor) dispatch(ctx context.Context, out SyncerOutbox) {
	switch o := out.(type) {
	case *SendToPeer:
		// A failed send means the peer is going away, and its
		// disconnect will stop this actor, so there's nothing to do
		// but log it.
		if err := s.send.send(ctx, false, o.Msgs...); err != nil {
			log.Debugf("GossipSyncer(%v): unable to send: %v",
				s.env.Peer, err)
		}

	case *ReportOutcome:
		deliver[managerMsg](ctx, &s.courier, s.manager,
			&managerEventMsg{event: &HistoricalSyncOutcome{
				Session: s.session,
				Attempt: o.Attempt,
				Outcome: o.Outcome,
			}},
		)

	case *ArmReplyTimer:
		seq := o.Seq
		callback := timeout.MapTimeoutExpired(
			s.self, func(timeout.ExpiredMsg) syncerMsg {
				return &syncerEventMsg{
					event: &ReplyTimerFired{Seq: seq},
				}
			},
		)
		s.timeouts.Tell(ctx, &timeout.ScheduleTimeoutRequest{
			ID:       s.timerID,
			Duration: o.After,
			Callback: callback,
		})

	case *DisarmReplyTimer:
		s.timeouts.Tell(ctx, &timeout.CancelTimeoutRequest{
			ID: s.timerID,
		})

	default:
		log.Errorf("GossipSyncer(%v): unknown outbox event %T",
			s.env.Peer, out)
	}
}

// OnStop cancels the reply timer.
//
// NOTE: This implements the actor.Stoppable interface.
func (s *syncerActor) OnStop(ctx context.Context) error {
	s.timeouts.Tell(ctx, &timeout.CancelTimeoutRequest{ID: s.timerID})

	return nil
}
