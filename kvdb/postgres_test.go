//go:build kvdb_postgres
// +build kvdb_postgres

package kvdb

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/stretchr/testify/require"
)

type m = map[string]interface{}

func TestPostgres(t *testing.T) {
	stop, err := postgres.StartEmbeddedPostgres()
	require.NoError(t, err)
	defer stop()

	tests := []struct {
		name       string
		test       func(*testing.T, walletdb.DB)
		expectedDb m
	}{
		{
			name: "read cursor empty interval",
			test: testReadCursorEmptyInterval,
		},
		{
			name: "read cursor non empty interval",
			test: testReadCursorNonEmptyInterval,
		},
		{
			name: "read write cursor",
			test: testReadWriteCursor,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(4), "key": "da", "parent_id": int64(1), "sequence": nil, "value": "3"},
					{"id": int64(6), "key": "a", "parent_id": int64(1), "sequence": nil, "value": "0"},
					{"id": int64(7), "key": "f", "parent_id": int64(1), "sequence": nil, "value": "5"},
					{"id": int64(3), "key": "c", "parent_id": int64(1), "sequence": nil, "value": "3"},
					{"id": int64(9), "key": "cx", "parent_id": int64(1), "sequence": nil, "value": "x"},
					{"id": int64(10), "key": "cy", "parent_id": int64(1), "sequence": nil, "value": "y"},
				},
			},
		},
		{
			name: "read write cursor with bucket and value",
			test: testReadWriteCursorWithBucketAndValue,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "key", "parent_id": int64(1), "sequence": nil, "value": "val"},
					{"id": int64(3), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(4), "key": "pear", "parent_id": int64(1), "sequence": nil, "value": nil},
				},
			},
		},
		{
			name: "bucket creation",
			test: testBucketCreation,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(3), "key": "mango", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(4), "key": "pear", "parent_id": int64(2), "sequence": nil, "value": nil},
				},
			},
		},
		{
			name: "bucket deletion",
			test: testBucketDeletion,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(3), "key": "key1", "parent_id": int64(2), "sequence": nil, "value": "val1"},
					{"id": int64(5), "key": "key3", "parent_id": int64(2), "sequence": nil, "value": "val3"},
				},
			},
		},
		{
			name: "bucket for each",
			test: func(t *testing.T, db walletdb.DB) {
				testBucketIterator(t, db, func(bucket walletdb.ReadWriteBucket,
					callback func(key, val []byte) error) error {

					return bucket.ForEach(callback)
				})
			},
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(3), "key": "key1", "parent_id": int64(1), "sequence": nil, "value": "val1"},
					{"id": int64(4), "key": "key1", "parent_id": int64(2), "sequence": nil, "value": "val1"},
					{"id": int64(5), "key": "key2", "parent_id": int64(1), "sequence": nil, "value": "val2"},
					{"id": int64(6), "key": "key2", "parent_id": int64(2), "sequence": nil, "value": "val2"},
					{"id": int64(7), "key": "key3", "parent_id": int64(1), "sequence": nil, "value": "val3"},
					{"id": int64(8), "key": "key3", "parent_id": int64(2), "sequence": nil, "value": "val3"},
				},
			},
		},
		{
			name: "bucket for all",
			test: func(t *testing.T, db walletdb.DB) {
				testBucketIterator(t, db, func(bucket walletdb.ReadWriteBucket,
					callback func(key, val []byte) error) error {

					return ForAll(bucket, callback)
				})
			},
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(3), "key": "key1", "parent_id": int64(1), "sequence": nil, "value": "val1"},
					{"id": int64(4), "key": "key1", "parent_id": int64(2), "sequence": nil, "value": "val1"},
					{"id": int64(5), "key": "key2", "parent_id": int64(1), "sequence": nil, "value": "val2"},
					{"id": int64(6), "key": "key2", "parent_id": int64(2), "sequence": nil, "value": "val2"},
					{"id": int64(7), "key": "key3", "parent_id": int64(1), "sequence": nil, "value": "val3"},
					{"id": int64(8), "key": "key3", "parent_id": int64(2), "sequence": nil, "value": "val3"},
				},
			},
		},
		{
			name: "bucket for each with error",
			test: testBucketForEachWithError,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(3), "key": "pear", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(4), "key": "key1", "parent_id": int64(1), "sequence": nil, "value": "val1"},
					{"id": int64(5), "key": "key2", "parent_id": int64(1), "sequence": nil, "value": "val2"},
				},
			},
		},
		{
			name: "bucket sequence",
			test: testBucketSequence,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(2), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": int64(4), "value": nil},
				},
			},
		},
		{
			name: "key clash",
			test: testKeyClash,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "key", "parent_id": int64(1), "sequence": nil, "value": "val"},
					{"id": int64(3), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": nil},
				},
			},
		},
		{
			name: "bucket create delete",
			test: testBucketCreateDelete,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(3), "key": "banana", "parent_id": int64(1), "sequence": nil, "value": "value"},
				},
			},
		},
		{
			name: "tx manual commit",
			test: testTxManualCommit,
			expectedDb: m{
				"test_kv": []m{
					{"id": int64(1), "key": "apple", "parent_id": nil, "sequence": nil, "value": nil},
					{"id": int64(2), "key": "testKey", "parent_id": int64(1), "sequence": nil, "value": "testVal"},
				},
			},
		},
		{
			name: "tx rollback",
			test: testTxRollback,
			expectedDb: m{
				"test_kv": []m(nil),
			},
		},
		{
			name: "top level bucket creation",
			test: testTopLevelBucketCreation,
		},
		{
			name: "bucket operation",
			test: testBucketOperations,
		},
		{
			name: "sub bucket sequence",
			test: testSubBucketSequence,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, err := postgres.NewFixture("")
			require.NoError(t, err)

			test.test(t, f.Db)

			if test.expectedDb != nil {
				dump, err := f.Dump()
				require.NoError(t, err)
				require.Equal(t, test.expectedDb, dump)
			}
		})
	}
}
