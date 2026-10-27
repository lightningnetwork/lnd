package chanfsm

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

var (
	// ErrChannelFailed is returned for a command sent to a failed
	// channel.
	ErrChannelFailed = errors.New("channel failed")

	// ErrMalformedNotBadOnion is returned for an
	// update_fail_malformed_htlc whose failure code lacks the BADONION
	// bit, which BOLT 2 requires.
	ErrMalformedNotBadOnion = errors.New("update_fail_malformed_htlc " +
		"without the BADONION bit")

	// ErrForwardMismatch is returned when the channel's forwarding
	// package differs from the updates the ledger says the revocation
	// locked in.
	ErrForwardMismatch = errors.New("forwarding package differs from " +
		"the ledger")

	// ErrLedgerDiverged is returned when the channel's protocol state
	// differs from the ledger after an operation.
	ErrLedgerDiverged = errors.New("channel state differs from the " +
		"ledger")

	// ErrNotReestablished is returned for a command sent before the
	// channel finished channel_reestablish.
	ErrNotReestablished = errors.New("channel not reestablished yet")

	// ErrReestablishFirst is returned for a peer message other than
	// channel_reestablish that arrives before it, which BOLT 2 forbids.
	ErrReestablishFirst = errors.New("peer sent a message before " +
		"channel_reestablish")

	// ErrUnexpectedReestablish is returned for a channel_reestablish
	// that arrives after the channel was reestablished.
	ErrUnexpectedReestablish = errors.New("channel_reestablish " +
		"received on an established channel")

	// ErrSyncMismatch is returned when the channel's answer to a
	// channel_reestablish differs from the ledger's.
	ErrSyncMismatch = errors.New("channel_reestablish answer differs " +
		"from the ledger")
)

// Env is the read-only environment of the channel state machine.
type Env struct {
	// ChanID is the channel's ID, which every message we build carries.
	ChanID lnwire.ChannelID
}

// Name returns a name for the environment, for logging.
//
// NOTE: This implements the protofsm.Environment interface.
func (e *Env) Name() string {
	return "chanfsm-" + e.ChanID.String()
}

// ChanState is a state of the channel state machine.
type ChanState = protofsm.State[Event, Outbox, *Env]

// chanTransition is the transition type of the channel state machine.
type chanTransition = protofsm.StateTransition[Event, Outbox, *Env]

// chanEmitted is the emitted event type of the channel state machine.
type chanEmitted = protofsm.EmittedEvent[Event, Outbox]

// Connecting is the state of a channel just loaded from disk, which has yet
// to send its channel_reestablish. Its ledger is a RestoredLedger, which can
// only be turned into one the protocol runs on by answering the peer's
// channel_reestablish.
type Connecting struct {
	// Restored is the ledger loaded from disk.
	Restored RestoredLedger
}

// Reestablishing is the state of a channel that sent its
// channel_reestablish and waits for the peer's. BOLT 2 forbids any other
// message first.
type Reestablishing struct {
	// Restored is the ledger loaded from disk.
	Restored RestoredLedger
}

// Synced is the state of a channel whose remote commitment chain has no
// unrevoked commitment beyond the peer's current one. We may sign a new one,
// and a revoke_and_ack from the peer is a protocol violation: there is
// nothing for it to revoke.
type Synced struct {
	// Ledger is the channel's protocol state. Its remote chain has no
	// pending commitment.
	Ledger Ledger
}

// AwaitingRevocation is the state of a channel whose peer holds a commitment
// we signed but hasn't yet revoked its previous one. We may not sign another,
// and the peer's next revoke_and_ack is the one that completes this one.
type AwaitingRevocation struct {
	// Ledger is the channel's protocol state. Its remote chain has a
	// pending commitment.
	Ledger Ledger
}

// Applying is the state of a channel whose state machine has authorized an
// operation on the channel and waits for its outcome. The actor runs the
// operation and feeds the outcome back before it handles anything else, so
// no other event ever reaches this state.
type Applying struct {
	// Ledger is the channel's protocol state before the operation.
	Ledger Ledger

	// Op is the operation.
	Op Op

	// reply is set if a local command waits on the operation for its
	// Reply.
	reply bool
}

