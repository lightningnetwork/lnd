package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestNetworkResultSerialization checks that NetworkResults are properly
// (de)serialized.
func TestNetworkResultSerialization(t *testing.T) {
	t.Parallel()

	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		t.Fatalf("unable gen rand preimag: %v", err)
	}

	var chanID lnwire.ChannelID
	if _, err := rand.Read(chanID[:]); err != nil {
		t.Fatalf("unable gen rand chanid: %v", err)
	}

	var reason [256]byte
	if _, err := rand.Read(reason[:]); err != nil {
		t.Fatalf("unable gen rand reason: %v", err)
	}

	settle := &lnwire.UpdateFulfillHTLC{
		ChanID:          chanID,
		ID:              2,
		PaymentPreimage: preimage,
	}

	fail := &lnwire.UpdateFailHTLC{
		ChanID:    chanID,
		ID:        1,
		Reason:    []byte{},
		ExtraData: make([]byte, 0),
	}

	fail2 := &lnwire.UpdateFailHTLC{
		ChanID:    chanID,
		ID:        1,
		Reason:    reason[:],
		ExtraData: make([]byte, 0),
	}

	testCases := []*networkResult{
		{
			msg: settle,
		},
		{
			msg:          fail,
			unencrypted:  false,
			isResolution: false,
		},
		{
			msg:          fail,
			unencrypted:  false,
			isResolution: true,
		},
		{
			msg:          fail2,
			unencrypted:  true,
			isResolution: false,
		},
	}

	for _, p := range testCases {
		var buf bytes.Buffer
		if err := serializeNetworkResult(&buf, p); err != nil {
			t.Fatalf("serialize failed: %v", err)
		}

		r := bytes.NewReader(buf.Bytes())
		p1, err := deserializeNetworkResult(r)
		if err != nil {
			t.Fatalf("unable to deserizlize: %v", err)
		}

		if !reflect.DeepEqual(p, p1) {
			t.Fatalf("not equal. %v vs %v", spew.Sdump(p),
				spew.Sdump(p1))
		}
	}
}

