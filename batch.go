package sphinx

import "errors"

// ErrAlreadyCommitted signals that an entry could not be added to the
// batch because it has already been persisted.
var ErrAlreadyCommitted = errors.New("cannot add to batch after committing")

// Batch is an object used to incrementally construct a set of entries to add to
// the replay log. After construction is completed, it can be added to the log
// using the PutBatch method.
type Batch struct {
	// isCommitted denotes whether or not this batch has been successfully
	// written to disk.
	isCommitted bool

	// id is a unique, caller chosen identifier for this batch.
	id []byte

	// entries stores the set of all potential entries that might get
	// written to the replay log. Some entries may be skipped after
	// examining the on-disk content at the time of commit..
	entries map[uint16]batchEntry

	// replayCache is an in memory lookup-table, which stores the hash
	// prefix of entries already added to this batch. This allows a quick
	// mechanism for intra-batch duplicate detection.
	replayCache map[HashPrefix]struct{}

	// replaySet contains the sequence numbers of all entries that were
	// detected as replays. The set is finalized upon writing the batch to
	// disk, and merges replays detected by the replay cache and on-disk
	// replay log.
	replaySet *ReplaySet
}

// NewBatch initializes an object for constructing a set of entries to
// atomically add to a replay log. Batches are identified by byte slice, which
// allows the caller to safely process the same batch twice and get an
// idempotent result.
func NewBatch(id []byte) *Batch {
	return &Batch{
		id:          id,
		entries:     make(map[uint16]batchEntry),
		replayCache: make(map[HashPrefix]struct{}),
		replaySet:   NewReplaySet(),
	}
}

// Put inserts a hash-prefix/CLTV pair into the current batch. This method only
// returns an error in the event that the batch was already committed to disk.
// Decisions regarding whether or not a particular sequence number is a replay
// is ultimately reported via the batch's ReplaySet after committing to disk.
func (b *Batch) Put(seqNum uint16, hashPrefix *HashPrefix, cltv uint32) error {
	// Abort if this batch was already written to disk.
	if b.isCommitted {
		return ErrAlreadyCommitted
	}

	// Check to see if this hash prefix is already included in this batch.
	// If so, we will opportunistically mark this index as replayed.
	if _, ok := b.replayCache[*hashPrefix]; ok {
		b.replaySet.Add(seqNum)
		return nil
	}

	// Otherwise, this is a distinct hash prefix for this batch. Add it to
	// our list of entries that we will try to write to disk. Each of these
	// entries will be checked again during the commit to see if any other
	// on-disk entries contain the same hash prefix.
	b.entries[seqNum] = batchEntry{
		hashPrefix: *hashPrefix,
		cltv:       cltv,
	}

	// Finally, add this hash prefix to our in-memory replay cache, this
	// will be consulted upon further adds to check for duplicates in the
	// same batch.
	b.replayCache[*hashPrefix] = struct{}{}

	return nil
}

// batchEntry is a tuple of a secret's hash prefix and the corresponding CLTV at
// which the onion blob from which the secret was derived expires.
type batchEntry struct {
	hashPrefix HashPrefix
	cltv       uint32
}
