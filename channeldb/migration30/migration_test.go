package migration30

import (
	"bytes"
	"fmt"
	"testing"

	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type (
	beforeMigrationFunc func(db kvdb.Backend) error
	afterMigrationFunc  func(t *testing.T, db kvdb.Backend)
)

// TestMigrateRevocationLog provide a comprehensive test for the revocation log
// migration. The revocation logs are stored inside a deeply nested bucket, and
// can be accessed via nodePub:chainHash:fundingOutpoint:revocationLogBucket.
// Based on each value in the chain, we'd end up in a different db state. This
// test alters nodePub, fundingOutpoint, and revocationLogBucket to test
// against possible db states, leaving the chainHash staying the same as it's
// less likely to be changed. In specific, we test based on whether we have one
// or two peers(nodePub). For each peer, we test whether we have one or two
// channels(fundingOutpoint). And for each channel, we test 5 cases based on
// the revocation migration states(see buildChannelCases). The total states
// grow quickly and the test may take longer than 5min.
func TestMigrateRevocationLog(t *testing.T) {
	t.Parallel()

	testCases := make([]*testCase, 0)

	// Create two peers, each has two channels.
	alice1, alice2 := createTwoChannels()
	bob1, bob2 := createTwoChannels()

	// Sort the two peers to match the order saved in boltdb.
	if bytes.Compare(
		alice1.IdentityPub.SerializeCompressed(),
		bob1.IdentityPub.SerializeCompressed(),
	) > 0 {

		alice1, bob1 = bob1, alice1
		alice2, bob2 = bob2, alice2
	}

	// Build test cases for two peers. Each peer is independent so we
	// combine the test cases based on its current db state. This would
	// create a total of 30x30=900 cases.
	for _, p1 := range buildPeerCases(alice1, alice2, false) {
		for _, p2 := range buildPeerCases(bob1, bob2, p1.unfinished) {
			setups := make([]beforeMigrationFunc, 0)
			setups = append(setups, p1.setups...)
			setups = append(setups, p2.setups...)

			asserters := make([]afterMigrationFunc, 0)
			asserters = append(asserters, p1.asserters...)
			asserters = append(asserters, p2.asserters...)

			name := fmt.Sprintf("alice: %s, bob: %s",
				p1.name, p2.name)

			tc := &testCase{
				name:      name,
				setups:    setups,
				asserters: asserters,
			}
			testCases = append(testCases, tc)
		}
	}

	fmt.Printf("Running %d test cases...\n", len(testCases))
	fmt.Printf("withAmtData is set to: %v\n", withAmtData)

	for i, tc := range testCases {
		tc := tc

		// Construct a test case name that can be easily traced.
		name := fmt.Sprintf("case_%d", i)
		fmt.Println(name, tc.name)

		success := t.Run(name, func(t *testing.T) {
			// Log the test's actual name on failure.
			t.Log("Test setup: ", tc.name)

			beforeMigration := func(db kvdb.Backend) error {
				for _, setup := range tc.setups {
					if err := setup(db); err != nil {
						return err
					}
				}
				return nil
			}

			afterMigration := func(db kvdb.Backend) error {
				for _, asserter := range tc.asserters {
					asserter(t, db)
				}
				return nil
			}

			cfg := &MigrateRevLogConfigImpl{
				NoAmountData: !withAmtData,
			}

			migtest.ApplyMigrationWithDB(
				t,
				beforeMigration,
				afterMigration,
				func(db kvdb.Backend) error {
					return MigrateRevocationLog(db, cfg)
				},
				false,
			)
		})
		if !success {
			return
		}
	}
}

// TestValidateMigration checks that the function `validateMigration` behaves
// as expected.
func TestValidateMigration(t *testing.T) {
	c := createTestChannel(nil)

	testCases := []struct {
		name       string
		setup      func(db kvdb.Backend) error
		expectFail bool
	}{
		{
			// Finished prior to v0.15.0.
			name: "valid migration",
			setup: func(db kvdb.Backend) error {
				return createFinished(db, c, true)
			},
			expectFail: false,
		},
		{
			// Finished after to v0.15.0.
			name: "valid migration after v0.15.0",
			setup: func(db kvdb.Backend) error {
				return createFinished(db, c, false)
			},
			expectFail: false,
		},
		{
			// Missing logs prior to v0.15.0.
			name: "invalid migration",
			setup: func(db kvdb.Backend) error {
				return createNotFinished(db, c, true)
			},
			expectFail: true,
		},
		{
			// Missing logs after to v0.15.0.
			name: "invalid migration after v0.15.0",
			setup: func(db kvdb.Backend) error {
				return createNotFinished(db, c, false)
			},
			expectFail: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		// Create a test db.
		cdb, err := migtest.MakeDB(t)
		require.NoError(t, err, "failed to create test db")

		t.Run(tc.name, func(t *testing.T) {
			// Setup test logs.
			err := tc.setup(cdb)
			require.NoError(t, err, "failed to setup")

			// Call the actual function and check the error is
			// returned as expected.
			err = kvdb.Update(cdb, validateMigration, func() {})

			if tc.expectFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// createTwoChannels creates two channels that have the same chainHash and
// IdentityPub, simulating having two channels under the same peer.
func createTwoChannels() (*mig26.OpenChannel, *mig26.OpenChannel) {
	// Create two channels under the same peer.
	c1 := createTestChannel(nil)
	c2 := createTestChannel(c1.IdentityPub)

	// If c1 is greater than c2, boltdb will put c2 before c1.
	if bytes.Compare(
		c1.FundingOutpoint.Hash[:],
		c2.FundingOutpoint.Hash[:],
	) > 0 {

		c1, c2 = c2, c1
	}

	return c1, c2
}

// channelTestCase defines a single test case given a particular channel state.
type channelTestCase struct {
	name       string
	setup      beforeMigrationFunc
	asserter   afterMigrationFunc
	unfinished bool
}

// buildChannelCases builds five channel test cases. These cases can be viewed
// as basic units that are used to build more complex test cases based on
// number of channels and peers.
func buildChannelCases(c *mig26.OpenChannel,
	overwrite bool) []*channelTestCase {

	// assertNewLogs is a helper closure that checks the old bucket and the
	// two new logs are saved.
	assertNewLogs := func(t *testing.T, db kvdb.Backend) {
		// Check that the old bucket is removed.
		assertOldLogBucketDeleted(t, db, c)

		l := fetchNewLog(t, db, c, logHeight1)
		assertRevocationLog(t, newLog1, l)

		l = fetchNewLog(t, db, c, logHeight2)
		assertRevocationLog(t, newLog2, l)
	}

	// case1 defines a case where we don't have a chanBucket.
	case1 := &channelTestCase{
		name: "no channel",
		setup: func(db kvdb.Backend) error {
			return setupTestLogs(db, nil, nil, nil)
		},
		// No need to assert anything.
		asserter: func(t *testing.T, db kvdb.Backend) {},
	}

	// case2 defines a case when the chanBucket has no old revocation logs.
	case2 := &channelTestCase{
		name: "empty old logs",
		setup: func(db kvdb.Backend) error {
			return setupTestLogs(db, c, nil, nil)
		},
		// No need to assert anything.
		asserter: func(t *testing.T, db kvdb.Backend) {},
	}

	// case3 defines a case when the chanBucket has finished its migration.
	case3 := &channelTestCase{
		name: "finished migration",
		setup: func(db kvdb.Backend) error {
			return createFinished(db, c, true)
		},
		asserter: func(t *testing.T, db kvdb.Backend) {
			// Check that the old bucket is removed.
			assertOldLogBucketDeleted(t, db, c)

			// Fetch the new log. We should see
			// OurOutputIndex matching the testOurIndex
			// value, indicating that for migrated logs we
			// won't touch them.
			//
			// NOTE: when the log is created before
			// migration, OurOutputIndex would be
			// testOurIndex rather than OutputIndexEmpty.
			l := fetchNewLog(t, db, c, logHeight1)
			require.EqualValues(
				t, testOurIndex, l.OurOutputIndex,
				"expected log to be NOT overwritten",
			)

			// Fetch the new log. We should see
			// TheirOutputIndex matching the testTheirIndex
			// value, indicating that for migrated logs we
			// won't touch them.
			//
			// NOTE: when the log is created before
			// migration, TheirOutputIndex would be
			// testTheirIndex rather than OutputIndexEmpty.
			l = fetchNewLog(t, db, c, logHeight2)
			require.EqualValues(
				t, testTheirIndex, l.TheirOutputIndex,
				"expected log to be NOT overwritten",
			)
		},
	}

	// case4 defines a case when the chanBucket has both old and new logs,
	// which happens when the migration is ongoing.
	case4 := &channelTestCase{
		name: "unfinished migration",
		setup: func(db kvdb.Backend) error {
			return createNotFinished(db, c, true)
		},
		asserter: func(t *testing.T, db kvdb.Backend) {
			// Check that the old bucket is removed.
			assertOldLogBucketDeleted(t, db, c)

			// Fetch the new log. We should see
			// OurOutputIndex matching the testOurIndex
			// value, indicating that for migrated logs we
			// won't touch them.
			//
			// NOTE: when the log is created before
			// migration, OurOutputIndex would be
			// testOurIndex rather than OutputIndexEmpty.
			l := fetchNewLog(t, db, c, logHeight1)
			require.EqualValues(
				t, testOurIndex, l.OurOutputIndex,
				"expected log to be NOT overwritten",
			)

			// We expect to have one new log.
			l = fetchNewLog(t, db, c, logHeight2)
			assertRevocationLog(t, newLog2, l)
		},
		unfinished: true,
	}

	// case5 defines a case when the chanBucket has no new logs, which
	// happens when we haven't migrated anything for this bucket yet.
	case5 := &channelTestCase{
		name: "initial migration",
		setup: func(db kvdb.Backend) error {
			return createNotStarted(db, c, true)
		},
		asserter:   assertNewLogs,
		unfinished: true,
	}

	// Check that the already migrated logs are overwritten. For two
	// channels sorted and stored in boltdb, when the first channel has
	// unfinished migrations, even channel two has migrated logs, they will
	// be overwritten to make sure the data stay consistent.
	if overwrite {
		case3.name += " overwritten"
		case3.asserter = assertNewLogs

		case4.name += " overwritten"
		case4.asserter = assertNewLogs
	}

	return []*channelTestCase{case1, case2, case3, case4, case5}
}

// testCase defines a case for a particular db state that we want to test based
// on whether we have one or two peers, one or two channels for each peer, and
// the particular state for each channel.
type testCase struct {
	// name has the format: peer: [channel state].
	name string

	// setups is a list of setup functions we'd run sequentially to provide
	// the initial db state.
	setups []beforeMigrationFunc

	// asserters is a list of assertions we'd perform after the migration
	// function has been called.
	asserters []afterMigrationFunc

	// unfinished specifies that the test case is testing a case where the
	// revocation migration is considered unfinished. This is useful if
	// it's used to construct a larger test case where there's a following
	// case with a state of finished, we can then test that the revocation
	// logs are overwritten even if the state says finished.
	unfinished bool
}

// buildPeerCases builds test cases based on whether we have one or two
// channels saved under this peer. When there's one channel, we have 5 states,
// and when there are two, we have 25 states, a total of 30 cases.
func buildPeerCases(c1, c2 *mig26.OpenChannel, unfinished bool) []*testCase {
	testCases := make([]*testCase, 0)

	// Single peer with one channel.
	for _, c := range buildChannelCases(c1, unfinished) {
		name := fmt.Sprintf("[channel: %s]", c.name)
		tc := &testCase{
			name:       name,
			setups:     []beforeMigrationFunc{c.setup},
			asserters:  []afterMigrationFunc{c.asserter},
			unfinished: c.unfinished,
		}
		testCases = append(testCases, tc)
	}

	// Single peer with two channels.
	testCases = append(
		testCases, buildTwoChannelCases(c1, c2, unfinished)...,
	)

	return testCases
}

// buildTwoChannelCases takes two channels to build test cases that covers all
// combinations of the two channels' state. Since each channel has 5 states,
// this will give us a total 25 states.
func buildTwoChannelCases(c1, c2 *mig26.OpenChannel,
	unfinished bool) []*testCase {

	testCases := make([]*testCase, 0)

	// buildCase is a helper closure that contructs a test case based on
	// the two smaller test cases.
	buildCase := func(tc1, tc2 *channelTestCase) {
		setups := make([]beforeMigrationFunc, 0)
		setups = append(setups, tc1.setup)
		setups = append(setups, tc2.setup)

		asserters := make([]afterMigrationFunc, 0)
		asserters = append(asserters, tc1.asserter)
		asserters = append(asserters, tc2.asserter)

		// If any of the test cases has unfinished state, the test case
		// would have a state of unfinished, indicating any peers after
		// this one must overwrite their revocation logs.
		unfinished := tc1.unfinished || tc2.unfinished

		name := fmt.Sprintf("[channelOne: %s] [channelTwo: %s]",
			tc1.name, tc2.name)

		tc := &testCase{
			name:       name,
			setups:     setups,
			asserters:  asserters,
			unfinished: unfinished,
		}
		testCases = append(testCases, tc)
	}

	// Build channel cases for both of the channels and combine them.
	for _, tc1 := range buildChannelCases(c1, unfinished) {
		// The second channel's already migrated logs will be
		// overwritten if the first channel has unfinished state, which
		// are case4 and case5.
		unfinished := unfinished || tc1.unfinished
		for _, tc2 := range buildChannelCases(c2, unfinished) {
			buildCase(tc1, tc2)
		}
	}

	return testCases
}

// assertOldLogBucketDeleted asserts that the given channel's old revocation
// log bucket doesn't exist.
func assertOldLogBucketDeleted(t testing.TB, cdb kvdb.Backend,
	c *mig26.OpenChannel) {

	var logBucket kvdb.RBucket
	err := kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		chanBucket, err := mig25.FetchChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}

		logBucket = chanBucket.NestedReadBucket(
			revocationLogBucketDeprecated,
		)
		return err
	}, func() {})

	require.NoError(t, err, "read bucket failed")
	require.Nil(t, logBucket, "expected old bucket to be deleted")
}

// fetchNewLog asserts a revocation log can be found using the given updateNum
// for the specified channel.
func fetchNewLog(t testing.TB, cdb kvdb.Backend,
	c *mig26.OpenChannel, updateNum uint64) RevocationLog {

	var newLog RevocationLog
	err := kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		chanBucket, err := mig25.FetchChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}

		logBucket, err := fetchLogBucket(chanBucket)
		if err != nil {
			return err
		}

		newLog, err = fetchRevocationLog(logBucket, updateNum)
		return err
	}, func() {})

	require.NoError(t, err, "failed to query revocation log")

	return newLog
}