// TestNetworkResultStore tests that the networkResult store behaves as
// expected, and that we can store, get and subscribe to results.
func TestNetworkResultStore(t *testing.T) {
	t.Parallel()

	const numResults = 4

	db := channeldb.OpenForTesting(t, t.TempDir())

	store, err := newNetworkResultStore(db, false)
	require.NoError(t, err)

	var results []*networkResult
	for i := 0; i < numResults; i++ {
		n := &networkResult{
			msg:          &lnwire.UpdateAddHTLC{},
			unencrypted:  true,
			isResolution: true,
		}
		results = append(results, n)
	}

	// Subscribe to 2 of them.
	var subs []<-chan *networkResult
	for i := uint64(0); i < 2; i++ {
		sub, err := store.SubscribeResult(i)
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}
		subs = append(subs, sub)
	}

	// Store three of them.
	for i := uint64(0); i < 3; i++ {
		err := store.StoreResult(i, results[i])
		if err != nil {
			t.Fatalf("unable to store result: %v", err)
		}
	}

	// The two subscribers should be notified.
	for _, sub := range subs {
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("no result received")
		}
	}

	// Let the third one subscribe now. THe result should be received
	// immediately.
	sub, err := store.SubscribeResult(2)
	require.NoError(t, err, "unable to subscribe")
	select {
	case <-sub:
	case <-time.After(1 * time.Second):
		t.Fatalf("no result received")
	}

	// Try fetching the result directly for the non-stored one. This should
	// fail.
	_, err = store.GetResult(3)
	if err != ErrPaymentIDNotFound {
		t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
	}

	// Add the result and try again.
	err = store.StoreResult(3, results[3])
	require.NoError(t, err, "unable to store result")

	_, err = store.GetResult(3)
	require.NoError(t, err, "unable to get result")

	// Since we don't delete results from the store (yet), make sure we
	// will get subscriptions for all of them.
	for i := uint64(0); i < numResults; i++ {
		sub, err := store.SubscribeResult(i)
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}

		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("no result received")
		}
	}

	// Clean the store keeping the first two results.
	toKeep := map[uint64]struct{}{
		0: {},
		1: {},
	}
	// Finally, delete the result.
	err = store.CleanStore(toKeep)
	require.NoError(t, err)

	// Payment IDs 0 and 1 should be found, 2 and 3 should be deleted.
	for i := uint64(0); i < numResults; i++ {
		_, err = store.GetResult(i)
		if i <= 1 {
			require.NoError(t, err, "unable to get result")
		}
		if i >= 2 && err != ErrPaymentIDNotFound {
			t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
		}
	}

	t.Run("InitAttempt duplicate prevention", func(t *testing.T) {
		var id uint64 = 100

		// Fetch the result directly. We expect to observe
		// ErrPaymentIDNotFound since we've not yet initialized this
		// payment attempt.
		_, err = store.GetResult(id)
		require.ErrorIs(t, err, ErrPaymentIDNotFound,
			"expected not found error for uninitialized attempt")

		// First initialization should succeed.
		err := store.InitAttempt(id)
		require.NoError(t, err, "unexpected InitAttempt failure")

		// Now, try fetching the result directly. We expect to observe
		// ErrAttemptResultPending since it's initialized but not
		// yet finalized.
		_, err = store.GetResult(id)
		require.ErrorIs(t, err, ErrAttemptResultPending,
			"expected result unavailable error for pending attempt")

		// Subscribe for the result following the initialization. No
		// result should be received immediately as StoreResult has not
		// yet updated the initialized attempt into a finalized attempt.
		sub, err := store.SubscribeResult(id)
		require.NoError(t, err, "unable to subscribe")
		select {
		case <-sub:
			t.Fatalf("unexpected non-final result notification " +
				"received")
		case <-time.After(1 * time.Second):
		}

		// Second initialization should fail (already initialized).
		// Try initializing an attempt with the same ID before a full
		// settle or fail result is back from the network (simulated by
		// StoreResult).
		err = store.InitAttempt(id)
		require.ErrorIs(t, err, ErrPaymentIDAlreadyExists,
			"expected duplicate InitAttempt to fail")

		// Store a result to simulate a full settle or fail HTLC result
		// coming back from the network.
		netResult := &networkResult{
			msg:          &lnwire.UpdateFulfillHTLC{},
			unencrypted:  true,
			isResolution: true,
		}
		err = store.StoreResult(id, netResult)
		require.NoError(t, err, "unable to store result after init")

		// Try InitAttempt again — still should fail even after a full
		// result is back from the network.
		err = store.InitAttempt(id)
		require.ErrorIs(t, err, ErrPaymentIDAlreadyExists,
			"expected InitAttempt to fail after result stored")

		// Now we confirm that the subscriber is notified of the final
		// attempt result.
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("failed to receive final (settle/fail) result")
		}

		// Verify that ID can be re-used only after explicit deletion of
		// attempts via CleanStore or a DeleteAttempts style method.
		err = store.CleanStore(map[uint64]struct{}{})
		require.NoError(t, err)

		// Now InitAttempt should succeed again.
		err = store.InitAttempt(id)
		require.NoError(t, err, "InitAttempt should succeed after "+
			"cleanup")
	})

	t.Run("Concurrent InitAttempt", func(t *testing.T) {
		var (
			id      uint64 = 999
			wg      sync.WaitGroup
			success atomic.Int32
		)

		// Launch 10 concurrent routines trying to init the same ID.
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := store.InitAttempt(id); err == nil {
					success.Add(1)
				}
			}()
		}
		wg.Wait()

		// Only exactly one call should succeed.
		require.EqualValues(t, 1, success.Load())
	})
}

