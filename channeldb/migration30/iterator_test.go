package migration30

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (
	testRefundTimeout = uint32(740_000)
	testIncoming      = true
	testRHash         = bytes.Repeat([]byte{1}, 32)

	testOutputIndex = int32(0)
	testHTLCAmt     = lnwire.MilliSatoshi(1000_000)
	testLocalAmt    = btcutil.Amount(10_000)
	testRemoteAmt   = btcutil.Amount(20_000)

	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				Sequence: 0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{Value: int64(testHTLCAmt.ToSatoshis())},
			{Value: int64(testLocalAmt)},
			{Value: int64(testRemoteAmt)},
		},
		LockTime: 5,
	}
)

// TestLocateChanBucket checks that the updateLocator can successfully locate a
// chanBucket or returns an error.
func TestLocateChanBucket(t *testing.T) {
	t.Parallel()

	// Create test database.
	cdb, err := migtest.MakeDB(t)
	require.NoError(t, err)

	// Create a test channel.
	c := createTestChannel(nil)

	var buf bytes.Buffer
	require.NoError(t, mig.WriteOutpoint(&buf, &c.FundingOutpoint))

	// Prepare the info needed to query the bucket.
	nodePub := c.IdentityPub.SerializeCompressed()
	chainHash := c.ChainHash[:]
	cp := buf.Bytes()

	// Create test buckets.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		_, err := mig25.CreateChanBucket(tx, &c.OpenChannel)
		if err != nil {
			return err
		}
		return nil
	}, func() {})
	require.NoError(t, err)

	// testLocator is a helper closure that tests a given locator's
	// locateChanBucket method.
	testLocator := func(l *updateLocator) error {
		return kvdb.Update(cdb, func(tx kvdb.RwTx) error {
			rootBucket := tx.ReadWriteBucket(openChannelBucket)
			_, err := l.locateChanBucket(rootBucket)
			return err
		}, func() {})
	}

	testCases := []struct {
		name        string
		locator     *updateLocator
		expectedErr error
	}{
		{
			name:        "empty node pub key",
			locator:     &updateLocator{},
			expectedErr: mig25.ErrNoActiveChannels,
		},
		{
			name: "empty chainhash",
			locator: &updateLocator{
				nodePub: nodePub,
			},
			expectedErr: mig25.ErrNoActiveChannels,
		},
		{
			name: "empty funding outpoint",
			locator: &updateLocator{
				nodePub:   nodePub,
				chainHash: chainHash,
			},
			expectedErr: mig25.ErrChannelNotFound,
		},
		{
			name: "successful query",
			locator: &updateLocator{
				nodePub:         nodePub,
				chainHash:       chainHash,
				fundingOutpoint: cp,
			},
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := testLocator(tc.locator)
			require.Equal(t, tc.expectedErr, err)
		})
	}
}