// assertRevocationLog asserts two revocation logs are equal.
func assertRevocationLog(t testing.TB, want, got RevocationLog) {
	require.Equal(t, want.OurOutputIndex, got.OurOutputIndex,
		"wrong OurOutputIndex")
	require.Equal(t, want.TheirOutputIndex, got.TheirOutputIndex,
		"wrong TheirOutputIndex")
	require.Equal(t, want.CommitTxHash, got.CommitTxHash,
		"wrong CommitTxHash")
	require.Equal(t, want.TheirBalance, got.TheirBalance,
		"wrong TheirBalance")
	require.Equal(t, want.OurBalance, got.OurBalance,
		"wrong OurBalance")
	require.Equal(t, len(want.HTLCEntries), len(got.HTLCEntries),
		"wrong HTLCEntries length")

	for i, expectedHTLC := range want.HTLCEntries {
		htlc := got.HTLCEntries[i]
		require.Equal(t, expectedHTLC.Amt, htlc.Amt, "wrong Amt")
		require.Equal(t, expectedHTLC.RHash, htlc.RHash, "wrong RHash")
		require.Equal(t, expectedHTLC.Incoming, htlc.Incoming,
			"wrong Incoming")
		require.Equal(t, expectedHTLC.OutputIndex, htlc.OutputIndex,
			"wrong OutputIndex")
		require.Equal(t, expectedHTLC.RefundTimeout, htlc.RefundTimeout,
			"wrong RefundTimeout")
	}
}

