package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (
	testChainHash = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}

	testChanPoint1 = wire.OutPoint{
		Hash: chainhash.Hash{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4,
		},
		Index: 1,
	}
)

// TestPersistReport tests the writing and retrieval of a report on disk with
// and without a spend txid.
func TestPersistReport(t *testing.T) {
	tests := []struct {
		name      string
		spendTxID *chainhash.Hash
	}{
		{
			name:      "Non-nil spend txid",
			spendTxID: &testChanPoint1.Hash,
		},
		{
			name:      "Nil spend txid",
			spendTxID: nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			db, err := MakeTestDB(t)
			require.NoError(t, err)

			channelOutpoint := testChanPoint1

			testOutpoint := testChanPoint1
			testOutpoint.Index++

			report := &ResolverReport{
				OutPoint:        testOutpoint,
				Amount:          2,
				ResolverType:    1,
				ResolverOutcome: 2,
				SpendTxID:       test.spendTxID,
			}

			// Write report to disk, and ensure it is identical when
			// it is read.
			err = db.PutResolverReport(
				nil, testChainHash, &channelOutpoint, report,
			)
			require.NoError(t, err)

			reports, err := db.FetchChannelReports(
				testChainHash, &channelOutpoint,
			)
			require.NoError(t, err)
			require.Equal(t, report, reports[0])
		})
	}
}

// TestFetchChannelReadBucket tests retrieval of the reports bucket for a
// channel, testing that the appropriate error is returned based on the state
// of the existing bucket.
func TestFetchChannelReadBucket(t *testing.T) {
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	channelOutpoint := testChanPoint1

	testOutpoint := testChanPoint1
	testOutpoint.Index++

	// If we attempt to get reports when we do not have any present, we
	// expect to fail because our chain hash bucket is not present.
	_, err = db.FetchChannelReports(
		testChainHash, &channelOutpoint,
	)
	require.Equal(t, ErrNoChainHashBucket, err)

	// Finally we write a report to disk and check that we can fetch it.
	report := &ResolverReport{
		OutPoint:        testOutpoint,
		Amount:          2,
		ResolverOutcome: 1,
		ResolverType:    2,
		SpendTxID:       nil,
	}

	err = db.PutResolverReport(
		nil, testChainHash, &channelOutpoint, report,
	)
	require.NoError(t, err)

	// Now that the channel bucket exists, we expect the channel to be
	// successfully fetched, with no reports.
	reports, err := db.FetchChannelReports(testChainHash, &testChanPoint1)
	require.NoError(t, err)
	require.Equal(t, report, reports[0])
}

// TestFetchChannelWriteBucket tests the creation of missing buckets when
// retrieving the reports bucket.
func TestFetchChannelWriteBucket(t *testing.T) {
	createReportsBucket := func(tx kvdb.RwTx) (kvdb.RwBucket, error) {
		return tx.CreateTopLevelBucket(closedChannelBucket)
	}

	createChainHashBucket := func(reports kvdb.RwBucket) (kvdb.RwBucket,
		error) {

		return reports.CreateBucketIfNotExists(testChainHash[:])
	}

	createChannelBucket := func(chainHash kvdb.RwBucket) (kvdb.RwBucket,
		error) {

		var chanPointBuf bytes.Buffer
		err := graphdb.WriteOutpoint(&chanPointBuf, &testChanPoint1)
		require.NoError(t, err)

		return chainHash.CreateBucketIfNotExists(chanPointBuf.Bytes())
	}

	tests := []struct {
		name  string
		setup func(tx kvdb.RwTx) error
	}{
		{
			name: "no existing buckets",
			setup: func(tx kvdb.RwTx) error {
				return nil
			},
		},
		{
			name: "reports bucket exists",
			setup: func(tx kvdb.RwTx) error {
				_, err := createReportsBucket(tx)
				return err
			},
		},
		{
			name: "chainhash bucket exists",
			setup: func(tx kvdb.RwTx) error {
				reports, err := createReportsBucket(tx)
				if err != nil {
					return err
				}

				_, err = createChainHashBucket(reports)
				return err
			},
		},
		{
			name: "channel bucket exists",
			setup: func(tx kvdb.RwTx) error {
				reports, err := createReportsBucket(tx)
				if err != nil {
					return err
				}

				chainHash, err := createChainHashBucket(reports)
				if err != nil {
					return err
				}

				_, err = createChannelBucket(chainHash)
				return err
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			db, err := MakeTestDB(t)
			require.NoError(t, err)

			// Update our db to the starting state we expect.
			err = kvdb.Update(db, test.setup, func() {})
			require.NoError(t, err)

			// Try to get our report bucket.
			err = kvdb.Update(db, func(tx kvdb.RwTx) error {
				_, err := fetchReportWriteBucket(
					tx, testChainHash, &testChanPoint1,
				)
				return err
			}, func() {})
			require.NoError(t, err)
		})
	}
}