// TestFindNextMigrateHeight checks that given a channel bucket, we can
// successfully find the next un-migrated commit height.
func TestFindNextMigrateHeight(t *testing.T) {
	t.Parallel()

	// Create test database.
	cdb, err := migtest.MakeDB(t)
	require.NoError(t, err)

	// tester is a helper closure that finds the next migration height.
	tester := func(c *mig26.OpenChannel) []byte {
		var height []byte
		err := kvdb.Update(cdb, func(tx kvdb.RwTx) error {
			chanBucket, err := mig25.FetchChanBucket(
				tx, &c.OpenChannel,
			)
			if err != nil {
				return err
			}

			height = findNextMigrateHeight(chanBucket)
			return nil
		}, func() {})
		require.NoError(t, err)

		return height
	}

	testCases := []struct {
		name           string
		oldLogs        []mig.ChannelCommitment
		newLogs        []mig.ChannelCommitment
		expectedHeight []byte
	}{
		{
			// When we don't have any old logs, our next migration
			// height would be nil.
			name:           "empty old logs",
			expectedHeight: nil,
		},
		{
			// When we don't have any migrated logs, our next
			// migration height would be the first height found in
			// the old logs.
			name: "empty migrated logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			expectedHeight: []byte{0, 0, 0, 0, 0, 0, 0, 1},
		},
		{
			// When we have migrated logs, the next migration
			// height should be the first height found in the old
			// logs but not in the migrated logs.
			name: "have migrated logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
			},
			expectedHeight: []byte{0, 0, 0, 0, 0, 0, 0, 2},
		},
		{
			// When both the logs have equal indexes, the next
			// migration should be nil as we've finished migrating
			// for this bucket.
			name: "have finished logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			expectedHeight: nil,
		},
		{
			// When there are new logs saved in the new bucket,
			// which happens when the node is running with
			// v.0.15.0, and we don't have any migrated logs, the
			// next migration height should be the first height
			// found in the old bucket.
			name: "have new logs but no migrated logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(3),
				createDummyChannelCommit(4),
			},
			expectedHeight: []byte{0, 0, 0, 0, 0, 0, 0, 1},
		},
		{
			// When there are new logs saved in the new bucket,
			// which happens when the node is running with
			// v.0.15.0, and we have migrated logs, the returned
			// value should be the next un-migrated height.
			name: "have new logs and migrated logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(3),
				createDummyChannelCommit(4),
			},
			expectedHeight: []byte{0, 0, 0, 0, 0, 0, 0, 2},
		},
		{
			// When there are new logs saved in the new bucket,
			// which happens when the node is running with
			// v.0.15.0, and we have corrupted logs, the returned
			// value should be the first height in the old bucket.
			name: "have new logs but missing logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(2),
				createDummyChannelCommit(3),
				createDummyChannelCommit(4),
			},
			expectedHeight: []byte{0, 0, 0, 0, 0, 0, 0, 1},
		},
		{
			// When there are new logs saved in the new bucket,
			// which happens when the node is running with
			// v.0.15.0, and we have finished the migration, we
			// expect a nil height to be returned.
			name: "have new logs and finished logs",
			oldLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
			},
			newLogs: []mig.ChannelCommitment{
				createDummyChannelCommit(1),
				createDummyChannelCommit(2),
				createDummyChannelCommit(3),
				createDummyChannelCommit(4),
			},
			expectedHeight: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Create a test channel.
			c := createTestChannel(nil)

			// Setup the database.
			err := setupTestLogs(cdb, c, tc.oldLogs, tc.newLogs)
			require.NoError(t, err)

			// Run the test and check the expected next migration
			// height is returned.
			height := tester(c)
			require.Equal(t, tc.expectedHeight, height)
		})
	}
}

// TestIterator checks that the iterator iterate the given bucket correctly.
func TestIterator(t *testing.T) {
	t.Parallel()

	// Create test database.
	cdb, err := migtest.MakeDB(t)
	require.NoError(t, err)

	// exitKey is used to signal exit when hitting this key.
	exitKey := []byte{1}

	// seekKey is used to position the cursor.
	seekKey := []byte{2}

	// endKey is the last key saved in the test bucket.
	endKey := []byte{3}

	// Create test bucket.
	bucketName := []byte("test-bucket")
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketName)
		if err != nil {
			return err
		}
		if err := bucket.Put(exitKey, testRHash); err != nil {
			return err
		}
		if err := bucket.Put(seekKey, testRHash); err != nil {
			return err
		}

		return bucket.Put(endKey, testRHash)
	}, func() {})
	require.NoError(t, err)

	// tester is a helper closure that tests the iterator.
	tester := func(seeker []byte, cb callback, expectedErr error) {
		err := kvdb.View(cdb, func(tx kvdb.RTx) error {
			bucket := tx.ReadBucket(bucketName)
			return iterator(bucket, seeker, cb)
		}, func() {})

		// Check the err is returned as expected.
		require.Equal(t, expectedErr, err)
	}

	// keysItered records the keys have been iterated.
	keysItered := make([][]byte, 0)

	// testCb creates a dummy callback that saves the keys it have
	// iterated.
	testCb := func(k, v []byte) error {
		keysItered = append(keysItered, k)
		if bytes.Equal(k, exitKey) {
			return errExit
		}
		return nil
	}

	// Test that without a seeker, we would iterate from the beginning,
	// which will end up iterating only one key since we would exit on it.
	tester(nil, testCb, errExit)
	require.Equal(t, [][]byte{exitKey}, keysItered)

	// Reset the keys.
	keysItered = make([][]byte, 0)

	// Now test that when we use a seeker, we would start our iteration at
	// the seeker posisiton. This means we won't exit it early since we've
	// skipped the exitKey.
	tester(seekKey, testCb, nil)
	require.Equal(t, [][]byte{seekKey, endKey}, keysItered)
}

