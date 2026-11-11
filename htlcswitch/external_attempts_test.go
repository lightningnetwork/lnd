package htlcswitch

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testIDSource returns a nextID function that counts up from start, and a
// counter of how many times it was called.
func testIDSource(start uint64) (func() (uint64, error), *atomic.Int64) {
	var (
		next  atomic.Uint64
		calls atomic.Int64
	)
	next.Store(start)

	return func() (uint64, error) {
		calls.Add(1)

		return next.Add(1) - 1, nil
	}, &calls
}

// newTestExternalStore creates a result store backed by a fresh database.
func newTestExternalStore(t *testing.T) (*networkResultStore,
	*channeldb.DB) {

	t.Helper()

	db := channeldb.OpenForTesting(t, t.TempDir())
	store, err := newNetworkResultStore(db, true)
	require.NoError(t, err)

	return store, db
}

// settleAttempt stores a settle result for the internal attempt ID.
func settleAttempt(t *testing.T, store *networkResultStore, id uint64) {
	t.Helper()

	require.NoError(t, store.StoreResult(id, &networkResult{
		msg:         &lnwire.UpdateFulfillHTLC{},
		unencrypted: true,
	}))
}

// TestInitAttempt checks registration of an external key: a new key
// allocates an internal ID, a repeat with the same fingerprint returns that ID
// as a duplicate, and a repeat with a different fingerprint is refused.
func TestInitAttempt(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	nextID, calls := testIDSource(100)

	key := []byte("payment-a/attempt-1")
	fp := [32]byte{1}

	// A new key allocates an internal ID and writes a pending
	// placeholder for it.
	id, err := store.InitAttempt(key, fp, nextID)
	require.NoError(t, err)
	require.EqualValues(t, 100, id)
	require.EqualValues(t, 1, calls.Load())

	_, err = store.GetResult(id)
	require.ErrorIs(t, err, ErrAttemptResultPending)

	got, err := store.LookupAttempt(key)
	require.NoError(t, err)
	require.Equal(t, id, got)

	// The same key with the same fingerprint is a duplicate. It returns
	// the original internal ID and allocates nothing.
	dupID, err := store.InitAttempt(key, fp, nextID)
	require.ErrorIs(t, err, ErrPaymentIDAlreadyExists)
	require.Equal(t, id, dupID)
	require.EqualValues(t, 1, calls.Load())

	// The same key with a different fingerprint is refused.
	_, err = store.InitAttempt(key, [32]byte{2}, nextID)
	require.ErrorIs(t, err, ErrAttemptFingerprintMismatch)
	require.EqualValues(t, 1, calls.Load())

	// A different key gets a different internal ID.
	otherID, err := store.InitAttempt(
		[]byte("payment-b/attempt-1"), fp, nextID,
	)
	require.NoError(t, err)
	require.NotEqual(t, id, otherID)

	// An empty key is rejected.
	_, err = store.InitAttempt(nil, fp, nextID)
	require.ErrorIs(t, err, ErrEmptyExternalKey)
}

// TestInitAttemptAllocationError checks that a failure to allocate an
// internal ID is reported as ambiguous and leaves no index entry behind.
func TestInitAttemptAllocationError(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	failing := func() (uint64, error) {
		return 0, errors.New("sequencer unavailable")
	}

	key := []byte("key")
	_, err := store.InitAttempt(key, [32]byte{}, failing)
	require.ErrorIs(t, err, ErrAmbiguousAttemptInit)

	_, err = store.LookupAttempt(key)
	require.ErrorIs(t, err, ErrPaymentIDNotFound)
}

// TestInitAttemptUsedID checks that an allocated internal ID which already
// has a record, for example one written under an attempt ID the switch's
// sequencer did not allocate, is never reused. The registration is ambiguous
// and writes nothing, and a retry draws the next ID.
func TestInitAttemptUsedID(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	key := []byte("key")

	// A record left under ID 5 by an earlier dispatcher.
	settleAttempt(t, store, 5)

	nextID, calls := testIDSource(5)
	_, err := store.InitAttempt(key, [32]byte{}, nextID)
	require.ErrorIs(t, err, ErrAmbiguousAttemptInit)

	// Nothing was registered for the key, and the existing record is
	// still the settled result.
	_, err = store.LookupAttempt(key)
	require.ErrorIs(t, err, ErrPaymentIDNotFound)

	var existing *networkResult
	err = kvdb.View(store.backend, func(tx kvdb.RTx) error {
		var err error
		existing, err = fetchResult(tx, 5)

		return err
	}, func() {})
	require.NoError(t, err)
	require.IsType(t, &lnwire.UpdateFulfillHTLC{}, existing.msg)

	// A retry draws the next ID and registers the key under it.
	id, err := store.InitAttempt(key, [32]byte{}, nextID)
	require.NoError(t, err)
	require.EqualValues(t, 6, id)
	require.EqualValues(t, 2, calls.Load())
}

// TestInitAttemptConcurrent checks that concurrent registrations of
// the same key yield exactly one internal ID.
func TestInitAttemptConcurrent(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	nextID, calls := testIDSource(1)

	const workers = 16
	var (
		wg       sync.WaitGroup
		ids      = make([]uint64, workers)
		errs     = make([]error, workers)
		key      = []byte("same-key")
		fp       = [32]byte{7}
		startSig = make(chan struct{})
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startSig
			ids[i], errs[i] = store.InitAttempt(
				key, fp, nextID,
			)
		}()
	}
	close(startSig)
	wg.Wait()

	var fresh int
	for i := range workers {
		switch {
		case errs[i] == nil:
			fresh++
		default:
			require.ErrorIs(t, errs[i], ErrPaymentIDAlreadyExists)
		}
		require.Equal(t, ids[0], ids[i])
	}
	require.Equal(t, 1, fresh)
	require.EqualValues(t, 1, calls.Load())
}