// TestNetworkResultStoreFailAndFetch tests the FailPendingAttempt and
// FetchPendingAttempts methods of the networkResultStore.
func TestNetworkResultStoreFailAndFetch(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	store, err := newNetworkResultStore(db, false)
	require.NoError(t, err)

	// Test FetchPendingAttempts on an empty store.
	pending, err := store.FetchPendingAttempts()
	require.NoError(t, err, "fetch on empty store failed")
	require.Empty(t, pending, "expected no pending attempts on empty store")

	// Initialize some attempts.
	require.NoError(t, store.InitAttempt(1))
	require.NoError(t, store.InitAttempt(2))
	require.NoError(t, store.InitAttempt(3))

	// Test FetchPendingAttempts with active pending attempts.
	pending, err = store.FetchPendingAttempts()
	require.NoError(t, err, "fetch with pending failed")
	require.ElementsMatch(t, []uint64{1, 2, 3}, pending,
		"unexpected pending attempts")

	// Test FailPendingAttempt.
	failReason := NewLinkError(&lnwire.FailTemporaryNodeFailure{})
	err = store.FailPendingAttempt(2, failReason)
	require.NoError(t, err, "FailPendingAttempt failed")

	// Verify that the failed attempt is no longer pending.
	pending, err = store.FetchPendingAttempts()
	require.NoError(t, err, "fetch after fail failed")
	require.ElementsMatch(t, []uint64{1, 3}, pending,
		"failed attempt should not be pending")

	// Verify that GetResult now returns the correct failure.
	result, err := store.GetResult(2)
	require.NoError(t, err, "GetResult for failed attempt failed")
	require.NotNil(t, result, "result should not be nil")

	failMsg, ok := result.msg.(*lnwire.UpdateFailHTLC)
	require.True(t, ok, "expected an UpdateFailHTLC message")

	// Decode the reason and check that it matches our original failure.
	reason, err := lnwire.DecodeFailure(
		bytes.NewReader(failMsg.Reason), 0,
	)
	require.NoError(t, err, "unable to decode failure reason")

	_, ok = reason.(*lnwire.FailTemporaryNodeFailure)
	require.True(t, ok, "expected temporary node failure")

	// Test misuse of FailPendingAttempt.
	t.Run("FailPendingAttempt misuse", func(t *testing.T) {
		// Fail a non-existent attempt.
		err := store.FailPendingAttempt(999,
			NewLinkError(&lnwire.FailTemporaryNodeFailure{}),
		)
		require.Error(t, err,
			"expected error when failing non-existent attempt")
		require.Contains(t, err.Error(), "not found",
			"expected not found error")

		// Initialize and settle an attempt, then confirm that
		// FailPendingAttempt cannot overwrite the successful result.
		var id uint64 = 100
		require.NoError(t, store.InitAttempt(id), "init attempt failed")
		settleResult := &networkResult{
			msg:         &lnwire.UpdateFulfillHTLC{},
			unencrypted: true,
		}
		require.NoError(t, store.StoreResult(id, settleResult),
			"store settle result failed")

		err = store.FailPendingAttempt(id, NewLinkError(
			&lnwire.FailTemporaryNodeFailure{}),
		)
		require.Error(t, err,
			"expected error when failing settled attempt")

		// Initialize and then store a HTLC failure result from the
		// network. FailPendingAttempt should not overwrite the real
		// failure reason.
		var id2 uint64 = 101
		require.NoError(t, store.InitAttempt(id2),
			"init attempt 2 failed")

		failResult := &networkResult{
			msg:         &lnwire.UpdateFailHTLC{},
			unencrypted: true,
		}
		require.NoError(t, store.StoreResult(id2, failResult),
			"store fail result failed")

		err = store.FailPendingAttempt(
			id2,
			NewLinkError(&lnwire.FailTemporaryNodeFailure{}),
		)
		require.Error(t, err,
			"expected error when failing already failed attempt")
	})
}

