//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package kvdb

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func TestSqlite(t *testing.T) {
	tests := []struct {
		name string
		test func(*testing.T, walletdb.DB)
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
		},
		{
			name: "read write cursor with bucket and value",
			test: testReadWriteCursorWithBucketAndValue,
		},
		{
			name: "bucket creation",
			test: testBucketCreation,
		},
		{
			name: "bucket deletion",
			test: testBucketDeletion,
		},
		{
			name: "bucket for each",
			test: testBucketForEach,
		},
		{
			name: "bucket for each with error",
			test: testBucketForEachWithError,
		},
		{
			name: "bucket sequence",
			test: testBucketSequence,
		},
		{
			name: "key clash",
			test: testKeyClash,
		},
		{
			name: "bucket create delete",
			test: testBucketCreateDelete,
		},
		{
			name: "bucket lookup after delete",
			test: testBucketLookupAfterDelete,
		},
		{
			name: "bucket lookup after create",
			test: testBucketLookupAfterCreate,
		},
		{
			name: "bucket lookup across txns",
			test: testBucketLookupAcrossTxns,
		},
		{
			name: "tx manual commit",
			test: testTxManualCommit,
		},
		{
			name: "tx rollback",
			test: testTxRollback,
		},
		{
			name: "prefetch",
			test: testPrefetch,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			db, err := StartSqliteTestBackend(
				t.TempDir(), "tmp.db", "test",
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, db.Close())
			})

			test.test(t, db)
		})
	}
}
