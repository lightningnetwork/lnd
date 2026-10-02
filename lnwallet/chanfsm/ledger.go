package chanfsm

import (
	"errors"
	"fmt"
	"slices"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrUnexpectedRevocation is returned for a revoke_and_ack that
	// arrives while the remote party has no unrevoked commitment beyond
	// its current one, so there is nothing for it to revoke.
	ErrUnexpectedRevocation = errors.New("revoke_and_ack received " +
		"without an outstanding commitment_signed")

	// ErrUnexpectedCommitment is returned for a commitment_signed that
	// arrives while our own new commitment is still unrevoked.
	ErrUnexpectedCommitment = errors.New("commitment_signed received " +
		"while our previous commitment is unrevoked")

	// ErrNoRevocationWindow is returned when we try to sign a new remote
	// commitment while the previous one is still unrevoked.
	ErrNoRevocationWindow = errors.New("remote commitment awaiting " +
		"revocation")

	// ErrNothingToRevoke is returned when we try to revoke our current
	// commitment without having received a new one.
	ErrNothingToRevoke = errors.New("no new local commitment to revoke")

	// ErrFeeUpdateNotInitiator is returned when the party that isn't the
	// channel initiator proposes a fee update.
	ErrFeeUpdateNotInitiator = errors.New("fee update from the " +
		"non-initiator")
)

// ErrHtlcID is returned for an update_add_htlc whose ID isn't the next one
// the sender must use.
type ErrHtlcID struct {
	// Got is the ID the add carries.
	Got uint64

	// Want is the next ID of the sender's log.
	Want uint64
}

// Error returns a description of the error.
func (e *ErrHtlcID) Error() string {
	return fmt.Sprintf("update_add_htlc ID %d, want %d", e.Got, e.Want)
}

// ErrUnknownHtlc is returned for a removal that names an HTLC that isn't in
// the log it targets.
type ErrUnknownHtlc struct {
	// Log is the party whose log the removal targets.
	Log lntypes.ChannelParty

	// ID is the HTLC the removal names.
	ID uint64
}

// Error returns a description of the error.
func (e *ErrUnknownHtlc) Error() string {
	return fmt.Sprintf("no HTLC %d in the %v log", e.ID, e.Log)
}

// ErrHtlcRemoved is returned for a removal of an HTLC that already has one.
type ErrHtlcRemoved struct {
	// Log is the party whose log the removal targets.
	Log lntypes.ChannelParty

	// ID is the HTLC the removal names.
	ID uint64
}

// Error returns a description of the error.
func (e *ErrHtlcRemoved) Error() string {
	return fmt.Sprintf("HTLC %d in the %v log already has a removal",
		e.ID, e.Log)
}

// ErrHtlcNotCommitted is returned for a removal of an HTLC that isn't yet
// committed far enough for BOLT 2 to allow removing it.
type ErrHtlcNotCommitted struct {
	// Log is the party whose log the removal targets.
	Log lntypes.ChannelParty

	// ID is the HTLC the removal names.
	ID uint64
}

// Error returns a description of the error.
func (e *ErrHtlcNotCommitted) Error() string {
	return fmt.Sprintf("HTLC %d in the %v log is not committed far "+
		"enough to be removed", e.ID, e.Log)
}

// parties lists both parties, ours first.
var parties = []lntypes.ChannelParty{lntypes.Local, lntypes.Remote}

// Kind is the kind of an update log entry.
type Kind uint8

const (
	// KindAdd adds an HTLC.
	KindAdd Kind = iota

	// KindSettle settles the HTLC named by ParentIndex.
	KindSettle

	// KindFail fails the HTLC named by ParentIndex.
	KindFail

	// KindMalformed fails the HTLC named by ParentIndex as malformed.
	KindMalformed

	// KindFee changes the commitment fee rate.
	KindFee
)