// Failed is the terminal state of a channel that saw a protocol violation, or
// whose channel refused an operation it can't recover from. It changes
// nothing, and answers every command with ErrChannelFailed.
type Failed struct {
	// Err is the reason the channel failed.
	Err error
}

// String returns the name of the state.
func (s *Connecting) String() string { return "Connecting" }

// String returns the name of the state.
func (s *Reestablishing) String() string { return "Reestablishing" }

// String returns the name of the state.
func (s *Synced) String() string { return "Synced" }

// String returns the name of the state.
func (s *AwaitingRevocation) String() string { return "AwaitingRevocation" }

// String returns the name of the state.
func (s *Applying) String() string { return "Applying(" + s.Op.String() + ")" }

// String returns the name of the state.
func (s *Failed) String() string { return "Failed" }

// IsTerminal returns false: the channel lives on.
func (s *Connecting) IsTerminal() bool { return false }

// IsTerminal returns false: the channel lives on.
func (s *Reestablishing) IsTerminal() bool { return false }

// IsTerminal returns false: the channel lives on.
func (s *Synced) IsTerminal() bool { return false }

// IsTerminal returns false: the channel lives on.
func (s *AwaitingRevocation) IsTerminal() bool { return false }

// IsTerminal returns false: the operation's outcome is still to come.
func (s *Applying) IsTerminal() bool { return false }

// IsTerminal returns true: a failed channel stays failed.
func (s *Failed) IsTerminal() bool { return true }

// NewState returns the first state of a channel just loaded from disk,
// which is how lnd starts every connection to the peer. It fails if the
// channel is in a state lnd never persists.
func NewState(ch Channel) (ChanState, error) {
	initiator := lntypes.Remote
	if ch.IsInitiator() {
		initiator = lntypes.Local
	}

	restored, err := RestoredFromSnapshot(
		ch.ProtocolSnapshot(), initiator,
	)
	if err != nil {
		return nil, err
	}

	return &Connecting{Restored: restored}, nil
}

// ProcessEvent handles an event in the Connecting state. The actor sends
// Connect before anything else, so nothing else ever arrives here.
//
// NOTE: This implements the protofsm.State interface.
func (s *Connecting) ProcessEvent(_ context.Context, event Event,
	_ *Env) (*chanTransition, error) {

	if _, ok := event.(*Connect); !ok {
		return nil, fmt.Errorf("%v: unexpected event %T", s, event)
	}

	op := &opChanSync{restored: s.Restored}

	return emit(&Applying{Op: op}, &ApplyOp{Op: op})
}

// ProcessEvent handles an event in the Reestablishing state. Only the
// peer's channel_reestablish moves the channel on: the restored ledger
// decides what it means before the channel sees it.
//
// NOTE: This implements the protofsm.State interface.
func (s *Reestablishing) ProcessEvent(_ context.Context, event Event,
	_ *Env) (*chanTransition, error) {

	switch e := event.(type) {
	case *PeerReestablish:
		next, plan, err := s.Restored.Reestablish(Reestablish{
			NextLocalHeight:  e.Msg.NextLocalCommitHeight,
			RemoteTailHeight: e.Msg.RemoteCommitTailHeight,
		})

		switch {
		// Only the channel can tell whether the peer is right that
		// we lost state, from the secret it sends: ask it, and fail
		// the channel either way.
		case errors.Is(err, ErrLocalDataLoss):
			return authorize(
				s.Restored.ledger(),
				&opConfirmDataLoss{msg: e.Msg}, false,
			)

		case err != nil:
			return fail(err, false)
		}

		return authorize(next, &opProcessSync{msg: e.Msg, plan: plan},
			false)

	case *Connect, *OpDone:
		return nil, fmt.Errorf("%v: unexpected event %T", s, event)
	}

	if isCommand(event) {
		return refuse(s, ErrNotReestablished)
	}

	return fail(ErrReestablishFirst, false)
}