// TestDeleteAttempts tests the DeleteAttempts method of the
// networkResultStore, verifying that it correctly deletes terminal attempts,
// refuses to delete pending attempts, and reports per-ID results.
func TestDeleteAttempts(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	store, err := newNetworkResultStore(db, false)
	require.NoError(t, err)

	// Helper to create and store a settled result.
	storeSettled := func(t *testing.T, id uint64) {
		t.Helper()
		require.NoError(t, store.InitAttempt(id))
		settleResult := &networkResult{
			msg:         &lnwire.UpdateFulfillHTLC{},
			unencrypted: true,
		}
		require.NoError(t, store.StoreResult(id, settleResult))
	}

	// Helper to create and store a failed result.
	storeFailed := func(t *testing.T, id uint64) {
		t.Helper()
		require.NoError(t, store.InitAttempt(id))
		failResult := &networkResult{
			msg:         &lnwire.UpdateFailHTLC{},
			unencrypted: true,
		}
		require.NoError(t, store.StoreResult(id, failResult))
	}

	t.Run("empty ID list", func(t *testing.T) {
		results, err := store.DeleteAttempts(nil)
		require.NoError(t, err)
		require.Empty(t, results)

		results, err = store.DeleteAttempts([]uint64{})
		require.NoError(t, err)
		require.Empty(t, results)
	})

	t.Run("delete settled attempt", func(t *testing.T) {
		var id uint64 = 10
		storeSettled(t, id)

		// Verify it exists before deletion.
		_, err := store.GetResult(id)
		require.NoError(t, err)

		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionOK, results[id])

		// Verify the record is gone.
		_, err = store.GetResult(id)
		require.ErrorIs(t, err, ErrPaymentIDNotFound)
	})

	t.Run("delete failed attempt", func(t *testing.T) {
		var id uint64 = 20
		storeFailed(t, id)

		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionOK, results[id])

		// Verify the record is gone.
		_, err = store.GetResult(id)
		require.ErrorIs(t, err, ErrPaymentIDNotFound)
	})

	t.Run("refuse to delete pending attempt", func(t *testing.T) {
		var id uint64 = 30
		require.NoError(t, store.InitAttempt(id))

		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionPending, results[id])

		// Verify the record is still present.
		_, err = store.GetResult(id)
		require.ErrorIs(t, err, ErrAttemptResultPending)
	})

	t.Run("reject duplicate IDs", func(t *testing.T) {
		_, err := store.DeleteAttempts([]uint64{1, 2, 1})
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate attempt ID 1")
	})

	t.Run("delete unknown attempt ID", func(t *testing.T) {
		results, err := store.DeleteAttempts([]uint64{9999})
		require.NoError(t, err)
		require.Equal(t, DeletionNotFound, results[uint64(9999)])
	})

	t.Run("unreadable message", func(t *testing.T) {
		// Write corrupt bytes directly to the store for a known
		// attempt ID.
		var id uint64 = 35
		var attemptIDBytes [8]byte
		binary.BigEndian.PutUint64(attemptIDBytes[:], id)

		err := kvdb.Update(db, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(
				networkResultStoreBucketKey,
			)
			if err != nil {
				return err
			}

			return bucket.Put(
				attemptIDBytes[:], []byte("corrupted-attempt"),
			)
		}, func() {})
		require.NoError(t, err)

		// DeleteAttempts should report DeletionFailed for the
		// unreadable entry.
		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionFailed, results[id])
	})

	t.Run("mixed batch", func(t *testing.T) {
		var (
			settledID uint64 = 40
			failedID  uint64 = 41
			pendingID uint64 = 42
			unknownID uint64 = 43
		)

		storeSettled(t, settledID)
		storeFailed(t, failedID)
		require.NoError(t, store.InitAttempt(pendingID))

		results, err := store.DeleteAttempts([]uint64{
			settledID, failedID, pendingID, unknownID,
		})
		require.NoError(t, err)
		require.Len(t, results, 4)

		require.Equal(t, DeletionOK, results[settledID])
		require.Equal(t, DeletionOK, results[failedID])
		require.Equal(t, DeletionPending, results[pendingID])
		require.Equal(t, DeletionNotFound, results[unknownID])

		// Verify settled and failed are gone.
		_, err = store.GetResult(settledID)
		require.ErrorIs(t, err, ErrPaymentIDNotFound)
		_, err = store.GetResult(failedID)
		require.ErrorIs(t, err, ErrPaymentIDNotFound)

		// Verify pending is still there.
		_, err = store.GetResult(pendingID)
		require.ErrorIs(t, err, ErrAttemptResultPending)
	})

	t.Run("idempotent re-delete", func(t *testing.T) {
		var id uint64 = 50
		storeSettled(t, id)

		// First delete succeeds.
		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionOK, results[id])

		// Second delete reports not found.
		results, err = store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionNotFound, results[id])
	})

	t.Run("InitAttempt succeeds after delete", func(t *testing.T) {
		var id uint64 = 60
		storeSettled(t, id)

		// Delete the attempt.
		results, err := store.DeleteAttempts([]uint64{id})
		require.NoError(t, err)
		require.Equal(t, DeletionOK, results[id])

		// Re-initializing the same ID should now succeed since the
		// record was fully removed.
		err = store.InitAttempt(id)
		require.NoError(t, err)
	})
}