// String returns the name of the kind.
func (k Kind) String() string {
	switch k {
	case KindAdd:
		return "Add"
	case KindSettle:
		return "Settle"
	case KindFail:
		return "Fail"
	case KindMalformed:
		return "Malformed"
	case KindFee:
		return "Fee"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// isRemoval reports whether the kind removes an HTLC.
func (k Kind) isRemoval() bool {
	return k == KindSettle || k == KindFail || k == KindMalformed
}

// Entry is one update in an update log.
type Entry struct {
	// LogIndex is the entry's position in its log.
	LogIndex uint64

	// Kind is the kind of update.
	Kind Kind

	// HtlcIndex is the ID of the HTLC an add adds.
	HtlcIndex uint64

	// ParentIndex is the ID of the HTLC a removal removes. The HTLC is an
	// add in the other party's log.
	ParentIndex uint64

	// Heights is, for each party's commitment chain, the height of the
	// first commitment that includes this entry, or zero if none does
	// yet. A height is set once and never changes.
	Heights lntypes.Dual[uint64]
}

// Log is the update log of one party: the updates it proposed that are still
// relevant to an unrevoked commitment.
type Log struct {
	// LogIndex is the index the next entry will take.
	LogIndex uint64

	// HtlcCounter is the ID the next add will take.
	HtlcCounter uint64

	// Entries are the entries in log order.
	Entries []Entry

	// Modified holds the IDs of this log's adds that the other party has
	// already proposed a removal for.
	Modified []uint64
}

// clone returns a deep copy of the log.
func (l *Log) clone() Log {
	c := *l
	c.Entries = slices.Clone(l.Entries)
	c.Modified = slices.Clone(l.Modified)

	return c
}

// add returns the index of the add with the given HTLC ID, or -1.
func (l *Log) add(id uint64) int {
	return slices.IndexFunc(l.Entries, func(e Entry) bool {
		return e.Kind == KindAdd && e.HtlcIndex == id
	})
}

// modified reports whether the add with the given ID already has a removal.
func (l *Log) modified(id uint64) bool {
	_, found := slices.BinarySearch(l.Modified, id)
	return found
}

// markModified records that the add with the given ID has a removal.
func (l *Log) markModified(id uint64) {
	i, found := slices.BinarySearch(l.Modified, id)
	if !found {
		l.Modified = slices.Insert(l.Modified, i, id)
	}
}

// unmarkModified forgets the removal of the add with the given ID.
func (l *Log) unmarkModified(id uint64) {
	i, found := slices.BinarySearch(l.Modified, id)
	if found {
		l.Modified = slices.Delete(l.Modified, i, i+1)
	}
}

// append adds an entry at the end of the log.
func (l *Log) append(e Entry) {
	e.LogIndex = l.LogIndex
	l.Entries = append(l.Entries, e)
	l.LogIndex++
}

// Commit is one commitment of a commitment chain.
type Commit struct {
	// Height is the commitment number.
	Height uint64

	// MsgIdx is, for each party's log, the log index up to which the
	// commitment includes updates: every entry below it is included.
	MsgIdx lntypes.Dual[uint64]
}

// Chain is one party's chain of unrevoked commitments. The tail is the
// party's current commitment. Pending is a newer commitment the party has
// been sent a signature for but has not yet revoked the tail for. The
// protocol allows at most one.
type Chain struct {
	// Tail is the current commitment.
	Tail Commit

	// Pending is the signed but not yet acknowledged next commitment.
	Pending fn.Option[Commit]
}

// Tip returns the newest commitment of the chain.
func (c Chain) Tip() Commit {
	return c.Pending.UnwrapOr(c.Tail)
}

// ForwardRef is an update that became irrevocably committed on a revocation,
// and so is handed to the switch.
type ForwardRef struct {
	// Kind is the kind of the update, never KindFee.
	Kind Kind

	// ID is the HTLC ID of an add, or the parent HTLC ID of a removal.
	ID uint64
}

// Ledger is the commitment protocol state of a channel from our point of
// view: both update logs and both commitment chains. It carries no amounts,
// scripts or signatures, which the channel itself checks. Every method is a
// pure function that returns a new ledger and leaves the receiver unchanged,
// so a transition can compute what an event does before anything happens.
type Ledger struct {
	// Logs holds our updates (Local) and the remote party's (Remote).
	Logs lntypes.Dual[Log]

	// Chains holds our commitment chain (Local) and the remote party's
	// (Remote).
	Chains lntypes.Dual[Chain]

	// Initiator is the party that opened the channel, which is the only
	// one that may propose fee updates.
	Initiator lntypes.ChannelParty

	// LastWasRevoke is set if the last commitment message we sent was a
	// revoke_and_ack rather than a commitment_signed.
	LastWasRevoke bool
}

// clone returns a deep copy of the ledger.
func (l Ledger) clone() Ledger {
	l.Logs = lntypes.Dual[Log]{
		Local:  l.Logs.Local.clone(),
		Remote: l.Logs.Remote.clone(),
	}

	return l
}

// logOf returns a pointer to the given party's log.
func logOf(l *Ledger, p lntypes.ChannelParty) *Log {
	if p.IsLocal() {
		return &l.Logs.Local
	}

	return &l.Logs.Remote
}

// chainOf returns a pointer to the given party's chain.
func chainOf(l *Ledger, p lntypes.ChannelParty) *Chain {
	if p.IsLocal() {
		return &l.Chains.Local
	}

	return &l.Chains.Remote
}

// AddHtlc returns the ledger after the given party proposes an HTLC with the
// given ID, which must be the next ID of its log.
func (l Ledger) AddHtlc(from lntypes.ChannelParty, id uint64) (Ledger,
	error) {

	if want := logOf(&l, from).HtlcCounter; id != want {
		return l, &ErrHtlcID{Got: id, Want: want}
	}

	next := l.clone()
	log := logOf(&next, from)
	log.append(Entry{Kind: KindAdd, HtlcIndex: id})
	log.HtlcCounter++

	return next, nil
}

// Committed reports whether an entry is included in the given party's
// current commitment.
func (l Ledger) Committed(e Entry, chain lntypes.ChannelParty) bool {
	h := e.Heights.GetForParty(chain)
	return h != 0 && h <= chainOf(&l, chain).Tail.Height
}

// LockedIn reports whether an entry is irrevocably committed: included in
// both parties' current commitments.
func (l Ledger) LockedIn(e Entry) bool {
	return l.Committed(e, lntypes.Local) && l.Committed(e, lntypes.Remote)
}

// RemoveHtlc returns the ledger after the given party proposes to remove an
// HTLC the other party offered.
//
// BOLT 2 sets a different bar for each side. A sender must not remove an
// HTLC until it is irrevocably committed in both commitments, so our own
// removals require that. A receiver must fail the channel if the removal
// names an HTLC that isn't in its current commitment, so the remote party's
// removals require the HTLC be in our current commitment.
func (l Ledger) RemoveHtlc(from lntypes.ChannelParty, kind Kind,
	id uint64) (Ledger, error) {

	if !kind.isRemoval() {
		return l, fmt.Errorf("%v is not a removal", kind)
	}

	owner := from.CounterParty()
	log := logOf(&l, owner)
	i := log.add(id)
	if i < 0 {
		return l, &ErrUnknownHtlc{Log: owner, ID: id}
	}
	if log.modified(id) {
		return l, &ErrHtlcRemoved{Log: owner, ID: id}
	}

	add := log.Entries[i]
	ok := l.LockedIn(add)
	if from.IsRemote() {
		ok = l.Committed(add, lntypes.Local)
	}
	if !ok {
		return l, &ErrHtlcNotCommitted{Log: owner, ID: id}
	}

	next := l.clone()
	logOf(&next, from).append(Entry{Kind: kind, ParentIndex: id})
	logOf(&next, owner).markModified(id)

	return next, nil
}

// UpdateFee returns the ledger after the given party proposes a fee update.
// Like lnd, a fee update that follows another one that no commitment
// includes yet replaces it rather than taking a new log entry.
func (l Ledger) UpdateFee(from lntypes.ChannelParty) (Ledger, error) {
	if from != l.Initiator {
		return l, ErrFeeUpdateNotInitiator
	}

	log := logOf(&l, from)
	for i := len(log.Entries) - 1; i >= 0; i-- {
		e := log.Entries[i]
		if e.Kind != KindFee {
			continue
		}
		if e.Heights.Local == 0 && e.Heights.Remote == 0 {
			return l, nil
		}

		break
	}

	next := l.clone()
	logOf(&next, from).append(Entry{Kind: KindFee})

	return next, nil
}

// OweCommitment reports whether the given party has updates, of its own or
// acknowledged from the other side, that the other party's newest commitment
// doesn't include yet. It mirrors lnd's oweCommitment.
func (l Ledger) OweCommitment(p lntypes.ChannelParty) bool {
	ownTip := chainOf(&l, p).Tip()
	otherTip := chainOf(&l, p.CounterParty()).Tip()

	ownPending := logOf(&l, p).LogIndex !=
		otherTip.MsgIdx.GetForParty(p)
	ackedPending := ownTip.MsgIdx.GetForParty(p.CounterParty()) !=
		otherTip.MsgIdx.GetForParty(p.CounterParty())

	return ownPending || ackedPending
}

// extend returns the ledger with a new commitment appended to the given
// party's chain, including every update below idx, and every entry it newly
// includes stamped with its height.
func (l Ledger) extend(chain lntypes.ChannelParty,
	idx lntypes.Dual[uint64]) Ledger {

	next := l.clone()
	c := chainOf(&next, chain)
	commit := Commit{Height: c.Tail.Height + 1, MsgIdx: idx}
	c.Pending = fn.Some(commit)

	for _, p := range parties {
		log := logOf(&next, p)
		for i := range log.Entries {
			e := &log.Entries[i]
			if e.LogIndex >= idx.GetForParty(p) {
				continue
			}
			if e.Heights.GetForParty(chain) == 0 {
				e.Heights.SetForParty(chain, commit.Height)
			}
		}
	}

	return next
}

// SignCommitment returns the ledger after we sign a new commitment for the
// remote party. It includes all of our updates, and the remote party's
// updates that our current commitment includes, which are the ones we've
// acknowledged by revoking.
func (l Ledger) SignCommitment() (Ledger, error) {
	if l.Chains.Remote.Pending.IsSome() {
		return l, ErrNoRevocationWindow
	}

	next := l.extend(lntypes.Remote, lntypes.Dual[uint64]{
		Local:  l.Logs.Local.LogIndex,
		Remote: l.Chains.Local.Tail.MsgIdx.Remote,
	})
	next.LastWasRevoke = false

	return next, nil
}

// ReceiveCommitment returns the ledger after the remote party signs a new
// commitment for us. It includes all of their updates, and our updates that
// their current commitment includes.
func (l Ledger) ReceiveCommitment() (Ledger, error) {
	if l.Chains.Local.Pending.IsSome() {
		return l, ErrUnexpectedCommitment
	}

	return l.extend(lntypes.Local, lntypes.Dual[uint64]{
		Local:  l.Chains.Remote.Tail.MsgIdx.Local,
		Remote: l.Logs.Remote.LogIndex,
	}), nil
}

// RevokeCommitment returns the ledger after we revoke our current commitment
// in favor of the new one the remote party signed.
func (l Ledger) RevokeCommitment() (Ledger, error) {
	pending, err := l.Chains.Local.Pending.UnwrapOrErr(ErrNothingToRevoke)
	if err != nil {
		return l, err
	}

	next := l.clone()
	next.Chains.Local = Chain{Tail: pending}
	next.LastWasRevoke = true

	return next, nil
}

// ReceiveRevocation returns the ledger after the remote party revokes its
// current commitment in favor of the one we signed, along with the remote
// updates this locks in, in log order.
//
// An update is forwarded on the one revocation that makes it irrevocably
// committed: the one whose new remote tail is the first remote commitment
// to include it. Our commitment includes a remote update before theirs
// does, since we only sign remote updates we've acknowledged, so it is
// already in our current commitment by then. A height is set once and the
// remote tail only grows, so no update is ever forwarded twice.
func (l Ledger) ReceiveRevocation() (Ledger, []ForwardRef, error) {
	pending, err := l.Chains.Remote.Pending.UnwrapOrErr(
		ErrUnexpectedRevocation,
	)
	if err != nil {
		return l, nil, err
	}

	next := l.clone()
	next.Chains.Remote = Chain{Tail: pending}

	var fwds []ForwardRef
	for _, e := range next.Logs.Remote.Entries {
		if e.Kind == KindFee {
			continue
		}
		if e.Heights.Remote != pending.Height ||
			!next.Committed(e, lntypes.Local) {

			continue
		}

		id := e.HtlcIndex
		if e.Kind.isRemoval() {
			id = e.ParentIndex
		}
		fwds = append(fwds, ForwardRef{Kind: e.Kind, ID: id})
	}

	compact(&next)

	return next, fwds, nil
}

// compact drops the entries no unrevoked commitment needs anymore: every
// removal and fee update both current commitments include, and the add each
// such removal removes. It mirrors lnd's compactLogs.
func compact(l *Ledger) {
	done := func(e Entry) bool {
		return e.Kind != KindAdd && l.LockedIn(e)
	}

	for _, p := range parties {
		log, other := logOf(l, p), logOf(l, p.CounterParty())

		var gone []uint64
		for _, e := range log.Entries {
			if done(e) && e.Kind.isRemoval() {
				gone = append(gone, e.ParentIndex)
			}
		}
		log.Entries = slices.DeleteFunc(log.Entries, done)

		for _, id := range gone {
			other.Entries = slices.DeleteFunc(other.Entries,
				func(e Entry) bool {
					return e.Kind == KindAdd &&
						e.HtlcIndex == id
				},
			)
			other.unmarkModified(id)
		}
	}
}
