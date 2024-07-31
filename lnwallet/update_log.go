package lnwallet

import "container/list"

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
	*list.List

	// updateIndex maps a `logIndex` to a particular update entry. It
	// deals with the four update types:
	//   `Fail|MalformedFail|Settle|FeeUpdate`
	updateIndex map[uint64]*list.Element

	// htlcIndex maps a `htlcCounter` to an offered HTLC entry, hence the
	// `Add` update.
	htlcIndex map[uint64]*list.Element

	// modifiedHtlcs is a set that keeps track of all the current modified
	// htlcs, hence update types `Fail|MalformedFail|Settle`. A modified
	// HTLC is one that's present in the log, and has as a pending fail or
	// settle that's attempting to consume it.
	modifiedHtlcs map[uint64]struct{}
}

// newUpdateLog creates a new updateLog instance.
func newUpdateLog(logIndex, htlcCounter uint64) *updateLog {
	return &updateLog{
		List:          list.New(),
		updateIndex:   make(map[uint64]*list.Element),
		htlcIndex:     make(map[uint64]*list.Element),
		logIndex:      logIndex,
		htlcCounter:   htlcCounter,
		modifiedHtlcs: make(map[uint64]struct{}),
	}
}

// restoreHtlc will "restore" a prior HTLC to the updateLog. We say restore as
// this method is intended to be used when re-covering a prior commitment
// state. This function differs from appendHtlc in that it won't increment
// either of log's counters. If the HTLC is already present, then it is
// ignored.
func (u *updateLog) restoreHtlc(pd *PaymentDescriptor) {
	if _, ok := u.htlcIndex[pd.HtlcIndex]; ok {
		return
	}

	u.htlcIndex[pd.HtlcIndex] = u.PushBack(pd)
}

// appendUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly.
func (u *updateLog) appendUpdate(pd *PaymentDescriptor) {
	u.updateIndex[u.logIndex] = u.PushBack(pd)
	u.logIndex++
}

// restoreUpdate appends a new update to the tip of the updateLog. The entry is
// also added to index accordingly. This function differs from appendUpdate in
// that it won't increment the log index counter.
func (u *updateLog) restoreUpdate(pd *PaymentDescriptor) {
	u.updateIndex[pd.LogIndex] = u.PushBack(pd)
}

// appendHtlc appends a new HTLC offer to the tip of the update log. The entry
// is also added to the offer index accordingly.
func (u *updateLog) appendHtlc(pd *PaymentDescriptor) {
	u.htlcIndex[u.htlcCounter] = u.PushBack(pd)
	u.htlcCounter++

	u.logIndex++
}

// lookupHtlc attempts to look up an offered HTLC according to its offer
// index. If the entry isn't found, then a nil pointer is returned.
func (u *updateLog) lookupHtlc(i uint64) *PaymentDescriptor {
	htlc, ok := u.htlcIndex[i]
	if !ok {
		return nil
	}

	//nolint:forcetypeassert
	return htlc.Value.(*PaymentDescriptor)
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

	delete(u.modifiedHtlcs, i)
}

// htlcHasModification returns true if the HTLC identified by the passed index
// has a pending modification within the log.
func (u *updateLog) htlcHasModification(i uint64) bool {
	_, o := u.modifiedHtlcs[i]
	return o
}

// markHtlcModified marks an HTLC as modified based on its HTLC index. After a
// call to this method, htlcHasModification will return true until the HTLC is
// removed.
func (u *updateLog) markHtlcModified(i uint64) {
	u.modifiedHtlcs[i] = struct{}{}
}

// compactLogs performs garbage collection within the log removing HTLCs which
// have been removed from the point-of-view of the tail of both chains. The
// entries which timeout/settle HTLCs are also removed.
func compactLogs(ourLog, theirLog *updateLog,
	localChainTail, remoteChainTail uint64) {

	compactLog := func(logA, logB *updateLog) {
		var nextA *list.Element
		for e := logA.Front(); e != nil; e = nextA {
			// Assign next iteration element at top of loop because
			// we may remove the current element from the list,
			// which can change the iterated sequence.
			nextA = e.Next()

			//nolint:forcetypeassert
			htlc := e.Value.(*PaymentDescriptor)

			// We skip Adds, as they will be removed along with the
			// fail/settles below.
			if htlc.EntryType == Add {
				continue
			}

			// If the HTLC hasn't yet been removed from either
			// chain, the skip it.
			if htlc.removeCommitHeightRemote == 0 ||
				htlc.removeCommitHeightLocal == 0 {

				continue
			}

			// Otherwise if the height of the tail of both chains
			// is at least the height in which the HTLC was
			// removed, then evict the settle/timeout entry along
			// with the original add entry.
			if remoteChainTail >= htlc.removeCommitHeightRemote &&
				localChainTail >= htlc.removeCommitHeightLocal {

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