// TestDeleteAttemptsProperties uses property-based testing to verify
// DeleteAttempts invariants hold across randomly generated inputs. Rather than
// hand-picking specific scenarios (like TestDeleteAttempts above), this test
// lets the rapid framework generate hundreds of random mixes of pending,
// settled, failed, and non-existent attempt IDs and checks that key properties
// always hold.
func TestDeleteAttemptsProperties(t *testing.T) {
	t.Parallel()

	// Property 1: Pending records are never deleted. No matter what mix of
	// IDs is requested, pending attempts must always survive.
	t.Run("pending records always survive", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(rt *rapid.T) {
			db := channeldb.OpenForTesting(t, t.TempDir())
			store, err := newNetworkResultStore(db, false)
			require.NoError(t, err)

			// Generate a random number of records.
			numPending := rapid.IntRange(1, 10).Draw(
				rt, "numPending",
			)
			numSettled := rapid.IntRange(0, 10).Draw(
				rt, "numSettled",
			)
			numFailed := rapid.IntRange(0, 10).Draw(
				rt, "numFailed",
			)

			var nextID uint64 = 1
			pendingIDs := make([]uint64, 0, numPending)
			allIDs := make([]uint64, 0)

			// Create pending records.
			for i := 0; i < numPending; i++ {
				id := nextID
				nextID++
				require.NoError(t, store.InitAttempt(id))
				pendingIDs = append(pendingIDs, id)
				allIDs = append(allIDs, id)
			}

			// Create settled records.
			for i := 0; i < numSettled; i++ {
				id := nextID
				nextID++
				require.NoError(t, store.InitAttempt(id))
				settleResult := &networkResult{
					msg: &lnwire.
						UpdateFulfillHTLC{},
					unencrypted: true,
				}
				require.NoError(t, store.StoreResult(
					id, settleResult,
				))
				allIDs = append(allIDs, id)
			}

			// Create failed records.
			for i := 0; i < numFailed; i++ {
				id := nextID
				nextID++
				require.NoError(t, store.InitAttempt(id))
				failResult := &networkResult{
					msg:         &lnwire.UpdateFailHTLC{},
					unencrypted: true,
				}
				require.NoError(t, store.StoreResult(
					id, failResult,
				))
				allIDs = append(allIDs, id)
			}

			// Add some non-existent IDs.
			numGhost := rapid.IntRange(0, 5).Draw(rt, "numGhost")
			for i := 0; i < numGhost; i++ {
				allIDs = append(allIDs, nextID+uint64(100+i))
			}

			// Delete all of them.
			results, err := store.DeleteAttempts(allIDs)
			require.NoError(t, err)

			// PROPERTY: Every pending record must report
			// DeletionPending and still be readable in the store.
			for _, id := range pendingIDs {
				require.Equal(
					t, DeletionPending, results[id],
					"pending attempt %d was not protected",
					id,
				)

				_, err := store.GetResult(id)
				require.ErrorIs(
					t, err, ErrAttemptResultPending,
					"pending attempt %d was deleted "+
						"from store", id,
				)
			}
		})
	})

	// Property 2: Result count always equals request count. Every requested
	// ID gets exactly one status in the response.
	t.Run("result count equals request count", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(rt *rapid.T) {
			db := channeldb.OpenForTesting(t, t.TempDir())
			store, err := newNetworkResultStore(db, false)
			require.NoError(t, err)

			numIDs := rapid.IntRange(0, 20).Draw(rt, "numIDs")
			ids := make([]uint64, numIDs)
			for i := range ids {
				ids[i] = uint64(i + 1)

				// Randomly decide: store a record or leave it
				// non-existent.
				action := rapid.IntRange(0, 2).Draw(
					rt, fmt.Sprintf("action_%d", i),
				)

				switch action {
				case 0:
					// Leave non-existent.
				case 1:
					// Pending.
					require.NoError(t, store.InitAttempt(
						ids[i],
					))
				case 2:
					// Terminal (settled).
					require.NoError(t, store.InitAttempt(
						ids[i],
					))
					result := &networkResult{
						msg: &lnwire.
							UpdateFulfillHTLC{},
						unencrypted: true,
					}
					require.NoError(t, store.StoreResult(
						ids[i], result,
					))
				}
			}

			results, err := store.DeleteAttempts(ids)
			require.NoError(t, err)

			// PROPERTY: Exactly one result per requested ID.
			require.Len(t, results, numIDs)
			for _, id := range ids {
				_, ok := results[id]
				require.True(
					t, ok,
					"missing result for ID %d", id,
				)
			}
		})
	})

	// Property 3: Deletion is idempotent. Deleting the same terminal IDs
	// twice never returns an error — the second call reports NotFound for
	// each already-deleted record.
	t.Run("deletion is idempotent", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(rt *rapid.T) {
			db := channeldb.OpenForTesting(t, t.TempDir())
			store, err := newNetworkResultStore(db, false)
			require.NoError(t, err)

			numTerminal := rapid.IntRange(1, 10).Draw(
				rt, "numTerminal",
			)
			ids := make([]uint64, numTerminal)
			for i := range ids {
				ids[i] = uint64(i + 1)
				require.NoError(t, store.InitAttempt(ids[i]))
				result := &networkResult{
					msg: &lnwire.
						UpdateFulfillHTLC{},
					unencrypted: true,
				}
				require.NoError(t, store.StoreResult(
					ids[i], result,
				))
			}

			// First delete — all should succeed.
			results1, err := store.DeleteAttempts(ids)
			require.NoError(t, err)
			for _, id := range ids {
				require.Equal(
					t, DeletionOK, results1[id],
					"first delete of %d should succeed",
					id,
				)
			}

			// Second delete — no error, all report gone.
			results2, err := store.DeleteAttempts(ids)
			require.NoError(t, err)

			// PROPERTY: Re-deleting never errors. Each ID reports
			// NotFound (the record is gone).
			for _, id := range ids {
				require.Equal(
					t, DeletionNotFound, results2[id],
					"re-delete of %d returned "+
						"unexpected status %v",
					id, results2[id],
				)
			}
		})
	})

	// Property 4: Terminal records are always removed. After a successful
	// delete, every terminal record must be gone from the store.
	t.Run("terminal records are removed", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(rt *rapid.T) {
			db := channeldb.OpenForTesting(t, t.TempDir())
			store, err := newNetworkResultStore(db, false)
			require.NoError(t, err)

			numTerminal := rapid.IntRange(1, 15).Draw(
				rt, "numTerminal",
			)
			ids := make([]uint64, numTerminal)
			for i := range ids {
				ids[i] = uint64(i + 1)
				require.NoError(t, store.InitAttempt(ids[i]))

				// Randomly choose settled or failed.
				var msg lnwire.Message
				if rapid.Bool().Draw(rt,
					fmt.Sprintf("settled_%d", i)) {

					msg = &lnwire.UpdateFulfillHTLC{}
				} else {
					msg = &lnwire.UpdateFailHTLC{}
				}

				result := &networkResult{
					msg:         msg,
					unencrypted: true,
				}
				require.NoError(t, store.StoreResult(
					ids[i], result,
				))
			}

			results, err := store.DeleteAttempts(ids)
			require.NoError(t, err)

			// PROPERTY: Every terminal record reports DeletionOK
			// and is gone from the store.
			for _, id := range ids {
				require.Equal(
					t, DeletionOK, results[id],
					"terminal attempt %d not deleted",
					id,
				)

				_, err := store.GetResult(id)
				require.ErrorIs(
					t, err, ErrPaymentIDNotFound,
					"terminal attempt %d still in "+
						"store after delete", id,
				)
			}
		})
	})

	// Property 5: Non-target IDs are unaffected. Deleting a subset of
	// terminal IDs must not disturb any other records in the store.
	t.Run("non-target IDs unaffected", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(rt *rapid.T) {
			db := channeldb.OpenForTesting(t, t.TempDir())
			store, err := newNetworkResultStore(db, false)
			require.NoError(t, err)

			numTarget := rapid.IntRange(1, 10).Draw(
				rt, "numTarget",
			)
			numBystander := rapid.IntRange(1, 10).Draw(
				rt, "numBystander",
			)

			var nextID uint64 = 1
			targetIDs := make([]uint64, numTarget)
			bystanderIDs := make([]uint64, numBystander)

			// Create terminal records to delete.
			for i := range targetIDs {
				targetIDs[i] = nextID
				nextID++
				require.NoError(t, store.InitAttempt(
					targetIDs[i],
				))
				result := &networkResult{
					msg: &lnwire.
						UpdateFulfillHTLC{},
					unencrypted: true,
				}
				require.NoError(t, store.StoreResult(
					targetIDs[i], result,
				))
			}

			// Create bystander records (mix of pending and
			// terminal) that should not be touched.
			for i := range bystanderIDs {
				bystanderIDs[i] = nextID
				nextID++
				require.NoError(t, store.InitAttempt(
					bystanderIDs[i],
				))

				// Randomly leave some as pending, settle
				// others.
				if rapid.Bool().Draw(rt,
					fmt.Sprintf("settle_%d", i)) {

					result := &networkResult{
						msg: &lnwire.
							UpdateFulfillHTLC{},
						unencrypted: true,
					}
					require.NoError(t, store.StoreResult(
						bystanderIDs[i], result,
					))
				}
			}

			// Delete only the target IDs.
			_, err = store.DeleteAttempts(targetIDs)
			require.NoError(t, err)

			// PROPERTY: Every bystander record is still in the
			// store, unmodified.
			for _, id := range bystanderIDs {
				_, err := store.GetResult(id)

				// GetResult returns nil error for terminal
				// results and ErrAttemptResultPending for
				// pending ones. Either is fine — the point
				// is it's not ErrPaymentIDNotFound.
				require.NotErrorIs(
					t, err, ErrPaymentIDNotFound,
					"bystander attempt %d was deleted",
					id,
				)
			}
		})
	})
}

