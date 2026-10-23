package chanfsm

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrLocalDataLoss is returned when the peer's channel_reestablish
	// says our current commitment is ahead of what we have: we lost
	// state, and must not update the channel again. Only the channel can
	// confirm it, by checking the secret the peer sends with it.
	ErrLocalDataLoss = errors.New("channel_reestablish: peer knows a " +
		"newer local commitment than we have")

	// ErrRemoteDataLoss is returned when the peer's channel_reestablish
	// shows it lost state it already acknowledged.
	ErrRemoteDataLoss = errors.New("channel_reestablish: peer lost " +
		"state it acknowledged")

	// ErrCannotSync is returned when the peer's channel_reestablish names
	// a commitment height no retransmission can reach.
	ErrCannotSync = errors.New("channel_reestablish: commitment " +
		"heights can't be synchronized")
)

// Reestablish is what the peer's channel_reestablish tells us: the height
// of the next commitment it expects from us, and the height of our current
// commitment as far as it knows.
type Reestablish struct {
	// NextLocalHeight is next_commitment_number: one above the height of
	// the peer's current commitment, unless it never received one we
	// signed.
	NextLocalHeight uint64

	// RemoteTailHeight is the height of our current commitment as the
	// peer knows it: next_revocation_number, the height of the next
	// commitment of ours it expects us to revoke.
	RemoteTailHeight uint64
}

// RestoredLedger is the ledger of a channel just loaded from disk, before
// channel_reestablish. Our side of it is a single commitment: lnd never
// persists a new local commitment before revoking the previous one, so a
// restored channel has none pending, and this type can't hold one. The only
// way to turn it into a Ledger the protocol runs on is Reestablish.
type RestoredLedger struct {
	logs          lntypes.Dual[Log]
	local         Commit
	remote        Chain
	initiator     lntypes.ChannelParty
	lastWasRevoke bool
}

// ledger returns the restored state as a Ledger.
func (r RestoredLedger) ledger() Ledger {
	return Ledger{
		Logs: lntypes.Dual[Log]{
			Local:  r.logs.Local.clone(),
			Remote: r.logs.Remote.clone(),
		},
		Chains: lntypes.Dual[Chain]{
			Local:  Chain{Tail: r.local},
			Remote: r.remote,
		},
		Initiator:     r.initiator,
		LastWasRevoke: r.lastWasRevoke,
	}
}

// SyncPlan is what we retransmit in answer to a channel_reestablish. It is
// one of InSync, ResendRevocation, ResendCommitment or ResendBoth: exactly
// the answers lnd's ProcessChanSyncMsg can give.
type SyncPlan interface {
	// Messages lists what the plan sends, in order: "raa" for a
	// revoke_and_ack, "update" for each resent update and "sig" for a
	// commitment_signed.
	Messages() []string

	syncPlan()
}

// InSync means the peer has everything we sent.
type InSync struct{}

// ResendRevocation means the peer never received our last revoke_and_ack,
// which we send again. If SignNew is set, we then sign a new commitment for
// the peer, since we owe one and have none outstanding.
type ResendRevocation struct {
	// SignNew is set if a new commitment_signed follows.
	SignNew bool
}

// ResendCommitment means the peer never received the commitment we signed
// for it, which we send again: the updates it added, then its
// commitment_signed.
type ResendCommitment struct {
	// Updates are the log indexes of the updates, in order.
	Updates []uint64
}

// ResendBoth means the peer received neither our last revoke_and_ack nor
// the commitment we signed after or before it. With a commitment
// outstanding we can't sign a new one, so nothing else follows.
type ResendBoth struct {
	// Updates are the log indexes of the commitment's updates.
	Updates []uint64

	// CommitmentFirst is set if we sent the commitment before the
	// revoke_and_ack, and so resend it first.
	CommitmentFirst bool
}

func (InSync) syncPlan()           {}
func (ResendRevocation) syncPlan() {}
func (ResendCommitment) syncPlan() {}
func (ResendBoth) syncPlan()       {}

// Messages lists what the plan sends.
func (InSync) Messages() []string { return nil }

// Messages lists what the plan sends.
func (p ResendRevocation) Messages() []string {
	if p.SignNew {
		return []string{"raa", "sig"}
	}

	return []string{"raa"}
}

// Messages lists what the plan sends.
func (p ResendCommitment) Messages() []string {
	return commitmentMessages(p.Updates)
}

// Messages lists what the plan sends.
func (p ResendBoth) Messages() []string {
	if p.CommitmentFirst {
		return append(commitmentMessages(p.Updates), "raa")
	}

	return append([]string{"raa"}, commitmentMessages(p.Updates)...)
}

// commitmentMessages lists a resent commitment's messages.
func commitmentMessages(updates []uint64) []string {
	msgs := make([]string, 0, len(updates)+1)
	for range updates {
		msgs = append(msgs, "update")
	}

	return append(msgs, "sig")
}

// Reestablish answers the peer's channel_reestablish. It returns the ledger
// the protocol continues from and what to retransmit, or the reason the
// channel can't continue. It mirrors lnd's ProcessChanSyncMsg.
func (r RestoredLedger) Reestablish(msg Reestablish) (Ledger, SyncPlan,
	error) {

	var (
		l          = r.ledger()
		localTail  = r.local.Height
		remoteTail = r.remote.Tail.Height
		remoteTip  = r.remote.Tip().Height
	)

	// Their view of our chain: in sync, or one revocation behind.
	var owesRevocation bool
	switch {
	case msg.RemoteTailHeight > localTail:
		return Ledger{}, nil, ErrLocalDataLoss

	case msg.RemoteTailHeight+1 < localTail:
		return Ledger{}, nil, ErrRemoteDataLoss

	case msg.RemoteTailHeight+1 == localTail:
		owesRevocation = true
	}

	// Our view of their chain: in sync, or one commitment behind, which
	// can only be the pending one.
	var (
		updates   []uint64
		resending bool
	)
	switch {
	case msg.NextLocalHeight > remoteTip+1:
		return Ledger{}, nil, fmt.Errorf("%w: next local height %d, "+
			"remote tip %d", ErrCannotSync, msg.NextLocalHeight,
			remoteTip)

	case msg.NextLocalHeight <= remoteTail:
		return Ledger{}, nil, ErrRemoteDataLoss

	case msg.NextLocalHeight == remoteTip:
		pending := r.remote.Pending.UnwrapOr(Commit{})
		updates = []uint64{}
		for _, e := range r.logs.Local.Entries {
			if e.LogIndex >= r.remote.Tail.MsgIdx.Local &&
				e.LogIndex < pending.MsgIdx.Local {

				updates = append(updates, e.LogIndex)
			}
		}
		resending = true
	}

	switch {
	case owesRevocation && resending:
		return l, ResendBoth{
			Updates:         updates,
			CommitmentFirst: r.lastWasRevoke,
		}, nil

	case resending:
		return l, ResendCommitment{Updates: updates}, nil

	// Having sent a revocation the peer never received, we may also owe
	// it a commitment for updates it sent that we acknowledged, and with
	// nothing outstanding, we sign one.
	case owesRevocation:
		if r.remote.Pending.IsNone() && l.OweCommitment(lntypes.Local) {
			signed, err := l.SignCommitment()
			if err != nil {
				return Ledger{}, nil, err
			}

			return signed, ResendRevocation{SignNew: true}, nil
		}

		return l, ResendRevocation{}, nil

	default:
		return l, InSync{}, nil
	}
}
