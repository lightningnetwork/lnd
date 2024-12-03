package lnwallet

import (
	"github.com/lightningnetwork/lnd/fn/v2"
)

// updateLog is an append-only log that stores updates to a node's commitment
// chain. This structure can be seen as the "mempool" within Lightning where
// changes are stored before they're committed to the chain. Once an entry has
// been committed in both the local and remote commitment chain, then it can be
// removed from this log.
//
// TODO(roasbeef): create lightning package, move commitment and update to
// package?
//   - also move state machine, separate from lnwallet package
//   - possible embed updateLog within commitmentChain.
type updateLog struct {
	// logIndex is a monotonically increasing integer that tracks the total
	// number of update entries ever applied to the log. When sending new
	// commitment states, we include all updates up to this index.
	logIndex uint64

	// htlcCounter is a monotonically increasing integer that tracks the
	// total number of offered HTLC's by the owner of this update log,
	// hence the `Add` update type. We use a distinct index for this
	// purpose, as update's that remove entries from the log will be
	// indexed using this counter.
	htlcCounter uint64

	// List is the updatelog itself, we embed this value so updateLog has
	// access to all the method of a list.List.
	*fn.List[*paymentDescriptor]

	// updateIndex maps a `logIndex` to a particular update entry. It
	// deals with the four update types:
	//   `Fail|MalformedFail|Settle|FeeUpdate`
	updateIndex map[uint64]*fn.Node[*paymentDescriptor]

	// htlcIndex maps a `htlcCounter` to an offered HTLC entry, hence the
	// `Add` update.
	htlcIndex map[uint64]*fn.Node[*paymentDescriptor]

	// modifiedHtlcs is a set that keeps track of all the current modified
	// htlcs, hence update types `Fail|MalformedFail|Settle`. A modified
	// HTLC is one that's present in the log, and has as a pending fail or
	// settle that's attempting to consume it.
	modifiedHtlcs fn.Set[uint64]
}

// newUpdateLog creates a new updateLog instance.
func newUpdateLog(logIndex, htlcCounter uint64) *updateLog {
	return &updateLog{
		List:          fn.NewList[*paymentDescriptor](),
		updateIndex:   make(map[uint64]*fn.Node[*paymentDescriptor]),
		htlcIndex:     make(map[uint64]*fn.Node[*paymentDescriptor]),
		logIndex:      logIndex,
		htlcCounter:   htlcCounter,
		modifiedHtlcs: fn.NewSet[uint64](),
	}
}

// restoreHtlc will "restore" a prior HTLC to the updateLog. We say restore as
// this method is intended to be used when re-covering a prior commitment
// state. This function differs from appendHtlc in that it won't increment
// either of log's counters. If the HTLC is already present, then it is
// ignored.
func (u *updateLog) restoreHtlc(pd *paymentDescriptor) {
	if _, ok := u.htlcIndex[pd.HtlcIndex]; ok {
		return
	}

	u.htlcIndex[pd.HtlcIndex] = u.PushBack(pd)
}

// appendUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly.
func (u *updateLog) appendUpdate(pd *paymentDescriptor) {
	u.updateIndex[u.logIndex] = u.PushBack(pd)
	u.logIndex++
}

// restoreUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly. This function differs from appendUpdate in
// that it won't increment the log index counter.
func (u *updateLog) restoreUpdate(pd *paymentDescriptor) {
	u.updateIndex[pd.LogIndex] = u.PushBack(pd)
}

// appendHtlc appends a new HTLC offer to the tip of the update log. The entry
// is also added to the offer index accordingly.
func (u *updateLog) appendHtlc(pd *paymentDescriptor) {
	u.htlcIndex[u.htlcCounter] = u.PushBack(pd)
	u.htlcCounter++

	u.logIndex++
}

// lookupHtlc attempts to look up an offered HTLC according to its offer
// index. If the entry isn't found, then a nil pointer is returned.
func (u *updateLog) lookupHtlc(i uint64) *paymentDescriptor {
	htlc, ok := u.htlcIndex[i]
	if !ok {
		return nil
	}

	return htlc.Value
}

// remove attempts to remove an entry from the update log. If the entry is
// found, then the entry will be removed from the update log and index.
func (u *updateLog) removeUpdate(i uint64) {
	entry := u.updateIndex[i]
	u.Remove(entry)
	delete(u.updateIndex, i)
}

// removeHtlc attempts to remove an HTLC offer form the update log. If the
// entry is found, then the entry will be removed from both the main log and
// the offer index.
func (u *updateLog) removeHtlc(i uint64) {
	entry := u.htlcIndex[i]
	u.Remove(entry)
	delete(u.htlcIndex, i)

	u.modifiedHtlcs.Remove(i)
}

// htlcHasModification returns true if the HTLC identified by the passed index
// has a pending modification within the log.
func (u *updateLog) htlcHasModification(i uint64) bool {
	return u.modifiedHtlcs.Contains(i)
}

// markHtlcModified marks an HTLC as modified based on its HTLC index. After a
// call to this method, htlcHasModification will return true until the HTLC is
// removed.
func (u *updateLog) markHtlcModified(i uint64) {
	u.modifiedHtlcs.Add(i)
}

// compactLogs performs garbage collection within the log removing HTLCs which
// have been removed from the point-of-view of the tail of both chains. The
// entries which timeout/settle HTLCs are also removed.
func compactLogs(ourLog, theirLog *updateLog,
	localChainTail, remoteChainTail uint64) {

	compactLog := func(logA, logB *updateLog) {
		var nextA *fn.Node[*paymentDescriptor]
		for e := logA.Front(); e != nil; e = nextA {
			// Assign next iteration element at top of loop because
			// we may remove the current element from the list,
			// which can change the iterated sequence.
			nextA = e.Next()

			htlc := e.Value
			rmvHeights := htlc.removeCommitHeights

			// We skip Adds, as they will be removed along with the
			// fail/settles below.
			if htlc.EntryType == Add {
				continue
			}

			// If the HTLC hasn't yet been removed from either
			// chain, the skip it.
			if rmvHeights.Remote == 0 || rmvHeights.Local == 0 {
				continue
			}

			// Otherwise if the height of the tail of both chains
			// is at least the height in which the HTLC was
			// removed, then evict the settle/timeout entry along
			// with the original add entry.
			if remoteChainTail >= rmvHeights.Remote &&
				localChainTail >= rmvHeights.Local {

				// Fee updates have no parent htlcs, so we only
				// remove the update itself.
				if htlc.EntryType == FeeUpdate {
					logA.removeUpdate(htlc.LogIndex)
					continue
				}

				// The other types (fail/settle) do have a
				// parent HTLC, so we'll remove that HTLC from
				// the other log.
				logA.removeUpdate(htlc.LogIndex)
				logB.removeHtlc(htlc.ParentIndex)
			}
		}
	}

	compactLog(ourLog, theirLog)
	compactLog(theirLog, ourLog)
}
