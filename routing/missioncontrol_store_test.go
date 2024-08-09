package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const testMaxRecords = 2

var (
	// mcStoreTestRoute is a test route for the mission control store tests.
	mcStoreTestRoute = route.Route{
		SourcePubKey: route.Vertex{1},
		Hops: []*route.Hop{
			{
				PubKeyBytes:   route.Vertex{2},
				LegacyPayload: true,
			},
		},
	}
)

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
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	store, err := newMissionControlStore(db, maxRecords, flushInterval)
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

	result1 := paymentResult{
		route:            &mcStoreTestRoute,
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
	require.NoError(t, err)
	require.Len(t, results, 2)

	// Check that results are stored in chronological order.
	require.Equal(t, &result1, results[0])
	require.Equal(t, &result2, results[1])

	// Recreate store to test pruning.
	store, err = newMissionControlStore(db, testMaxRecords, time.Second)
	require.NoError(t, err)

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
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.Equal(t, &result2, results[0])
	require.Equal(t, &result3, results[1])
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
		return &paymentResult{
			route:            &mcStoreTestRoute,
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

	// Store enough results that fill the max number of results.
	for i := 0; i < testMaxRecords; i++ {
		store.AddResult(nextResult())
	}
	assertResults(testMaxRecords)

	// Finally, stop the store to recreate it.
	store.stop()

	// Recreate store.
	store, err := newMissionControlStore(db, testMaxRecords, flushInterval)
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
				result := &paymentResult{
					route:            &mcStoreTestRoute,
					failure:          failureDetails,
					failureSourceIdx: &failureSourceIdx,
					id:               lastID,
					timeReply:        testTime,
					timeFwd:          testTimeFwd,
				}
				store.AddResult(result)
			}

			// Do the first flush.
			err := store.storeResults()
			require.NoError(b, err)

			// Create the additional results.
			results := make([]*paymentResult, tc)
			for i := 0; i < len(results); i++ {
				results[i] = &paymentResult{
					route:            &mcStoreTestRoute,
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
				err := store.storeResults()
				require.NoError(b, err)
			}
		})
	}
}

// timeToUnix converts a time.Time value to its Unix time representation.
func timeToUnix(t time.Time) int64 {
	return t.Unix()
}

// timedPairResultsAreEqual compares two TimedPairResult structs for equality.
// It returns true if the FailTime, FailAmt, SuccessTime, and SuccessAmt fields
// of both TimedPairResult structs are equal. The comparison of FailTime and
// SuccessTime is done by converting them to Unix time to avoid issues with
// internal representation differences.
//
// Parameters:
//   - tpr1: The first TimedPairResult to compare.
//   - tpr2: The second TimedPairResult to compare.
//
// Returns:
//   - bool: true if both TimedPairResult structs are equal, false otherwise.
func timedPairResultsAreEqual(tpr1, tpr2 TimedPairResult) bool {
	return timeToUnix(tpr1.FailTime) == timeToUnix(tpr2.FailTime) &&
		tpr1.FailAmt == tpr2.FailAmt &&
		timeToUnix(tpr1.SuccessTime) == timeToUnix(tpr2.SuccessTime) &&
		tpr1.SuccessAmt == tpr2.SuccessAmt
}