// TestDeleteAttempts checks that terminal attempts are deleted
// together with their index entries, pending attempts are kept, and statuses
// follow the order of the requested keys.
func TestDeleteAttempts(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	nextID, _ := testIDSource(1)

	pendingKey := []byte("pending")
	settledKey := []byte("settled")
	unknownKey := []byte("unknown")

	_, err := store.InitAttempt(pendingKey, [32]byte{}, nextID)
	require.NoError(t, err)

	settledID, err := store.InitAttempt(
		settledKey, [32]byte{}, nextID,
	)
	require.NoError(t, err)
	settleAttempt(t, store, settledID)

	statuses, err := store.DeleteAttempts(
		[][]byte{settledKey, unknownKey, pendingKey},
	)
	require.NoError(t, err)
	require.Equal(t, []DeletionStatus{
		DeletionOK, DeletionNotFound, DeletionPending,
	}, statuses)

	// The settled attempt's record and index entry are both gone.
	_, err = store.LookupAttempt(settledKey)
	require.ErrorIs(t, err, ErrPaymentIDNotFound)
	_, err = store.GetResult(settledID)
	require.ErrorIs(t, err, ErrPaymentIDNotFound)

	// The pending attempt is untouched.
	pendingID, err := store.LookupAttempt(pendingKey)
	require.NoError(t, err)
	_, err = store.GetResult(pendingID)
	require.ErrorIs(t, err, ErrAttemptResultPending)

	// Registering a deleted key again allocates a new internal ID.
	newID, err := store.InitAttempt(settledKey, [32]byte{}, nextID)
	require.NoError(t, err)
	require.NotEqual(t, settledID, newID)

	// Duplicate keys in one request are rejected.
	_, err = store.DeleteAttempts([][]byte{pendingKey, pendingKey})
	require.Error(t, err)

	// An empty request is a no-op.
	statuses, err = store.DeleteAttempts(nil)
	require.NoError(t, err)
	require.Empty(t, statuses)
}

// TestDeleteAttemptsSharedInternalID checks that DeleteAttempts returns when
// two keys in one request name the same internal ID. A consistent index never
// holds such entries, but a repeated ID must not deadlock on its own lock.
func TestDeleteAttemptsSharedInternalID(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	nextID, _ := testIDSource(1)

	keyA := []byte("key-a")
	keyB := []byte("key-b")

	id, err := store.InitAttempt(keyA, [32]byte{1}, nextID)
	require.NoError(t, err)
	settleAttempt(t, store, id)

	// Point a second key at the same internal ID.
	entry := externalIndexEntry{attemptID: id, fingerprint: [32]byte{1}}
	err = kvdb.Update(store.backend, func(tx kvdb.RwTx) error {
		index := tx.ReadWriteBucket(externalAttemptIndexBucketKey)

		return index.Put(keyB, entry.encode())
	}, func() {})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteAttempts([][]byte{keyA, keyB})
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("DeleteAttempts deadlocked on a shared internal ID")
	}
}

// TestExternalAttemptFailedPending checks that an attempt failed in place, as
// orphan cleanup does on restart, stays reachable through its key and can then
// be deleted.
func TestExternalAttemptFailedPending(t *testing.T) {
	t.Parallel()

	store, db := newTestExternalStore(t)
	nextID, _ := testIDSource(1)

	key := []byte("key")
	id, err := store.InitAttempt(key, [32]byte{}, nextID)
	require.NoError(t, err)

	require.NoError(t, store.FailPendingAttempt(
		id, NewLinkError(&lnwire.FailTemporaryNodeFailure{}),
	))

	// A store reopened on the same database still resolves the key to
	// the failed record.
	reopened, err := newNetworkResultStore(db, true)
	require.NoError(t, err)

	got, err := reopened.LookupAttempt(key)
	require.NoError(t, err)
	require.Equal(t, id, got)

	result, err := reopened.GetResult(id)
	require.NoError(t, err)
	require.IsType(t, &lnwire.UpdateFailHTLC{}, result.msg)

	statuses, err := reopened.DeleteAttempts([][]byte{key})
	require.NoError(t, err)
	require.Equal(t, []DeletionStatus{DeletionOK}, statuses)
}

// TestDisableRemoteRouterExternalIndex checks that an index entry prevents
// the remote router marker from being removed.
func TestDisableRemoteRouterExternalIndex(t *testing.T) {
	t.Parallel()

	store, _ := newTestExternalStore(t)
	nextID, _ := testIDSource(1)

	key := []byte("key")
	id, err := store.InitAttempt(key, [32]byte{}, nextID)
	require.NoError(t, err)
	settleAttempt(t, store, id)

	require.ErrorIs(t, store.DisableRemoteRouter(), ErrAttemptEntriesExist)

	statuses, err := store.DeleteAttempts([][]byte{key})
	require.NoError(t, err)
	require.Equal(t, []DeletionStatus{DeletionOK}, statuses)

	require.NoError(t, store.DisableRemoteRouter())
}
