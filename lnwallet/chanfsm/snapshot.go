package chanfsm

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// LedgerFromSnapshot builds the ledger that describes a channel's commitment
// protocol state. It fails if the channel is in a state the protocol never
// reaches, such as a chain with more than one unrevoked commitment beyond
// its tail.
func LedgerFromSnapshot(s *lnwallet.ProtocolSnapshot,
	initiator lntypes.ChannelParty) (Ledger, error) {

	l := Ledger{Initiator: initiator, LastWasRevoke: s.LastWasRevoke}

	for _, p := range parties {
		chain, err := chainFromSnapshot(s.Chains.GetForParty(p))
		if err != nil {
			return Ledger{}, fmt.Errorf("%v chain: %w", p, err)
		}
		*chainOf(&l, p) = chain

		log, err := logFromSnapshot(s.Logs.GetForParty(p))
		if err != nil {
			return Ledger{}, fmt.Errorf("%v log: %w", p, err)
		}
		*logOf(&l, p) = log
	}

	return l, nil
}

// chainFromSnapshot converts a snapshot commitment chain.
func chainFromSnapshot(s lnwallet.SnapshotChain) (Chain, error) {
	commit := func(c lnwallet.SnapshotCommit) Commit {
		return Commit{Height: c.Height, MsgIdx: c.MessageIndices}
	}

	switch len(s.Commits) {
	case 1:
		return Chain{Tail: commit(s.Commits[0])}, nil

	case 2:
		return Chain{
			Tail:    commit(s.Commits[0]),
			Pending: fn.Some(commit(s.Commits[1])),
		}, nil

	default:
		return Chain{}, fmt.Errorf("%d unrevoked commitments",
			len(s.Commits))
	}
}

// logFromSnapshot converts a snapshot update log.
func logFromSnapshot(s lnwallet.SnapshotLog) (Log, error) {
	l := Log{
		LogIndex:    s.LogIndex,
		HtlcCounter: s.HtlcCounter,
		Modified:    slices.Clone(s.Modified),
	}

	for _, u := range s.Updates {
		e := Entry{LogIndex: u.LogIndex}

		switch u.Kind {
		case lnwallet.SnapshotAdd:
			e.Kind = KindAdd
			e.HtlcIndex = u.HtlcIndex
			e.Heights = u.AddHeights

		case lnwallet.SnapshotSettle:
			e.Kind = KindSettle
			e.ParentIndex = u.ParentIndex
			e.Heights = u.RemoveHeights

		case lnwallet.SnapshotFail:
			e.Kind = KindFail
			e.ParentIndex = u.ParentIndex
			e.Heights = u.RemoveHeights

		case lnwallet.SnapshotMalformedFail:
			e.Kind = KindMalformed
			e.ParentIndex = u.ParentIndex
			e.Heights = u.RemoveHeights

		case lnwallet.SnapshotFeeUpdate:
			e.Kind = KindFee
			e.Heights = u.AddHeights

		default:
			return Log{}, fmt.Errorf("unknown update kind %v",
				u.Kind)
		}

		l.Entries = append(l.Entries, e)
	}

	// lnd keeps a restored log in the order it rebuilt it, which isn't
	// always log order. Log order is what the protocol means.
	slices.SortFunc(l.Entries, func(a, b Entry) int {
		return cmp.Compare(a.LogIndex, b.LogIndex)
	})

	return l, nil
}

// MatchesSnapshot returns an error describing the first difference between
// the ledger and a channel's commitment protocol state, or nil if they
// agree. It doesn't compare LastWasRevoke, which a live channel doesn't
// keep current.
func (l Ledger) MatchesSnapshot(s *lnwallet.ProtocolSnapshot) error {
	want, err := LedgerFromSnapshot(s, l.Initiator)
	if err != nil {
		return err
	}

	for _, p := range parties {
		got, exp := chainOf(&l, p), chainOf(&want, p)
		if !reflect.DeepEqual(normChain(*got), normChain(*exp)) {
			return fmt.Errorf("%v chain: ledger %+v, channel %+v",
				p, *got, *exp)
		}

		gotLog := normLog(*logOf(&l, p))
		expLog := normLog(*logOf(&want, p))
		if !reflect.DeepEqual(gotLog, expLog) {
			return fmt.Errorf("%v log: ledger %+v, channel %+v", p,
				gotLog, expLog)
		}
	}

	return nil
}

// normChain returns a comparable form of a chain.
func normChain(c Chain) [2]Commit {
	return [2]Commit{c.Tail, c.Pending.UnwrapOr(Commit{})}
}

// normLog returns the log with nil and empty slices made equal.
func normLog(l Log) Log {
	if len(l.Entries) == 0 {
		l.Entries = nil
	}
	if len(l.Modified) == 0 {
		l.Modified = nil
	}

	return l
}
