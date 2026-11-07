package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// externalAttemptIndexBucketKey is the top-level bucket that maps the
	// idempotency key chosen by an external dispatcher to the internal
	// attempt ID the switch allocated for it.
	externalAttemptIndexBucketKey = []byte("external-attempt-index")

	// ErrAttemptFingerprintMismatch is returned when an external attempt
	// key is already registered for a request that differs from the one
	// presented. The registered attempt may be in flight, so the caller
	// must neither treat this as a dispatch failure nor reuse the key.
	ErrAttemptFingerprintMismatch = errors.New("attempt key is " +
		"registered for a different request")

	// ErrEmptyExternalKey is returned when an external attempt key is
	// empty.
	ErrEmptyExternalKey = errors.New("external attempt key is empty")

	// errInternalIDInUse is returned internally when a freshly allocated
	// internal attempt ID already has a record in the result store. This
	// can only happen for records written under IDs that were not
	// allocated by the switch's own sequencer. Nothing is written, and the
	// registration is reported as ambiguous so that a retry draws the next
	// ID.
	errInternalIDInUse = errors.New("internal attempt ID already in use")

	// errUnreadableIndex is returned when an index entry cannot be
	// decoded, so the attempt it names is unknown.
	errUnreadableIndex = errors.New("unreadable external index entry")
)

const (
	// externalIndexVersion is the encoding version of an index entry. It
	// is bumped when the entry layout changes or when the request
	// fingerprint is computed differently, so that an entry is only read
	// and compared by a build that knows how it was written.
	externalIndexVersion byte = 0

	// externalIndexEntryLen is the length of an encoded index entry: a
	// version byte, the 8-byte internal attempt ID and a 32-byte request
	// fingerprint.
	externalIndexEntryLen = 1 + 8 + 32
)

// ExternalAttemptStore indexes attempts dispatched by an external router.
// The external router names each attempt with an idempotency key of its own
// choosing, and the store maps that key to an internal attempt ID allocated
// from the switch's own sequencer. Everything inside the switch keeps working
// on the internal ID, so the external key never enters the switch's own
// number space.
type ExternalAttemptStore interface {
	// InitAttempt registers the attempt named by key. On first
	// sight it allocates an internal attempt ID with nextID, and writes the
	// index entry and the pending placeholder in one transaction. If the
	// key is already registered with the same fingerprint, the existing
	// internal ID is returned together with ErrPaymentIDAlreadyExists. If
	// it is registered with a different fingerprint,
	// ErrAttemptFingerprintMismatch is returned. An empty key is refused
	// with ErrEmptyExternalKey before anything is read or written. Any
	// other error is wrapped in ErrAmbiguousAttemptInit.
	InitAttempt(key []byte, fingerprint [32]byte,
		nextID func() (uint64, error)) (uint64, error)

	// LookupAttempt returns the internal attempt ID for key, or
	// ErrPaymentIDNotFound if the key is not registered.
	LookupAttempt(key []byte) (uint64, error)

	// DeleteAttempts removes terminal attempts named by keys,
	// deleting the record and its index entry together. The returned
	// statuses are in the same order as keys. A key that appears more
	// than once fails the whole call, and nothing is deleted.
	DeleteAttempts(keys [][]byte) ([]DeletionStatus, error)
}

// externalIndexEntry is the decoded value of an index entry.
type externalIndexEntry struct {
	attemptID   uint64
	fingerprint [32]byte
}

// encode serializes the index entry.
func (e *externalIndexEntry) encode() []byte {
	b := make([]byte, externalIndexEntryLen)
	b[0] = externalIndexVersion
	binary.BigEndian.PutUint64(b[1:9], e.attemptID)
	copy(b[9:], e.fingerprint[:])

	return b
}

// decodeExternalIndexEntry deserializes an index entry.
func decodeExternalIndexEntry(b []byte) (*externalIndexEntry, error) {
	if len(b) != externalIndexEntryLen {
		return nil, fmt.Errorf("invalid external index entry length "+
			"%d", len(b))
	}
	if b[0] != externalIndexVersion {
		return nil, fmt.Errorf("unknown external index entry version "+
			"%d", b[0])
	}

	e := &externalIndexEntry{
		attemptID: binary.BigEndian.Uint64(b[1:9]),
	}
	copy(e.fingerprint[:], b[9:])

	return e, nil
}

// fetchExternalIndexEntry reads the index entry for key within tx. It
// returns ErrPaymentIDNotFound if the key is not registered.
func fetchExternalIndexEntry(tx kvdb.RTx,
	key []byte) (*externalIndexEntry, error) {

	index := tx.ReadBucket(externalAttemptIndexBucketKey)
	if index == nil {
		return nil, ErrPaymentIDNotFound
	}

	entryBytes := index.Get(key)
	if entryBytes == nil {
		return nil, ErrPaymentIDNotFound
	}

	return decodeExternalIndexEntry(entryBytes)
}