// BenchmarkMigration creates a benchmark test for the migration. The test uses
// the flag `-benchtime` to specify how many revocation logs we want to test.
func BenchmarkMigration(b *testing.B) {
	// Stop the timer and start it again later when the actual migration
	// starts.
	b.StopTimer()

	// Gather number of records by reading `-benchtime` flag.
	numLogs := b.N

	// Create a mock store.
	mockStore := &mockStore{}
	mockStore.On("AddNextEntry", mock.Anything).Return(nil)
	mockStore.On("Encode", mock.Anything).Return(nil)

	// Build the test data.
	oldLogs := make([]mig.ChannelCommitment, numLogs)
	beforeMigration := func(db kvdb.Backend) error {
		fmt.Printf("\nBuilding test data for %d logs...\n", numLogs)
		defer fmt.Println("Finished building test data, migrating...")

		// We use a mock store here to bypass the check in
		// `AddNextEntry` so we don't need a "read" preimage here. This
		// shouldn't affect our benchmark result as the migration will
		// load the actual store from db.
		c := createTestChannel(nil)
		c.RevocationStore = mockStore

		// Create the test logs.
		for i := 0; i < numLogs; i++ {
			oldLog := oldLog2
			oldLog.CommitHeight = uint64(i)
			oldLogs[i] = oldLog
		}

		return setupTestLogs(db, c, oldLogs, nil)
	}

	cfg := &MigrateRevLogConfigImpl{
		NoAmountData: !withAmtData,
	}

	// Run the migration test.
	migtest.ApplyMigrationWithDB(
		b,
		beforeMigration,
		nil,
		func(db kvdb.Backend) error {
			b.StartTimer()
			defer b.StopTimer()

			return MigrateRevocationLog(db, cfg)
		},
		false,
	)
}
