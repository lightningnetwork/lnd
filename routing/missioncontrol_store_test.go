package routing

import (
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const testMaxRecords = 2

// TestMissionControlStore tests the recording of payment failure events
// in mission control. It tests encoding and decoding of differing lnwire
// failures (FailIncorrectDetails and FailMppTimeout), pruning of results
// and idempotent writes.
func TestMissionControlStore(t *testing.T) {
	// Set time zone explicitly to keep test deterministic.
	time.Local = time.UTC

	file, err := os.CreateTemp("", "*.db")
	if err != nil {
		t.Fatal(err)
	}

	dbPath := file.Name()
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		require.NoError(t, os.Remove(dbPath))
	})

	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	store, err := newMissionControlStore(db, testMaxRecords, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	results, err := store.fetchAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatal("expected no results")
	}

	testRoute := route.Route{
		SourcePubKey: route.Vertex{1},
		Hops: []*route.Hop{
			{
				PubKeyBytes:   route.Vertex{2},
				LegacyPayload: true,
			},
		},
	}

	failureSourceIdx := 1

	result1 := paymentResult{
		route:            &testRoute,
		failure:          lnwire.NewFailIncorrectDetails(100, 1000),
		failureSourceIdx: &failureSourceIdx,
		id:               99,
		timeReply:        testTime,
		timeFwd:          testTime.Add(-time.Minute),
	}

	result2 := result1
	result2.timeReply = result1.timeReply.Add(time.Hour)
	result2.timeFwd = result1.timeReply.Add(time.Hour)
	result2.id = 2

	// Store result.
	store.AddResult(&result2)

	// Store again to test idempotency.
	store.AddResult(&result2)

	// Store second result which has an earlier timestamp.
	store.AddResult(&result1)
	require.NoError(t, store.storeResults())

	results, err = store.fetchAll()
	if err != nil {
		t.Fatal(err)
	}
	require.Equal(t, 2, len(results))

	if len(results) != 2 {
		t.Fatal("expected two results")
	}

	// Check that results are stored in chronological order.
	if !reflect.DeepEqual(&result1, results[0]) {
		t.Fatalf("the results differ: %v vs %v", spew.Sdump(&result1),
			spew.Sdump(results[0]))
	}
	if !reflect.DeepEqual(&result2, results[1]) {
		t.Fatalf("the results differ: %v vs %v", spew.Sdump(&result2),
			spew.Sdump(results[1]))
	}

	// Recreate store to test pruning.
	store, err = newMissionControlStore(db, testMaxRecords, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Add a newer result which failed due to mpp timeout.
	result3 := result1
	result3.timeReply = result1.timeReply.Add(2 * time.Hour)
	result3.timeFwd = result1.timeReply.Add(2 * time.Hour)
	result3.id = 3
	result3.failure = &lnwire.FailMPPTimeout{}

	store.AddResult(&result3)
	require.NoError(t, store.storeResults())

	// Check that results are pruned.
	results, err = store.fetchAll()
	if err != nil {
		t.Fatal(err)
	}
	require.Equal(t, 2, len(results))
	if len(results) != 2 {
		t.Fatal("expected two results")
	}

	if !reflect.DeepEqual(&result2, results[0]) {
		t.Fatalf("the results differ: %v vs %v", spew.Sdump(&result2),
			spew.Sdump(results[0]))
	}
	if !reflect.DeepEqual(&result3, results[1]) {
		t.Fatalf("the results differ: %v vs %v", spew.Sdump(&result3),
			spew.Sdump(results[1]))
	}
}

