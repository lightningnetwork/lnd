package kvdb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func TestBolt(t *testing.T) {
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
		cache := []bool{false, true}
		for _, useCache := range cache {
			name := fmt.Sprintf("%v/Cache=%v", test.name, useCache)

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				f := NewBoltFixture(t)
				defer f.Cleanup()

				backend := f.NewBackend()
				if useCache {
					cache := NewCache(backend, nil, nil)
					require.NoError(t, cache.Init())
					backend = cache
				}

				test.test(t, backend)
			})
		}
	}
}
