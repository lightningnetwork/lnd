package chanfsm

import (
	"errors"
	"fmt"
	"slices"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// Restore returns the ledger lnd rebuilds from disk for a channel whose
// in-memory state was this ledger, as it does on every reconnection.
//
// lnd persists both current commitments, the pending remote commitment with
// the updates of ours it added, the peer's updates we acknowledged but
// haven't signed for (those between the peer's current commitment and ours),
// and our updates the peer acknowledged but hasn't signed for (those between
// our current commitment and the peer's). Everything else, such as updates
// no commitment of ours includes yet or a new local commitment we haven't
// revoked the previous one for, is lost, and the two sides retransmit it
// during channel_reestablish.
//
// Each restored entry keeps its log index, but its heights are rebuilt from
// the commitments that include it: the current height of each chain, or the
// pending remote height. They are the heights a replay of the protocol from
// the current commitments would give, which is all the protocol depends on.
func (l Ledger) Restore() RestoredLedger {
	var (
		lt = l.Chains.Local.Tail
		rt = l.Chains.Remote.Tail
		rp = l.Chains.Remote.Pending
	)

	// removedOn reports whether a commitment including the given indexes
	// removes the HTLC with the given ID from the owner's log: the other
	// party's log holds a removal of it below the commitment's index for
	// that log.
	removedOn := func(owner lntypes.ChannelParty, id uint64,
		idx lntypes.Dual[uint64]) bool {

		remover := owner.CounterParty()
		below := idx.GetForParty(remover)

		return slices.ContainsFunc(
			logOf(&l, remover).Entries, func(e Entry) bool {
				return e.Kind.isRemoval() &&
					e.ParentIndex == id &&
					e.LogIndex < below
			},
		)
	}

	// onCommit reports whether a commitment including the given indexes
	// holds the given add of the owner's log.
	onCommit := func(owner lntypes.ChannelParty, e Entry,
		idx lntypes.Dual[uint64]) bool {

		return e.LogIndex < idx.GetForParty(owner) &&
			!removedOn(owner, e.HtlcIndex, idx)
	}

	// The parents of the removals in a range of a log, whose adds lnd
	// stamps with the height of the commitment that includes the removal.
	parents := func(g *Log, from, to uint64) []uint64 {
		var ids []uint64
		for _, e := range g.Entries {
			if e.Kind.isRemoval() && e.LogIndex >= from &&
				e.LogIndex < to {

				ids = append(ids, e.ParentIndex)
			}
		}

		return ids
	}

	// Our updates the peer acknowledged but hasn't signed for, and the
	// peer's updates we acknowledged but haven't signed for.
	peerUnsigned := parents(
		&l.Logs.Local, lt.MsgIdx.Local, rt.MsgIdx.Local,
	)
	ackedUnsigned := parents(
		&l.Logs.Remote, rt.MsgIdx.Remote, lt.MsgIdx.Remote,
	)

	next := RestoredLedger{
		initiator:     l.Initiator,
		lastWasRevoke: l.LastWasRevoke,
		local:         lt,
		remote:        Chain{Tail: rt, Pending: rp},
	}
	tip := next.remote.Tip()

	// The peer's log: the adds on our current commitment, then the
	// peer's other updates we acknowledged but haven't signed for.
	theirs := &next.logs.Remote
	theirs.LogIndex = lt.MsgIdx.Remote
	theirs.HtlcCounter = htlcCounterBelow(&l.Logs.Remote, theirs.LogIndex)
	for _, e := range l.Logs.Remote.Entries {
		switch {
		case e.Kind == KindAdd &&
			onCommit(lntypes.Remote, e, lt.MsgIdx):

			e.Heights.Local = lt.Height
			switch {
			case onCommit(lntypes.Remote, e, rt.MsgIdx),
				slices.Contains(peerUnsigned, e.HtlcIndex):

				e.Heights.Remote = rt.Height

			case fn.MapOptionZ(rp, func(c Commit) bool {
				return onCommit(lntypes.Remote, e, c.MsgIdx)
			}):
				e.Heights.Remote = rp.UnwrapOr(Commit{}).Height

			default:
				e.Heights.Remote = 0
			}

		case e.Kind != KindAdd && e.LogIndex >= rt.MsgIdx.Remote &&
			e.LogIndex < lt.MsgIdx.Remote:

			e.Heights.Local = lt.Height
			e.Heights.Remote = 0
			if e.LogIndex < tip.MsgIdx.Remote && rp.IsSome() {
				e.Heights.Remote = tip.Height
			}
			if e.Kind.isRemoval() {
				next.logs.Local.markModified(e.ParentIndex)
			}

		default:
			continue
		}
		theirs.Entries = append(theirs.Entries, e)
	}

	// Our log: the adds on the peer's current commitment, the updates
	// the pending commitment added, and our other updates the peer
	// acknowledged but hasn't signed for.
	ours := &next.logs.Local
	ours.LogIndex = tip.MsgIdx.Local
	ours.HtlcCounter = htlcCounterBelow(&l.Logs.Local, ours.LogIndex)
	for _, e := range l.Logs.Local.Entries {
		switch {
		case e.Kind == KindAdd && onCommit(lntypes.Local, e, rt.MsgIdx):
			e.Heights.Remote = rt.Height
			e.Heights.Local = 0
			if onCommit(lntypes.Local, e, lt.MsgIdx) ||
				slices.Contains(ackedUnsigned, e.HtlcIndex) {

				e.Heights.Local = lt.Height
			}

		case rp.IsSome() && e.LogIndex >= rt.MsgIdx.Local &&
			e.LogIndex < tip.MsgIdx.Local:

			e.Heights = lntypes.Dual[uint64]{Remote: tip.Height}
			if e.Kind.isRemoval() {
				theirs.markModified(e.ParentIndex)
			}

		case e.Kind != KindAdd && e.LogIndex >= lt.MsgIdx.Local &&
			e.LogIndex < rt.MsgIdx.Local:

			e.Heights = lntypes.Dual[uint64]{Remote: rt.Height}
			if e.Kind.isRemoval() {
				theirs.markModified(e.ParentIndex)
			}

		default:
			continue
		}
		ours.Entries = append(ours.Entries, e)
	}

	return next
}

// RestoredFromSnapshot returns the restored ledger of a channel just loaded
// from disk. It fails if the channel is in a state lnd never persists, such
// as a new local commitment it hasn't revoked the previous one for.
func RestoredFromSnapshot(s *lnwallet.ProtocolSnapshot,
	initiator lntypes.ChannelParty) (RestoredLedger, error) {

	l, err := LedgerFromSnapshot(s, initiator)
	if err != nil {
		return RestoredLedger{}, err
	}
	if l.Chains.Local.Pending.IsSome() {
		return RestoredLedger{}, errors.New("a channel loaded from " +
			"disk has a pending local commitment")
	}

	return RestoredLedger{
		logs:          l.Logs,
		local:         l.Chains.Local.Tail,
		remote:        l.Chains.Remote,
		initiator:     initiator,
		lastWasRevoke: l.LastWasRevoke,
	}, nil
}

// MatchesSnapshot returns an error describing the first difference between
// the restored ledger and a channel just loaded from disk, including the
// persisted LastWasRevoke.
func (r RestoredLedger) MatchesSnapshot(s *lnwallet.ProtocolSnapshot) error {
	if err := r.ledger().MatchesSnapshot(s); err != nil {
		return err
	}
	if r.lastWasRevoke != s.LastWasRevoke {
		return fmt.Errorf("LastWasRevoke: ledger %v, channel %v",
			r.lastWasRevoke, s.LastWasRevoke)
	}

	return nil
}

// htlcCounterBelow returns the HTLC counter a log had when its index was
// the given one: the ID of its first add at or above the index, or its
// current counter if there is none.
func htlcCounterBelow(g *Log, idx uint64) uint64 {
	for _, e := range g.Entries {
		if e.Kind == KindAdd && e.LogIndex >= idx {
			return e.HtlcIndex
		}
	}

	return g.HtlcCounter
}
