package routing

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

const testMaxRecords = 2

// mcStoreTestRoute is a test route for the mission control store tests.
var mcStoreTestRoute = extractMCRoute(&route.Route{
	TotalAmount:  lnwire.MilliSatoshi(5),
	SourcePubKey: route.Vertex{1},
	Hops: []*route.Hop{
		{
			PubKeyBytes:   route.Vertex{2},
			ChannelID:     4,
			AmtToForward:  lnwire.MilliSatoshi(7),
			CustomRecords: make(map[uint64][]byte),
		},
	},
})

// mcStoreTestHarness is the harness for a MissonControlStore test.
type mcStoreTestHarness struct {
	db    walletdb.DB
	store *missionControlStore
}

// newMCStoreTestHarness initializes a test mission control store.
func newMCStoreTestHarness(t testing.TB, maxRecords int,
	flushInterval time.Duration) mcStoreTestHarness {
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
		false,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	store, err := newMissionControlStore(
		newDefaultNamespacedStore(db), maxRecords, flushInterval,
	)
	require.NoError(t, err)

	return mcStoreTestHarness{db: db, store: store}
}

// TestMissionControlStore tests the recording of payment failure events
// in mission control. It tests encoding and decoding of differing lnwire
// failures (FailIncorrectDetails and FailMppTimeout), pruning of results
// and idempotent writes.
func TestMissionControlStore(t *testing.T) {
	h := newMCStoreTestHarness(t, testMaxRecords, time.Second)
	db, store := h.db, h.store

	results, err := store.fetchAll()
	require.NoError(t, err)
	require.Len(t, results, 0)

	failureSourceIdx := 1

	result1 := newPaymentResult(
		99, mcStoreTestRoute, testTime, testTime,
		newPaymentFailure(
			&failureSourceIdx,
			lnwire.NewFailIncorrectDetails(100, 1000),
		),
	)

	result2 := newPaymentResult(
		2, mcStoreTestRoute, testTime.Add(time.Hour),
		testTime.Add(time.Hour),
		newPaymentFailure(
			&failureSourceIdx,
			lnwire.NewFailIncorrectDetails(100, 1000),
		),
	)

	// Store result.
	store.AddResult(result2)

	// Store again to test idempotency.
	store.AddResult(result2)

	// Store second result which has an earlier timestamp.
	store.AddResult(result1)
	require.NoError(t, store.storeResults())

	results, err = store.fetchAll()
	require.NoError(t, err)
	require.Len(t, results, 2)

	// Check that results are stored in chronological order.
	require.Equal(t, result1, results[0])
	require.Equal(t, result2, results[1])

	// Recreate store to test pruning.
	store, err = newMissionControlStore(
		newDefaultNamespacedStore(db), testMaxRecords, time.Second,
	)
	require.NoError(t, err)

	// Add a newer result which failed due to mpp timeout.
	result3 := result1
	result3.timeReply = tlv.NewPrimitiveRecord[tlv.TlvType1](
		uint64(testTime.Add(2 * time.Hour).UnixNano()),
	)
	result3.timeFwd = tlv.NewPrimitiveRecord[tlv.TlvType0](
		uint64(testTime.Add(2 * time.Hour).UnixNano()),
	)
	result3.id = 3
	result3.failure = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType3](*newPaymentFailure(
			&failureSourceIdx, &lnwire.FailMPPTimeout{},
		)),
	)

	store.AddResult(result3)
	require.NoError(t, store.storeResults())

	// Check that results are pruned.
	results, err = store.fetchAll()
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.Equal(t, result2, results[0])
	require.Equal(t, result3, results[1])

	// Also demonstrate the persistence of a success result.
	result4 := newPaymentResult(
		5, mcStoreTestRoute, testTime.Add(3*time.Hour),
		testTime.Add(3*time.Hour), nil,
	)
	store.AddResult(result4)
	require.NoError(t, store.storeResults())

	// We should still only have 2 results.
	results, err = store.fetchAll()
	require.NoError(t, err)
	require.Len(t, results, 2)

	// The two latest results should have been returned.
	require.Equal(t, result3, results[0])
	require.Equal(t, result4, results[1])
}

// TestMissionControlStoreFlushing asserts the periodic flushing of the store
// works correctly.
func TestMissionControlStoreFlushing(t *testing.T) {
	const flushInterval = 500 * time.Millisecond

	h := newMCStoreTestHarness(t, testMaxRecords, flushInterval)
	db, store := h.db, h.store

	var (
		failureSourceIdx = 1
		failureDetails   = lnwire.NewFailIncorrectDetails(100, 1000)
		lastID           uint64
	)
	nextResult := func() *paymentResult {
		lastID += 1
		return newPaymentResult(
			lastID, mcStoreTestRoute, testTime.Add(-time.Hour),
			testTime,
			newPaymentFailure(&failureSourceIdx, failureDetails),
		)
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

	// Store enough results that fill the max number of results.
	for i := 0; i < testMaxRecords; i++ {
		store.AddResult(nextResult())
	}
	assertResults(testMaxRecords)

	// Finally, stop the store to recreate it.
	store.stop()

	// Recreate store.
	store, err := newMissionControlStore(
		newDefaultNamespacedStore(db), testMaxRecords, flushInterval,
	)
	require.NoError(t, err)
	store.run()
	defer store.stop()
	time.Sleep(flushInterval)
	assertResults(testMaxRecords)

	// Fill the store with results again.
	for i := 0; i < testMaxRecords; i++ {
		store.AddResult(nextResult())
	}
	assertResults(testMaxRecords)
}

// BenchmarkMissionControlStoreFlushing benchmarks the periodic storage of data
// from the mission control store when additional results are added between
// runs.
func BenchmarkMissionControlStoreFlushing(b *testing.B) {
	var (
		failureSourceIdx = 1
		failureDetails   = lnwire.NewFailIncorrectDetails(100, 1000)
		testTimeFwd      = testTime.Add(-time.Minute)

		tests = []int{0, 1, 10, 100, 250, 500}
	)

	const testMaxRecords = 1000

	for _, tc := range tests {
		tc := tc
		name := fmt.Sprintf("%v additional results", tc)
		b.Run(name, func(b *testing.B) {
			h := newMCStoreTestHarness(
				b, testMaxRecords, time.Second,
			)
			store := h.store

			// Fill the store.
			var lastID uint64
			for i := 0; i < testMaxRecords; i++ {
				lastID++
				result := newPaymentResult(
					lastID, mcStoreTestRoute, testTimeFwd,
					testTime,
					newPaymentFailure(
						&failureSourceIdx,
						failureDetails,
					),
				)
				store.AddResult(result)
			}

			// Do the first flush.
			err := store.storeResults()
			require.NoError(b, err)

			// Create the additional results.
			results := make([]*paymentResult, tc)
			for i := 0; i < len(results); i++ {
				results[i] = newPaymentResult(
					0, mcStoreTestRoute, testTimeFwd,
					testTime,
					newPaymentFailure(
						&failureSourceIdx,
						failureDetails,
					),
				)
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
				err := store.storeResults()
				require.NoError(b, err)
			}
		})
	}
}