// fetchIndexedAttemptIDs returns the internal attempt IDs named in the
// external attempt index. It returns errUnreadableIndex if any entry cannot be
// decoded.
func fetchIndexedAttemptIDs(tx kvdb.RTx) (map[uint64]struct{}, error) {
	ids := make(map[uint64]struct{})

	index := tx.ReadBucket(externalAttemptIndexBucketKey)
	if index == nil {
		return ids, nil
	}

	err := kvdb.ForAll(index, func(k, v []byte) error {
		entry, err := decodeExternalIndexEntry(v)
		if err != nil {
			return fmt.Errorf("%w: key %x: %w", errUnreadableIndex,
				k, err)
		}
		ids[entry.attemptID] = struct{}{}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// resultExists reports whether any record exists for the attempt ID.
func resultExists(tx kvdb.RTx, attemptID uint64) bool {
	results := tx.ReadBucket(networkResultStoreBucketKey)
	if results == nil {
		return false
	}

	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], attemptID)

	return results.Get(attemptIDBytes[:]) != nil
}

// InitAttempt registers an externally keyed attempt.
//
// NOTE: This is part of the ExternalAttemptStore interface.
func (store *networkResultStore) InitAttempt(key []byte,
	fingerprint [32]byte, nextID func() (uint64, error)) (uint64, error) {

	if len(key) == 0 {
		return 0, ErrEmptyExternalKey
	}

	// Serialize every operation on this key. Under the lock the index
	// entry for the key cannot change, so the lookup below and the write
	// that follows it cannot race with another call for the same key.
	keyStr := string(key)
	store.externalKeyMtx.Lock(keyStr)
	defer store.externalKeyMtx.Unlock(keyStr)

	var existing *externalIndexEntry
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		existing, err = fetchExternalIndexEntry(tx, key)

		return err
	}, func() {
		existing = nil
	})
	switch {
	case err == nil:
		if existing.fingerprint != fingerprint {
			return 0, ErrAttemptFingerprintMismatch
		}

		return existing.attemptID, ErrPaymentIDAlreadyExists

	case !errors.Is(err, ErrPaymentIDNotFound):
		return 0, fmt.Errorf("%w: %w", ErrAmbiguousAttemptInit, err)
	}

	// The key is new. Allocate an internal ID outside of any database
	// transaction: the sequencer writes to the same database, so calling
	// it inside the transaction below would deadlock.
	attemptID, err := nextID()
	if err != nil {
		return 0, fmt.Errorf("%w: unable to allocate attempt ID: %w",
			ErrAmbiguousAttemptInit, err)
	}

	// If the ID already has a record, nothing is written and the
	// registration is ambiguous. The sequencer has moved on, so a retry
	// of the same key draws the next ID.
	err = store.initIndexedAttempt(key, attemptID, fingerprint)
	if errors.Is(err, errInternalIDInUse) {
		log.Warnf("Allocated attempt ID %d already has a record",
			attemptID)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrAmbiguousAttemptInit, err)
	}

	log.Debugf("Initialized external attempt key=%x with attemptID=%d",
		key, attemptID)

	return attemptID, nil
}

// initIndexedAttempt writes the pending placeholder for attemptID and the
// index entry for key in one transaction.
func (store *networkResultStore) initIndexedAttempt(key []byte,
	attemptID uint64, fingerprint [32]byte) error {

	// Take the attempt ID lock as well, to keep the placeholder write
	// ordered against result subscribers. The ID is freshly allocated, so
	// this never contends in practice.
	store.attemptIDMtx.Lock(attemptID)
	defer store.attemptIDMtx.Unlock(attemptID)

	return kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		if resultExists(tx, attemptID) {
			return errInternalIDInUse
		}

		index, err := tx.CreateTopLevelBucket(
			externalAttemptIndexBucketKey,
		)
		if err != nil {
			return err
		}

		// The key lock guarantees no entry was written since the
		// lookup, so finding one here is an invariant violation.
		if index.Get(key) != nil {
			return fmt.Errorf("index entry for key %x appeared "+
				"under lock", key)
		}

		if err := putPendingPlaceholder(tx, attemptID); err != nil {
			return err
		}

		entry := &externalIndexEntry{
			attemptID:   attemptID,
			fingerprint: fingerprint,
		}

		return index.Put(key, entry.encode())
	}, func() {})
}

// LookupAttempt returns the internal attempt ID registered for key.
//
// NOTE: This is part of the ExternalAttemptStore interface.
func (store *networkResultStore) LookupAttempt(key []byte) (uint64,
	error) {

	var entry *externalIndexEntry
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		entry, err = fetchExternalIndexEntry(tx, key)

		return err
	}, func() {
		entry = nil
	})
	if err != nil {
		return 0, err
	}

	return entry.attemptID, nil
}