// TestDisableRemoteRouter tests that the DisableRemoteRouter method behaves as
// expected.
func TestDisableRemoteRouter(t *testing.T) {
	t.Parallel()

	checkMarker := func(t *testing.T, db kvdb.Backend) bool {
		var marked bool
		err := db.View(func(tx kvdb.RTx) error {
			if tx.ReadBucket(externalLifecycleMarkerBucket) != nil {
				marked = true
			}

			return nil
		}, func() {
			marked = false
		})
		require.NoError(t, err)

		return marked
	}

	// Test case 1: In-flight payments exist.
	t.Run("in-flight payments", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, true)
		require.NoError(t, err)
		require.True(t, checkMarker(t, db))

		// Add a payment result to simulate an in-flight payment.
		err = store.storeResult(0, &networkResult{
			msg: &lnwire.UpdateAddHTLC{},
		})
		require.NoError(t, err)

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.ErrorIs(t, err, ErrAttemptEntriesExist)

		// The marker should still exist.
		require.True(t, checkMarker(t, db))
	})

	// Test case 2: No in-flight payments.
	t.Run("no in-flight payments", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, true)
		require.NoError(t, err)
		require.True(t, checkMarker(t, db))

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.NoError(t, err)

		// The marker should be gone.
		require.False(t, checkMarker(t, db))
	})

	// Test case 3: No in-flight payments and no marker.
	t.Run("no marker", func(t *testing.T) {
		t.Parallel()

		db := channeldb.OpenForTesting(t, t.TempDir())
		store, err := newNetworkResultStore(db, false)
		require.NoError(t, err)
		require.False(t, checkMarker(t, db))

		// Attempt to disable the remote router.
		err = store.DisableRemoteRouter()
		require.NoError(t, err)

		// The marker should still be gone.
		require.False(t, checkMarker(t, db))
	})
}

// TestMarkerCollision verifies that creating a network result store in local
// mode (externalLifecycle=false) fails when the database is already marked for
// external lifecycle management. This prevents the local router from
// accidentally cleaning attempt data that a remote controller depends on.
func TestMarkerCollision(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	// First, create a store in external mode to write the marker.
	_, err := newNetworkResultStore(db, true)
	require.NoError(t, err)

	// Now, try to create a store in local mode on the same DB. This
	// should fail because the DB is marked for external management.
	_, err = newNetworkResultStore(db, false)
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"marked for external lifecycle management",
	)
}