// stateFor returns the resting state for a ledger: which one is a function of
// whether the peer has a commitment to revoke.
func stateFor(l Ledger) ChanState {
	if l.Chains.Remote.Pending.IsSome() {
		return &AwaitingRevocation{Ledger: l}
	}

	return &Synced{Ledger: l}
}

// emit returns a transition to next that emits the given outbox events.
func emit(next ChanState, out ...Outbox) (*chanTransition, error) {
	t := &chanTransition{NextState: next}
	if len(out) > 0 {
		t.NewEvents = fn.Some(chanEmitted{Outbox: out})
	}

	return t, nil
}

// authorize returns a transition to Applying that asks the actor to run op,
// after emitting out.
func authorize(l Ledger, op Op, reply bool,
	out ...Outbox) (*chanTransition, error) {

	out = append(slices.Clone(out), &ApplyOp{Op: op})

	return emit(&Applying{Ledger: l, Op: op, reply: reply}, out...)
}

// fail returns a transition to Failed. A local command waiting for its
// answer gets one.
func fail(err error, reply bool) (*chanTransition, error) {
	out := []Outbox{&FailChannel{Err: err}}
	if reply {
		out = append(out, &Reply{Err: err})
	}

	return emit(&Failed{Err: err}, out...)
}

// refuse returns a transition that answers a command with an error and
// changes nothing.
func refuse(s ChanState, err error) (*chanTransition, error) {
	return emit(s, &Reply{Err: err})
}

// ProcessEvent handles an event in the Synced state.
//
// NOTE: This implements the protofsm.State interface.
func (s *Synced) ProcessEvent(_ context.Context, event Event,
	_ *Env) (*chanTransition, error) {

	switch event.(type) {
	// The peer has no commitment we signed that it hasn't revoked its
	// predecessor for, so there is nothing for this revocation to
	// complete. No operation is authorized: the channel is never asked
	// to apply it.
	case *PeerRevokeAndAck:
		return fail(ErrUnexpectedRevocation, false)

	case *SignCommitment:
		if !s.Ledger.OweCommitment(lntypes.Local) {
			return emit(s, &Reply{Value: false})
		}

		return authorize(s.Ledger, &opSign{}, true)
	}

	return handleOpen(s, s.Ledger, event)
}

// ProcessEvent handles an event in the AwaitingRevocation state.
//
// NOTE: This implements the protofsm.State interface.
func (s *AwaitingRevocation) ProcessEvent(_ context.Context, event Event,
	_ *Env) (*chanTransition, error) {

	switch e := event.(type) {
	case *PeerRevokeAndAck:
		// The ledger can't refuse this: the state says the peer has a
		// commitment to revoke.
		if _, _, err := s.Ledger.ReceiveRevocation(); err != nil {
			return nil, fmt.Errorf("AwaitingRevocation with %w",
				err)
		}

		return authorize(s.Ledger, &opReceiveRevocation{msg: e.Msg},
			false)

	// The window is closed until the peer revokes. The link retries on
	// its next commit tick.
	case *SignCommitment:
		return emit(s, &Reply{Value: false})
	}

	return handleOpen(s, s.Ledger, event)
}