// DeleteAttempts removes terminal attempts named by their external
// keys. A record and its index entry are always deleted together, so an index
// entry never outlives the record it points to.
//
// NOTE: This is part of the ExternalAttemptStore interface.
func (store *networkResultStore) DeleteAttempts(
	keys [][]byte) ([]DeletionStatus, error) {

	if len(keys) == 0 {
		return nil, nil
	}

	// Reject duplicate keys and lock keys in sorted order, so concurrent
	// callers cannot deadlock on each other.
	seen := make(map[string]struct{}, len(keys))
	sortedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		keyStr := string(key)
		if _, ok := seen[keyStr]; ok {
			return nil, fmt.Errorf("duplicate attempt key %x", key)
		}
		seen[keyStr] = struct{}{}
		sortedKeys = append(sortedKeys, keyStr)
	}
	slices.Sort(sortedKeys)

	for _, keyStr := range sortedKeys {
		store.externalKeyMtx.Lock(keyStr)
	}
	defer func() {
		for _, keyStr := range sortedKeys {
			store.externalKeyMtx.Unlock(keyStr)
		}
	}()

	// Resolve the internal IDs first, so their locks can be taken before
	// the write transaction. The key locks keep the index stable between
	// this read and the write below.
	var attemptIDs []uint64
	err := kvdb.View(store.backend, func(tx kvdb.RTx) error {
		for _, key := range keys {
			entry, err := fetchExternalIndexEntry(tx, key)
			if errors.Is(err, ErrPaymentIDNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			attemptIDs = append(attemptIDs, entry.attemptID)
		}

		return nil
	}, func() {
		attemptIDs = nil
	})
	if err != nil {
		return nil, fmt.Errorf("DeleteAttempts failed: %w", err)
	}

	slices.Sort(attemptIDs)

	// Two keys never share an internal ID in a consistent index, but a
	// repeated ID would deadlock on its own lock, so take each lock once.
	attemptIDs = slices.Compact(attemptIDs)
	for _, id := range attemptIDs {
		store.attemptIDMtx.Lock(id)
	}
	defer func() {
		for _, id := range attemptIDs {
			store.attemptIDMtx.Unlock(id)
		}
	}()

	statuses := make([]DeletionStatus, len(keys))
	err = kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		index := tx.ReadWriteBucket(externalAttemptIndexBucketKey)
		results := tx.ReadWriteBucket(networkResultStoreBucketKey)

		for i, key := range keys {
			status, err := deleteExternalAttempt(
				index, results, key,
			)
			if err != nil {
				return err
			}
			statuses[i] = status
		}

		return nil
	}, func() {
		statuses = make([]DeletionStatus, len(keys))
	})
	if err != nil {
		return nil, fmt.Errorf("DeleteAttempts failed: %w", err)
	}

	return statuses, nil
}

// deleteExternalAttempt deletes the record and index entry for one key if the
// attempt is terminal, and reports the outcome.
func deleteExternalAttempt(index, results kvdb.RwBucket,
	key []byte) (DeletionStatus, error) {

	if index == nil {
		return DeletionNotFound, nil
	}

	entryBytes := index.Get(key)
	if entryBytes == nil {
		return DeletionNotFound, nil
	}

	entry, err := decodeExternalIndexEntry(entryBytes)
	if err != nil {
		log.Warnf("Unable to decode index entry for key %x during "+
			"delete: %v", key, err)

		return DeletionFailed, nil
	}

	var attemptIDBytes [8]byte
	binary.BigEndian.PutUint64(attemptIDBytes[:], entry.attemptID)

	var resultBytes []byte
	if results != nil {
		resultBytes = results.Get(attemptIDBytes[:])
	}

	// An index entry without a record should not exist, since both are
	// written and deleted together. Remove the stray entry and report the
	// attempt as not found.
	if resultBytes == nil {
		log.Warnf("Index entry for key %x points at missing attempt "+
			"%d, removing it", key, entry.attemptID)

		return DeletionNotFound, index.Delete(key)
	}

	result, err := deserializeNetworkResult(bytes.NewReader(resultBytes))
	if err != nil {
		log.Warnf("Unable to deserialize result for attempt %d "+
			"during delete: %v", entry.attemptID, err)

		return DeletionFailed, nil
	}

	if result.msg.MsgType() == pendingHtlcMsgType {
		return DeletionPending, nil
	}

	if err := results.Delete(attemptIDBytes[:]); err != nil {
		return 0, fmt.Errorf("failed to delete attempt %d: %w",
			entry.attemptID, err)
	}
	if err := index.Delete(key); err != nil {
		return 0, fmt.Errorf("failed to delete index entry for key "+
			"%x: %w", key, err)
	}

	return DeletionOK, nil
}
