package htlcswitch

import (
	"bytes"
	"encoding/binary"
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

	store := newNetworkResultStore(db)

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

// TestNetworkResultStoreCleanStore tests that CleanStore prunes the store down
// to exactly the requested attempt IDs. It opens the store via
// kvdb.GetTestBackend so that, under the postgres/sqlite build tags, the SQL
// drop-and-recreate path is exercised (TestNetworkResultStore above goes
// through channeldb.OpenForTesting, which always opens bbolt).
func TestNetworkResultStoreCleanStore(t *testing.T) {
	if kvdb.PostgresBackend {
		stop, err := kvdb.StartEmbeddedPostgres()
		require.NoError(t, err)

		t.Cleanup(func() {
			require.NoError(t, stop())
		})
	} else {
		t.Parallel()
	}

	tests := []struct {
		name       string
		numResults uint64
		keep       map[uint64]struct{}
	}{
		{
			name:       "partial keep",
			numResults: 5,
			keep:       map[uint64]struct{}{0: {}, 2: {}, 4: {}},
		},
		{
			name:       "empty keep",
			numResults: 5,
			keep:       map[uint64]struct{}{},
		},
		{
			name:       "keep all",
			numResults: 5,
			keep: map[uint64]struct{}{
				0: {}, 1: {}, 2: {}, 3: {}, 4: {},
			},
		},
		{
			name:       "empty store",
			numResults: 0,
			keep:       map[uint64]struct{}{0: {}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// GetTestBackend keys its fixture off the unique temp
			// dir, so parallel subtests get independent databases.
			backend, cleanup, err := kvdb.GetTestBackend(
				t.TempDir(), "db",
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, backend.Close())
				cleanup()
			})

			store := newNetworkResultStore(backend)

			// Store a result for every attempt ID.
			for i := range test.numResults {
				msg := &lnwire.UpdateAddHTLC{ID: i}
				n := &networkResult{
					msg:          msg,
					unencrypted:  true,
					isResolution: true,
				}
				require.NoError(t, store.StoreResult(i, n))
			}

			// Prune, keeping only the IDs in the keep set.
			require.NoError(t, store.CleanStore(test.keep))

			// Every kept ID must still be present; every other ID
			// must have been removed.
			for i := range test.numResults {
				result, err := store.GetResult(i)
				if _, ok := test.keep[i]; ok {
					require.NoErrorf(t, err,
						"kept id %d missing", i)
					msg := result.msg
					add, ok := msg.(*lnwire.UpdateAddHTLC)
					require.True(t, ok)
					require.Equal(t, i, add.ID)

					continue
				}

				require.ErrorIsf(t, err, ErrPaymentIDNotFound,
					"id %d not removed", i)
			}
		})
	}
}

// BenchmarkPostgresNetworkResultStoreCleanStore benchmarks pruning the network
// result store at different sizes on PostgreSQL, where individual bucket
// operations require database round trips.
func BenchmarkPostgresNetworkResultStoreCleanStore(b *testing.B) {
	if !kvdb.PostgresBackend {
		b.Skip("requires the kvdb_postgres build tag")
	}

	stop, err := kvdb.StartEmbeddedPostgres()
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, stop())
	})

	tests := []struct {
		name       string
		numResults uint64
		numKeep    uint64
	}{
		{
			name:       "1k_all_stale",
			numResults: 1_000,
		},
		{
			name:       "10k_all_stale",
			numResults: 10_000,
		},
		{
			name:       "10k_keep_10",
			numResults: 10_000,
			numKeep:    10,
		},
	}

	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			backend, cleanup, err := kvdb.GetTestBackend(
				b.TempDir(), "db",
			)
			require.NoError(b, err)
			b.Cleanup(func() {
				require.NoError(b, backend.Close())
				cleanup()
			})

			store := newNetworkResultStore(backend)
			keep := make(map[uint64]struct{}, test.numKeep)
			for i := range test.numKeep {
				keep[i] = struct{}{}
			}

			var result bytes.Buffer
			err = serializeNetworkResult(&result, &networkResult{
				msg:          &lnwire.UpdateAddHTLC{},
				unencrypted:  true,
				isResolution: true,
			})
			require.NoError(b, err)

			// Populate results in one transaction.
			// This setup is outside the measured benchmark.
			update := func(tx kvdb.RwTx) error {
				bucket, err := tx.CreateTopLevelBucket(
					networkResultStoreBucketKey,
				)
				if err != nil {
					return err
				}

				var key [8]byte
				for i := range test.numResults {
					binary.BigEndian.PutUint64(key[:], i)
					if err := bucket.Put(
						key[:], result.Bytes(),
					); err != nil {
						return err
					}
				}

				return nil
			}
			populate := func() error {
				return kvdb.Update(backend, update, func() {})
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				require.NoError(b, populate())
				b.StartTimer()

				require.NoError(b, store.CleanStore(keep))
			}
		})
	}
}

// TestNetworkResultStoreFailAndFetch tests the FailPendingAttempt and
// FetchPendingAttempts methods of the networkResultStore.
func TestNetworkResultStoreFailAndFetch(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	store := newNetworkResultStore(db)

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