// handleOpen handles the events that Synced and AwaitingRevocation handle
// alike. Every one either authorizes an operation, refuses a command, or
// fails the channel, and checks the event against the ledger first, so the
// channel is only ever asked to apply an update the protocol allows.
func handleOpen(s ChanState, l Ledger, event Event) (*chanTransition,
	error) {

	switch e := event.(type) {
	case *PeerReestablish:
		return fail(ErrUnexpectedReestablish, false)

	case *AddHTLC:
		return authorize(l, &opAdd{ev: e}, true)

	case *SettleHTLC:
		_, err := l.RemoveHtlc(lntypes.Local, KindSettle, e.Msg.ID)
		if err != nil {
			return refuse(s, err)
		}

		return authorize(l, &opSettle{ev: e}, true)

	case *FailHTLC:
		_, err := l.RemoveHtlc(lntypes.Local, KindFail, e.Msg.ID)
		if err != nil {
			return refuse(s, err)
		}

		return authorize(l, &opFail{ev: e}, true)

	case *MalformedFailHTLC:
		_, err := l.RemoveHtlc(lntypes.Local, KindMalformed, e.Msg.ID)
		if err != nil {
			return refuse(s, err)
		}

		return authorize(l, &opMalformed{ev: e}, true)

	case *UpdateFee:
		if _, err := l.UpdateFee(lntypes.Local); err != nil {
			return refuse(s, err)
		}

		return authorize(l, &opFee{ev: e}, true)

	case *PeerAdd:
		if _, err := l.AddHtlc(lntypes.Remote, e.Msg.ID); err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveAdd{msg: e.Msg}, false)

	case *PeerFulfill:
		_, err := l.RemoveHtlc(lntypes.Remote, KindSettle, e.Msg.ID)
		if err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveSettle{msg: e.Msg}, false)

	case *PeerFail:
		_, err := l.RemoveHtlc(lntypes.Remote, KindFail, e.Msg.ID)
		if err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveFail{
			id: e.Msg.ID, reason: e.Msg.Reason,
		}, false)

	// lnd records a malformed fail from the peer as an ordinary fail
	// whose reason is the failure it stands for.
	case *PeerFailMalformed:
		if e.Msg.FailureCode&lnwire.FlagBadOnion == 0 {
			return fail(ErrMalformedNotBadOnion, false)
		}
		_, err := l.RemoveHtlc(lntypes.Remote, KindFail, e.Msg.ID)
		if err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveFail{
			id: e.Msg.ID, mal: e.Msg,
		}, false)

	case *PeerUpdateFee:
		if _, err := l.UpdateFee(lntypes.Remote); err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveFee{msg: e.Msg}, false)

	case *PeerCommitSig:
		if _, err := l.ReceiveCommitment(); err != nil {
			return fail(err, false)
		}

		return authorize(l, &opReceiveCommit{msg: e.Msg}, false)

	default:
		return nil, fmt.Errorf("%v: unexpected event %T", s, event)
	}
}

// ProcessEvent handles the outcome of the operation being applied.
//
// NOTE: This implements the protofsm.State interface.
func (s *Applying) ProcessEvent(_ context.Context, event Event,
	env *Env) (*chanTransition, error) {

	done, ok := event.(*OpDone)
	if !ok || done.Op != s.Op {
		return nil, fmt.Errorf("%v: unexpected event %T", s, event)
	}

	if done.Err != nil {
		// A local command the channel refused left it unchanged, as
		// the ledger is.
		if isLocalCommand(s.Op) {
			return refuse(stateFor(s.Ledger), done.Err)
		}

		// The channel can't recover from any other failure in
		// process: a refused peer update is a protocol violation, and
		// the channel may have changed before refusing it.
		return fail(fmt.Errorf("%v: %w", s.Op, done.Err), s.reply)
	}

	t, err := s.complete(done, env)
	if err != nil {
		return fail(err, s.reply)
	}

	// The ledger must now describe the channel. The next state's ledger
	// is the one after this operation, whether it rests or applies the
	// next one.
	var next Ledger
	switch n := t.NextState.(type) {
	case *Synced:
		next = n.Ledger
	case *AwaitingRevocation:
		next = n.Ledger
	case *Applying:
		next = n.Ledger
	default:
		return t, nil
	}
	err = fn.MapOptionZ(done.Snapshot, next.MatchesSnapshot)
	if err != nil {
		return fail(fmt.Errorf("%w after %v: %w", ErrLedgerDiverged,
			s.Op, err), s.reply)
	}

	return t, nil
}

