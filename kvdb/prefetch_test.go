package kvdb

import (
	"fmt"
	"maps"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/require"
)

func fetchBucket(t *testing.T, bucket walletdb.ReadBucket) map[string]string {
	items := make(map[string]string)
	err := bucket.ForEach(func(k, v []byte) error {
		if v != nil {
			items[string(k)] = string(v)
		}

		return nil
	})
	require.NoError(t, err)

	return items
}

func alterBucket(t *testing.T, bucket walletdb.ReadWriteBucket,
	put map[string]string, remove []string) {

	for k, v := range put {
		require.NoError(t, bucket.Put([]byte(k), []byte(v)))
	}

	for _, k := range remove {
		require.NoError(t, bucket.Delete([]byte(k)))
	}
}

func prefetchTest(t *testing.T, db walletdb.DB,
	prefetchAt []bool, put map[string]string, remove []string) {

	prefetch := func(i int, tx walletdb.ReadTx) {
		require.Less(t, i, len(prefetchAt))
		if prefetchAt[i] {
			Prefetch(
				RootBucket(tx),
				[]string{"top"}, []string{"top", "bucket"},
			)
		}
	}

	items := map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
		"d": "4",
		"e": "5",
	}

	err := Update(db, func(tx walletdb.ReadWriteTx) error {
		top, err := tx.CreateTopLevelBucket([]byte("top"))
		require.NoError(t, err)
		require.NotNil(t, top)

		for k, v := range items {
			require.NoError(t, top.Put([]byte(k), []byte(v)))
		}

		bucket, err := top.CreateBucket([]byte("bucket"))
		require.NoError(t, err)
		require.NotNil(t, bucket)

		for k, v := range items {
			require.NoError(t, bucket.Put([]byte(k), []byte(v)))
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	maps.Copy(items, put)

	for _, k := range remove {
		delete(items, k)
	}

	err = Update(db, func(tx walletdb.ReadWriteTx) error {
		prefetch(0, tx)
		top := tx.ReadWriteBucket([]byte("top"))
		require.NotNil(t, top)
		alterBucket(t, top, put, remove)

		prefetch(1, tx)
		require.Equal(t, items, fetchBucket(t, top))

		prefetch(2, tx)
		bucket := top.NestedReadWriteBucket([]byte("bucket"))
		require.NotNil(t, bucket)
		alterBucket(t, bucket, put, remove)

		prefetch(3, tx)
		require.Equal(t, items, fetchBucket(t, bucket))

		return nil
	}, func() {})
	require.NoError(t, err)

	err = Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.DeleteTopLevelBucket([]byte("top"))
	}, func() {})
	require.NoError(t, err)
}

// testPrefetch tests that prefetching buckets works as expected even when the
// prefetch happens multiple times and the bucket contents change. Our expectation
// is that with or without prefetches, the kvdb layer works accourding to the
// interface specification.
func testPrefetch(t *testing.T, db walletdb.DB) {
	tests := []struct {
		put    map[string]string
		remove []string
	}{
		{
			put:    nil,
			remove: nil,
		},
		{
			put: map[string]string{
				"a":   "a",
				"aa":  "aa",
				"aaa": "aaa",
				"x":   "x",
				"y":   "y",
			},
			remove: nil,
		},
		{
			put: map[string]string{
				"a":   "a",
				"aa":  "aa",
				"aaa": "aaa",
				"x":   "x",
				"y":   "y",
			},
			remove: []string{"a", "c", "d"},
		},
		{
			put:    nil,
			remove: []string{"b", "d"},
		},
	}

	prefetchAt := [][]bool{
		{false, false, false, false},
		{true, false, false, false},
		{false, true, false, false},
		{false, false, true, false},
		{false, false, false, true},
		{true, true, false, false},
		{true, true, true, false},
		{true, true, true, true},
		{true, false, true, true},
		{true, false, false, true},
		{true, false, true, false},
	}

	for i, test := range tests {
		test := test

		for j := 0; j < len(prefetchAt); j++ {
			if !t.Run(
				fmt.Sprintf("prefetch %d %d", i, j),
				func(t *testing.T) {
					prefetchTest(
						t, db, prefetchAt[j], test.put,
						test.remove,
					)
				}) {

				fmt.Printf("Prefetch test (%d, %d) failed:\n"+
					"testcase=%v\n prefetch=%v\n",
					i, j, spew.Sdump(test),
					spew.Sdump(prefetchAt[j]))
			}
		}
	}
}