// TestMissionControlStoreFlushing asserts the periodic flushing of the store
// works correctly.
func TestMissionControlStoreFlushing(t *testing.T) {
	// Set time zone explicitly to keep test deterministic.
	time.Local = time.UTC

	file, err := os.CreateTemp("", "*.db")
	require.NoError(t, err)

	dbPath := file.Name()
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		require.NoError(t, os.Remove(dbPath))
	})

	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	const flushInterval = time.Second

	store, err := newMissionControlStore(db, testMaxRecords, flushInterval)
	require.NoError(t, err)

	testRoute := route.Route{
		SourcePubKey: route.Vertex{1},
		Hops: []*route.Hop{
			{
				PubKeyBytes:   route.Vertex{2},
				LegacyPayload: true,
			},
		},
	}

	failureSourceIdx := 1
	failureDetails := lnwire.NewFailIncorrectDetails(100, 1000)
	var lastID uint64 = 0
	nextResult := func() *paymentResult {
		lastID += 1
		return &paymentResult{
			route:            &testRoute,
			failure:          failureDetails,
			failureSourceIdx: &failureSourceIdx,
			id:               lastID,
			timeReply:        testTime,
			timeFwd:          testTime.Add(-time.Minute),
		}
	}

	// Helper to assert the number of results is correct.
	assertResults := func(wantCount int) {
		t.Helper()
		err := wait.NoError(func() error {
			results, err := store.fetchAll()
			if err != nil {
				return err
			}
			if wantCount != len(results) {
				return fmt.Errorf("wrong nb of results: got "+
					"%d, want %d", len(results), wantCount)
			}
			if len(results) == 0 {
				return nil
			}
			gotLastID := results[len(results)-1].id
			if len(results) > 0 && gotLastID != lastID {
				return fmt.Errorf("wrong id for last item: "+
					"got %d, want %d", gotLastID, lastID)
			}

			return nil
		}, flushInterval*5)
		require.NoError(t, err)
	}

	// Run the store.
	store.run()
	time.Sleep(flushInterval)

	// Wait for the flush interval. There should be no records.
	assertResults(0)

	// Store a result and check immediately. There still shouldn't be
	// any results stored (flush interval has not elapsed).
	store.AddResult(nextResult())
	assertResults(0)

	// Assert that eventually the result is stored after being flushed.
	assertResults(1)

	// Store enough results that fill the max nb of results.
	for i := 0; i < testMaxRecords+1; i++ {
		store.AddResult(nextResult())
	}
	assertResults(testMaxRecords)

	// Finally, stop the store to recreate it.
	store.stop()

	// Recreate store.
	store, err = newMissionControlStore(db, testMaxRecords, flushInterval)
	require.NoError(t, err)
	store.run()
	defer store.stop()
	time.Sleep(flushInterval)
	assertResults(testMaxRecords)

	// Fill the store with results again.
	for i := 0; i < testMaxRecords+1; i++ {
		store.AddResult(nextResult())
	}
	assertResults(testMaxRecords)
}

// drainMCStoreQueueChan drains the store's queueChan when the test does not
// issue a run().
func drainMCStoreQueueChan(t testing.TB, store *missionControlStore) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
			case <-store.queueChan:
			}
		}
	}()
}

// BenchmarkMissionControlStoreFlushingNoWork benchmarks the periodic storage
// of data from the mission control store when no additional results are added
// between runs.
func BenchmarkMissionControlStoreFlushing(b *testing.B) {
	testRoute := route.Route{
		SourcePubKey: route.Vertex{1},
		Hops: []*route.Hop{
			{
				PubKeyBytes:   route.Vertex{2},
				LegacyPayload: true,
			},
		},
	}

	failureSourceIdx := 1
	failureDetails := lnwire.NewFailIncorrectDetails(100, 1000)
	testTimeFwd := testTime.Add(-time.Minute)

	const testMaxRecords = 1000

	tests := []struct {
		name      string
		nbResults int
		doBench   func(b *testing.B, store *missionControlStore)
	}{{
		name:      "no additional results",
		nbResults: 0,
	}, {
		name:      "one additional result",
		nbResults: 1,
	}, {
		name:      "ten additional results",
		nbResults: 10,
	}, {
		name:      "100 additional results",
		nbResults: 100,
	}, {
		name:      "250 additional results",
		nbResults: 250,
	}, {
		name:      "500 additional results",
		nbResults: 500,
	}}

	for _, tc := range tests {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			// Set time zone explicitly to keep test deterministic.
			time.Local = time.UTC

			file, err := os.CreateTemp("", "*.db")
			require.NoError(b, err)

			dbPath := file.Name()
			b.Cleanup(func() {
				require.NoError(b, file.Close())
				require.NoError(b, os.Remove(dbPath))
			})

			db, err := kvdb.Create(
				kvdb.BoltBackendName, dbPath, true,
				kvdb.DefaultDBTimeout,
			)
			require.NoError(b, err)
			b.Cleanup(func() {
				require.NoError(b, db.Close())
			})

			store, err := newMissionControlStore(
				db, testMaxRecords, time.Second,
			)
			require.NoError(b, err)

			// Fill the store.
			var lastID uint64
			for i := 0; i < testMaxRecords; i++ {
				lastID++
				result := &paymentResult{
					route:            &testRoute,
					failure:          failureDetails,
					failureSourceIdx: &failureSourceIdx,
					id:               lastID,
					timeReply:        testTime,
					timeFwd:          testTimeFwd,
				}
				store.AddResult(result)
			}

			// Do the first flush.
			err = store.storeResults()
			require.NoError(b, err)
			<-store.queueChan

			// Create the additional results.
			results := make([]*paymentResult, tc.nbResults)
			for i := 0; i < len(results); i++ {
				results[i] = &paymentResult{
					route:            &testRoute,
					failure:          failureDetails,
					failureSourceIdx: &failureSourceIdx,
					timeReply:        testTime,
					timeFwd:          testTimeFwd,
				}
			}

			// Run the actual benchmark.
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				for j := 0; j < len(results); j++ {
					lastID++
					results[j].id = lastID
					store.AddResult(results[j])
				}
				if len(results) > 0 {
					<-store.queueChan
				}
				err := store.storeResults()
				require.NoError(b, err)
			}
		})
	}
}