// complete returns the transition for the successful outcome of the
// operation. An error means the channel did something the ledger says it
// can't have, and fails the channel.
func (s *Applying) complete(done *OpDone,
	env *Env) (*chanTransition, error) {

	l := s.Ledger

	switch op := s.Op.(type) {
	case *opAdd:
		id, ok := done.Value.(uint64)
		if !ok {
			return nil, fmt.Errorf("AddHTLC returned %T",
				done.Value)
		}
		next, err := l.AddHtlc(lntypes.Local, id)
		if err != nil {
			return nil, err
		}

		msg := *op.ev.Htlc
		msg.ID = id

		return emit(stateFor(next), &SendToPeer{
			Msgs: []lnwire.Message{&msg},
		}, &Reply{Value: id})

	case *opSettle:
		return s.localUpdate(l.RemoveHtlc(
			lntypes.Local, KindSettle, op.ev.Msg.ID,
		))(op.ev.Msg)

	case *opFail:
		return s.localUpdate(l.RemoveHtlc(
			lntypes.Local, KindFail, op.ev.Msg.ID,
		))(op.ev.Msg)

	case *opMalformed:
		return s.localUpdate(l.RemoveHtlc(
			lntypes.Local, KindMalformed, op.ev.Msg.ID,
		))(op.ev.Msg)

	case *opFee:
		return s.localUpdate(l.UpdateFee(lntypes.Local))(
			&lnwire.UpdateFee{
				ChanID:   env.ChanID,
				FeePerKw: uint32(op.ev.FeePerKw),
			},
		)

	case *opReceiveAdd:
		id, ok := done.Value.(uint64)
		if !ok || id != op.msg.ID {
			return nil, fmt.Errorf("ReceiveHTLC took ID %v for "+
				"add %d", done.Value, op.msg.ID)
		}

		return rest(l.AddHtlc(lntypes.Remote, id))

	case *opReceiveSettle:
		return rest(l.RemoveHtlc(lntypes.Remote, KindSettle, op.msg.ID))

	case *opReceiveFail:
		return rest(l.RemoveHtlc(lntypes.Remote, KindFail, op.id))

	case *opReceiveFee:
		return rest(l.UpdateFee(lntypes.Remote))

	case *opSign:
		commit, ok := done.Value.(*lnwallet.NewCommitState)
		if !ok {
			return nil, fmt.Errorf("SignNextCommitment returned "+
				"%T", done.Value)
		}
		next, err := l.SignCommitment()
		if err != nil {
			return nil, err
		}
		records, err := lnwire.ParseCustomRecords(commit.AuxSigBlob)
		if err != nil {
			return nil, fmt.Errorf("error parsing aux sigs: %w",
				err)
		}

		out := []Outbox{
			&ContractUpdate{
				Set:   RemotePendingHtlcSet,
				Htlcs: commit.PendingHTLCs,
			},
			&SendToPeer{Msgs: []lnwire.Message{&lnwire.CommitSig{
				ChanID:        env.ChanID,
				CommitSig:     commit.CommitSig,
				HtlcSigs:      commit.HtlcSigs,
				PartialSig:    commit.PartialSig,
				CustomRecords: records,
			}}},
		}
		if s.reply {
			out = append(out, &Reply{Value: true})
		}

		return emit(stateFor(next), out...)

	// Having accepted the peer's new commitment, we revoke our previous
	// one at once, as the link does.
	case *opReceiveCommit:
		next, err := l.ReceiveCommitment()
		if err != nil {
			return nil, err
		}

		return authorize(next, &opRevoke{}, false)

	case *opRevoke:
		res, ok := done.Value.(*revokeResult)
		if !ok {
			return nil, fmt.Errorf("RevokeCurrentCommitment "+
				"returned %T", done.Value)
		}
		next, err := l.RevokeCommitment()
		if err != nil {
			return nil, err
		}

		out := []Outbox{&SendToPeer{Msgs: []lnwire.Message{res.msg}}}
		if len(res.final) > 0 {
			out = append(out, &FinalHtlcs{Resolved: res.final})
		}
		out = append(out, &ContractUpdate{
			Set: LocalHtlcSet, Htlcs: res.htlcs,
		})

		return signIfOwed(next, out...)

	// The revocation locks in updates. The channel builds the forwarding
	// package, and the ledger decides which updates it must hold: any
	// difference means one of them is wrong, and nothing is forwarded.
	// Our channel_reestablish is ready: send it, and wait for the
	// peer's.
	case *opChanSync:
		msg, ok := done.Value.(*lnwire.ChannelReestablish)
		if !ok {
			return nil, fmt.Errorf("ChanSyncMsg returned %T",
				done.Value)
		}

		return emit(&Reestablishing{Restored: op.restored},
			&SendToPeer{Msgs: []lnwire.Message{msg}})

	// The channel retransmits what the ledger planned, and nothing else.
	case *opProcessSync:
		msgs, ok := done.Value.([]lnwire.Message)
		if !ok {
			return nil, fmt.Errorf("ProcessChanSyncMsg returned %T",
				done.Value)
		}
		got, want := syncMessages(msgs), planMessages(l, op.plan)
		if !slices.Equal(got, want) {
			return nil, fmt.Errorf("%w: channel %v, ledger %v",
				ErrSyncMismatch, got, want)
		}
		if len(msgs) == 0 {
			return emit(stateFor(l))
		}

		return emit(stateFor(l), &SendToPeer{Msgs: msgs})

	// The channel accepted a channel_reestablish the ledger says means we
	// lost state. It must not have.
	case *opConfirmDataLoss:
		return nil, fmt.Errorf("%w: channel accepted a "+
			"channel_reestablish claiming local data loss",
			ErrSyncMismatch)

	case *opReceiveRevocation:
		res, ok := done.Value.(*revocationResult)
		if !ok {
			return nil, fmt.Errorf("ReceiveRevocation returned %T",
				done.Value)
		}
		next, fwds, err := l.ReceiveRevocation()
		if err != nil {
			return nil, err
		}
		got, want := forwardsOf(res.pkg), sortForwards(fwds)
		if !sameForwards(got, want) {
			return nil, fmt.Errorf("%w: channel %v, ledger %v",
				ErrForwardMismatch, got, want)
		}

		return signIfOwed(next,
			&ContractUpdate{Set: RemoteHtlcSet, Htlcs: res.htlcs},
			&ForwardPackage{Pkg: res.pkg},
		)

	default:
		return nil, fmt.Errorf("unknown operation %v", s.Op)
	}
}

