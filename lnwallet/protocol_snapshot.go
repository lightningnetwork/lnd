package lnwallet

import (
	"slices"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SnapshotUpdateKind is the kind of an entry in a ProtocolSnapshot's update
// log.
type SnapshotUpdateKind uint8

const (
	// SnapshotAdd is an HTLC add, including a noop add.
	SnapshotAdd SnapshotUpdateKind = iota

	// SnapshotSettle settles the HTLC named by the entry's ParentIndex.
	SnapshotSettle

	// SnapshotFail fails the HTLC named by the entry's ParentIndex.
	SnapshotFail

	// SnapshotMalformedFail fails the HTLC named by the entry's
	// ParentIndex as malformed.
	SnapshotMalformedFail

	// SnapshotFeeUpdate is a fee update.
	SnapshotFeeUpdate
)

// String returns the name of the kind.
func (k SnapshotUpdateKind) String() string {
	switch k {
	case SnapshotAdd:
		return "Add"
	case SnapshotSettle:
		return "Settle"
	case SnapshotFail:
		return "Fail"
	case SnapshotMalformedFail:
		return "MalformedFail"
	case SnapshotFeeUpdate:
		return "FeeUpdate"
	default:
		return "Unknown"
	}
}

// SnapshotUpdate is one entry of an update log, reduced to the fields that
// decide the commitment protocol: which commitments include it, and which
// HTLC it adds or removes.
type SnapshotUpdate struct {
	// Kind is the kind of update.
	Kind SnapshotUpdateKind

	// LogIndex is the entry's index in its update log.
	LogIndex uint64

	// HtlcIndex is the HTLC's index for an add, and zero otherwise.
	HtlcIndex uint64

	// ParentIndex is the index of the HTLC a removal targets, which lives
	// in the other party's log, and zero otherwise.
	ParentIndex uint64

	// AddHeights is, for each party's commitment chain, the height of
	// the first commitment that includes this add or fee update, or zero
	// if none does yet.
	AddHeights lntypes.Dual[uint64]

	// RemoveHeights is, for each party's commitment chain, the height of
	// the first commitment that includes this removal or fee update, or
	// zero if none does yet.
	RemoveHeights lntypes.Dual[uint64]

	// Forwarded is set once a remote update has been placed in a
	// forwarding package in this session.
	Forwarded bool
}

// SnapshotLog is one party's update log.
type SnapshotLog struct {
	// LogIndex is the index the next update will take.
	LogIndex uint64

	// HtlcCounter is the index the next HTLC add will take.
	HtlcCounter uint64

	// Updates are the entries still in the log, in log order.
	Updates []SnapshotUpdate

	// Modified are the HTLC indexes of this log's adds that the other
	// party has already targeted with a removal, in ascending order.
	Modified []uint64
}

// SnapshotCommit is one commitment of a commitment chain.
type SnapshotCommit struct {
	// Height is the commitment's height.
	Height uint64

	// MessageIndices are the log indexes, for each party's log, up to
	// which this commitment includes updates.
	MessageIndices lntypes.Dual[uint64]
}

// SnapshotChain is one party's commitment chain.
type SnapshotChain struct {
	// Commits are the unrevoked commitments, tail first.
	Commits []SnapshotCommit
}

// ProtocolSnapshot is an abstract view of the commitment protocol state of a
// channel: the two update logs and the two commitment chains, without any
// amounts, scripts or signatures. It exists so that a model of the protocol
// can be checked against, and restored from, the channel.
type ProtocolSnapshot struct {
	// Logs holds our update log (Local) and the remote party's (Remote).
	Logs lntypes.Dual[SnapshotLog]

	// Chains holds our commitment chain (Local) and the remote party's
	// (Remote).
	Chains lntypes.Dual[SnapshotChain]

	// LastWasRevoke is set if the last commitment message we sent was a
	// revoke_and_ack rather than a commitment_signed, which decides the
	// order of retransmission during channel_reestablish. It is the value
	// persisted when the channel was loaded: lnd writes it to disk but
	// doesn't update it in memory, so it is only current for a channel
	// that was just loaded, which is the only kind that reestablishes.
	LastWasRevoke bool
}

// ProtocolSnapshot returns an abstract view of the channel's commitment
// protocol state.
func (lc *LightningChannel) ProtocolSnapshot() *ProtocolSnapshot {
	lc.RLock()
	defer lc.RUnlock()

	return &ProtocolSnapshot{
		Logs: lntypes.Dual[SnapshotLog]{
			Local:  snapshotLog(lc.updateLogs.Local),
			Remote: snapshotLog(lc.updateLogs.Remote),
		},
		Chains: lntypes.Dual[SnapshotChain]{
			Local:  snapshotChain(lc.commitChains.Local),
			Remote: snapshotChain(lc.commitChains.Remote),
		},
		LastWasRevoke: lc.channelState.LastWasRevoke,
	}
}

// snapshotLog returns the abstract view of an update log.
func snapshotLog(log *updateLog) SnapshotLog {
	s := SnapshotLog{
		LogIndex:    log.logIndex,
		HtlcCounter: log.htlcCounter,
	}

	for e := log.Front(); e != nil; e = e.Next() {
		pd := e.Value

		var kind SnapshotUpdateKind
		switch pd.EntryType {
		case Add, NoOpAdd:
			kind = SnapshotAdd
		case Settle:
			kind = SnapshotSettle
		case Fail:
			kind = SnapshotFail
		case MalformedFail:
			kind = SnapshotMalformedFail
		case FeeUpdate:
			kind = SnapshotFeeUpdate
		}

		u := SnapshotUpdate{
			Kind:          kind,
			LogIndex:      pd.LogIndex,
			AddHeights:    pd.addCommitHeights,
			RemoveHeights: pd.removeCommitHeights,
			Forwarded:     pd.isForwarded,
		}
		switch kind {
		case SnapshotAdd:
			u.HtlcIndex = pd.HtlcIndex
		case SnapshotSettle, SnapshotFail, SnapshotMalformedFail:
			u.ParentIndex = pd.ParentIndex
		}

		s.Updates = append(s.Updates, u)
	}

	s.Modified = log.modifiedHtlcs.ToSlice()
	slices.Sort(s.Modified)

	return s
}

// snapshotChain returns the abstract view of a commitment chain.
func snapshotChain(chain *commitmentChain) SnapshotChain {
	var s SnapshotChain
	for e := chain.commitments.Front(); e != nil; e = e.Next() {
		s.Commits = append(s.Commits, SnapshotCommit{
			Height:         e.Value.height,
			MessageIndices: e.Value.messageIndices,
		})
	}

	return s
}

// ChanSyncMsg returns the channel_reestablish we send the peer when we
// reconnect.
func (lc *LightningChannel) ChanSyncMsg() (*lnwire.ChannelReestablish, error) {
	return lc.channelState.ChanSyncMsg()
}