// TestIterateBuckets checks that we can successfully iterate the buckets and
// update the locator during the iteration.
func TestIterateBuckets(t *testing.T) {
	t.Parallel()

	// Create test database.
	cdb, err := migtest.MakeDB(t)
	require.NoError(t, err)

	// Create three test channels.
	c1 := createTestChannel(nil)
	c2 := createTestChannel(nil)
	c3 := createTestChannel(nil)

	// Create test buckets.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		_, err := mig25.CreateChanBucket(tx, &c1.OpenChannel)
		if err != nil {
			return err
		}

		_, err = mig25.CreateChanBucket(tx, &c2.OpenChannel)
		if err != nil {
			return err
		}

		_, err = mig25.CreateChanBucket(tx, &c3.OpenChannel)
		if err != nil {
			return err
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	// testCb creates a dummy callback that saves the locator it received.
	locators := make([]*updateLocator, 0)
	testCb := func(_ kvdb.RwBucket, l *updateLocator) error { // nolint:unparam
		locators = append(locators, l)
		return nil
	}

	// Iterate the buckets with a nil locator.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(openChannelBucket)
		return iterateBuckets(bucket, nil, testCb)
	}, func() {})
	require.NoError(t, err)

	// We should see three locators.
	require.Len(t, locators, 3)

	// We now test we can iterate the buckets using a locator.
	//
	// Copy the locator which points to the second channel.
	locator := &updateLocator{
		nodePub:         locators[1].nodePub,
		chainHash:       locators[1].chainHash,
		fundingOutpoint: locators[1].fundingOutpoint,
	}

	// Reset the locators.
	locators = make([]*updateLocator, 0)

	// Iterate the buckets with a locator.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(openChannelBucket)
		return iterateBuckets(bucket, locator, testCb)
	}, func() {})
	require.NoError(t, err)

	// We should see two locators.
	require.Len(t, locators, 2)
}