// localUpdate returns a function that completes a local update command: it
// sends the update to the peer and answers the command.
func (s *Applying) localUpdate(next Ledger, err error) func(
	lnwire.Message) (*chanTransition, error) {

	return func(msg lnwire.Message) (*chanTransition, error) {
		if err != nil {
			return nil, err
		}

		return emit(stateFor(next),
			&SendToPeer{Msgs: []lnwire.Message{msg}}, &Reply{},
		)
	}
}

// rest returns the transition to the resting state of a ledger.
func rest(next Ledger, err error) (*chanTransition, error) {
	if err != nil {
		return nil, err
	}

	return emit(stateFor(next))
}

// signIfOwed returns the transition that emits out, then signs a new remote
// commitment if we owe one and may sign, as the link does after a
// commitment exchange.
func signIfOwed(next Ledger, out ...Outbox) (*chanTransition, error) {
	if next.Chains.Remote.Pending.IsNone() &&
		next.OweCommitment(lntypes.Local) {

		return authorize(next, &opSign{}, false, out...)
	}

	return emit(stateFor(next), out...)
}

// ProcessEvent handles an event in the Failed state: commands are refused,
// and everything else is dropped.
//
// NOTE: This implements the protofsm.State interface.
func (s *Failed) ProcessEvent(_ context.Context, event Event,
	_ *Env) (*chanTransition, error) {

	switch event.(type) {
	case *OpDone, *Connect:
		return nil, fmt.Errorf("%v: unexpected event %T", s, event)
	}

	if isCommand(event) {
		return refuse(s, fmt.Errorf("%w: %w", ErrChannelFailed, s.Err))
	}

	return emit(s)
}