// TestPersistMCData verifies that the persistMCData function correctly
// stores a given MissionControlSnapshot into the database and retrieves it
// accurately using fetchMCData.
//
// It performs the following steps:
// 1. Creates a test harness for the mission control store.
// 2. Prepares a sample MissionControlSnapshot with timed pair results.
// 3. Persists the snapshot using persistMCData.
// 4. Fetches the stored data using fetchMCData.
// 5. Verifies that the fetched data matches the original snapshot.
func TestPersistMCData(t *testing.T) {
	h := newMCStoreTestHarness(t, testMaxRecords, time.Second)
	store := h.store

	// Prepare a sample mission control snapshot.
	snapshot := &MissionControlSnapshot{
		Pairs: []MissionControlPairSnapshot{
			{
				Pair: DirectedNodePair{
					From: route.Vertex{1},
					To:   route.Vertex{2},
				},
				TimedPairResult: TimedPairResult{
					SuccessTime: time.Now().Add(-time.Hour),
					SuccessAmt:  lnwire.MilliSatoshi(1500),
				},
			},
			{
				Pair: DirectedNodePair{
					From: route.Vertex{3},
					To:   route.Vertex{4},
				},
				TimedPairResult: TimedPairResult{
					FailTime: time.Now().Add(-time.Hour),
					FailAmt:  lnwire.MilliSatoshi(3000),
				},
			},
		},
	}

	// Persist the mission control snapshot.
	err := store.persistMCData(snapshot)
	require.NoError(t, err)

	// Fetch the data to verify.
	mcSnapshots, err := store.fetchMCData()
	require.NoError(t, err)
	require.Len(t, mcSnapshots, 1)
	require.Len(t, mcSnapshots[0].Pairs, 2)
	require.Equal(t, snapshot.Pairs[0].Pair, mcSnapshots[0].Pairs[0].Pair)
	require.Equal(t, snapshot.Pairs[1].Pair, mcSnapshots[0].Pairs[1].Pair)
	require.True(
		t, timedPairResultsAreEqual(
			snapshot.Pairs[0].TimedPairResult,
			mcSnapshots[0].Pairs[0].TimedPairResult,
		),
	)
	require.True(
		t, timedPairResultsAreEqual(
			snapshot.Pairs[1].TimedPairResult,
			mcSnapshots[0].Pairs[1].TimedPairResult,
		),
	)
}

// TestResetMCData verifies that the resetMCData function correctly
// clears all mission control data from the database.
//
// It performs the following steps:
// 1. Creates a test harness for the mission control store.
// 2. Prepares and persists a sample MissionControlSnapshot.
// 3. Calls resetMCData to clear the mission control data.
// 4. Fetches the data using fetchMCData to verify that it has been reset.
func TestResetMCData(t *testing.T) {
	h := newMCStoreTestHarness(t, testMaxRecords, time.Second)
	store := h.store

	// Prepare a sample mission control snapshot.
	snapshot := &MissionControlSnapshot{
		Pairs: []MissionControlPairSnapshot{
			{
				Pair: DirectedNodePair{
					From: route.Vertex{1},
					To:   route.Vertex{2},
				},
				TimedPairResult: TimedPairResult{
					SuccessTime: time.Now().Add(-time.Hour),
					SuccessAmt:  lnwire.MilliSatoshi(2000),
				},
			},
		},
	}

	// Persist the mission control snapshot.
	err := store.persistMCData(snapshot)
	require.NoError(t, err)

	// Reset the mission control data.
	err = store.resetMCData()
	require.NoError(t, err)

	// Fetch the data to verify it has been reset.
	mcSnapshots, err := store.fetchMCData()
	require.NoError(t, err)
	require.Len(t, mcSnapshots, 0)
}

// TestDeserializeMCData verifies that the deserializeMCData function correctly
// deserializes key and value bytes into a MissionControlPairSnapshot.
//
// It performs the following steps:
//  1. Prepares sample data for serialization, including 'From' and 'To' public
//     keys and a TimedPairResult.
//  2. Serializes the TimedPairResult into JSON format.
//  3. Concatenates the 'From' and 'To' public keys to form the key.
//  4. Deserializes the mission control data using deserializeMCData.
//  5. Verifies that the deserialized data matches the original data.
func TestDeserializeMCData(t *testing.T) {
	// Prepare sample data for serialization.
	from := route.Vertex{1}
	to := route.Vertex{2}
	timedPairResult := TimedPairResult{
		FailTime:    time.Now(),
		FailAmt:     lnwire.MilliSatoshi(1000),
		SuccessTime: time.Now().Add(-time.Hour),
		SuccessAmt:  lnwire.MilliSatoshi(2000),
	}
	data, err := json.Marshal(timedPairResult)
	require.NoError(t, err)

	// Concatenate the 'From' and 'To' public keys to form the key.
	// Create the key by concatenating From and To bytes.
	key := append([]byte{}, from[:]...)
	key = append(key, to[:]...)

	// Deserialize the mission control data.
	mcSnapshot, err := deserializeMCData(key, data)
	require.NoError(t, err)
	require.Equal(t, from, mcSnapshot.Pair.From)
	require.Equal(t, to, mcSnapshot.Pair.To)
	require.True(
		t, timedPairResultsAreEqual(
			timedPairResult, mcSnapshot.TimedPairResult,
		),
	)
}