// TestLocalNextUpdateNum checks that we can successfully locate the next
// migration target record.
func TestLocalNextUpdateNum(t *testing.T) {
	t.Parallel()

	// assertLocator checks the locator has expected values in its fields.
	assertLocator := func(t *testing.T, c *mig26.OpenChannel,
		height []byte, l *updateLocator) {

		var buf bytes.Buffer
		require.NoError(
			t, mig.WriteOutpoint(&buf, &c.FundingOutpoint),
		)

		// Prepare the info needed to validate the locator.
		nodePub := c.IdentityPub.SerializeCompressed()
		chainHash := c.ChainHash[:]
		cp := buf.Bytes()

		require.Equal(t, nodePub, l.nodePub, "wrong nodePub")
		require.Equal(t, chainHash, l.chainHash, "wrong chainhash")
		require.Equal(t, cp, l.fundingOutpoint, "wrong outpoint")
		require.Equal(t, height, l.nextHeight, "wrong nextHeight")
	}

	// createTwoChannels is a helper closure that creates two testing
	// channels and returns the channels sorted by their nodePub to match
	// how they are stored in boltdb.
	createTwoChannels := func() (*mig26.OpenChannel, *mig26.OpenChannel) {
		c1 := createTestChannel(nil)
		c2 := createTestChannel(nil)

		// If c1 is greater than c2, boltdb will put c2 before c1.
		if bytes.Compare(
			c1.IdentityPub.SerializeCompressed(),
			c2.IdentityPub.SerializeCompressed(),
		) > 0 {

			c1, c2 = c2, c1
		}

		return c1, c2
	}

	// createNotFinished will setup a situation where we have un-migrated
	// logs and return the next migration height.
	createNotFinished := func(cdb kvdb.Backend,
		c *mig26.OpenChannel) []byte {

		// Create test logs.
		oldLogs := []mig.ChannelCommitment{
			createDummyChannelCommit(1),
			createDummyChannelCommit(2),
		}
		newLogs := []mig.ChannelCommitment{
			createDummyChannelCommit(1),
		}
		err := setupTestLogs(cdb, c, oldLogs, newLogs)
		require.NoError(t, err)

		return []byte{0, 0, 0, 0, 0, 0, 0, 2}
	}

	// createFinished will setup a situation where all the old logs have
	// been migrated and return a nil.
	createFinished := func(cdb kvdb.Backend, c *mig26.OpenChannel) []byte { // nolint:unparam
		// Create test logs.
		oldLogs := []mig.ChannelCommitment{
			createDummyChannelCommit(1),
			createDummyChannelCommit(2),
		}
		newLogs := []mig.ChannelCommitment{
			createDummyChannelCommit(1),
			createDummyChannelCommit(2),
		}
		err := setupTestLogs(cdb, c, oldLogs, newLogs)
		require.NoError(t, err)

		return nil
	}

	// emptyChannel builds a test case where no channel buckets exist.
	emptyChannel := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		// Create the root bucket.
		err := setupTestLogs(cdb, nil, nil, nil)
		require.NoError(t, err)

		return nil, nil
	}

	// singleChannelNoLogs builds a test case where we have a single
	// channel without any revocation logs.
	singleChannelNoLogs := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		// Create a test channel.
		c := createTestChannel(nil)

		// Create test logs.
		err := setupTestLogs(cdb, c, nil, nil)
		require.NoError(t, err)

		return c, nil
	}

	// singleChannelNotFinished builds a test case where we have a single
	// channel and have unfinished old logs.
	singleChannelNotFinished := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		c := createTestChannel(nil)
		return c, createNotFinished(cdb, c)
	}

	// singleChannelFinished builds a test where we have a single channel
	// and have finished all the migration.
	singleChannelFinished := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		c := createTestChannel(nil)
		return c, createFinished(cdb, c)
	}

	// twoChannelsNotFinished builds a test case where we have two channels
	// and have unfinished old logs.
	twoChannelsNotFinished := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		c1, c2 := createTwoChannels()
		createFinished(cdb, c1)
		return c2, createNotFinished(cdb, c2)
	}

	// twoChannelsFinished builds a test case where we have two channels
	// and have finished the migration.
	twoChannelsFinished := func(cdb kvdb.Backend) (
		*mig26.OpenChannel, []byte) {

		c1, c2 := createTwoChannels()
		createFinished(cdb, c1)
		return c2, createFinished(cdb, c2)
	}

	type setupFunc func(cdb kvdb.Backend) (*mig26.OpenChannel, []byte)

	testCases := []struct {
		name         string
		setup        setupFunc
		expectFinish bool
	}{
		{
			name:         "empty buckets",
			setup:        emptyChannel,
			expectFinish: true,
		},
		{
			name:         "single channel no logs",
			setup:        singleChannelNoLogs,
			expectFinish: true,
		},
		{
			name:         "single channel not finished",
			setup:        singleChannelNotFinished,
			expectFinish: false,
		},
		{
			name:         "single channel finished",
			setup:        singleChannelFinished,
			expectFinish: true,
		},
		{
			name:         "two channels not finished",
			setup:        twoChannelsNotFinished,
			expectFinish: false,
		},
		{
			name:         "two channels finished",
			setup:        twoChannelsFinished,
			expectFinish: true,
		},
	}

	// tester is a helper closure that finds the locator.
	tester := func(t *testing.T, cdb kvdb.Backend) *updateLocator {
		var l *updateLocator
		err := kvdb.Update(cdb, func(tx kvdb.RwTx) error {
			rootBucket := tx.ReadWriteBucket(openChannelBucket)

			// Find the locator.
			locator, err := locateNextUpdateNum(rootBucket)
			if err != nil {
				return err
			}

			l = locator
			return nil
		}, func() {})
		require.NoError(t, err)

		return l
	}

	for _, tc := range testCases {
		// Create a test database.
		cdb, err := migtest.MakeDB(t)
		require.NoError(t, err)

		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Setup the test case.
			c, height := tc.setup(cdb)

			// Run the test and assert the locator.
			locator := tester(t, cdb)
			if tc.expectFinish {
				require.Nil(t, locator, "expected nil locator")
			} else {
				assertLocator(t, c, height, locator)
			}
		})
	}
}

func createDummyChannelCommit(height uint64) mig.ChannelCommitment {
	htlc := mig.HTLC{
		Amt:           testHTLCAmt,
		RefundTimeout: testRefundTimeout,
		OutputIndex:   testOutputIndex,
		Incoming:      testIncoming,
	}
	copy(htlc.RHash[:], testRHash)
	c := mig.ChannelCommitment{
		CommitHeight: height,
		Htlcs:        []mig.HTLC{htlc},
		CommitTx:     testTx,
	}
	return c
}
