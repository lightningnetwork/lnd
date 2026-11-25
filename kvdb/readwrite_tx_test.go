package kvdb

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func testTxManualCommit(t *testing.T, db walletdb.DB) {
	tx, err := db.BeginReadWriteTx()
	require.NoError(t, err)
	require.NotNil(t, tx)

	// Every callback must run on commit, in the order it was added, not
	// just the last one.
	var committed []int
	tx.OnCommit(func() {
		committed = append(committed, 1)
	})
	tx.OnCommit(func() {
		committed = append(committed, 2)
	})

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	require.NoError(t, err)
	require.NotNil(t, apple)
	require.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	banana, err := tx.CreateTopLevelBucket([]byte("banana"))
	require.NoError(t, err)
	require.NotNil(t, banana)
	require.NoError(t, banana.Put([]byte("testKey"), []byte("testVal")))
	require.NoError(t, tx.DeleteTopLevelBucket([]byte("banana")))

	require.NoError(t, tx.Commit())
	require.Equal(t, []int{1, 2}, committed)
}

func testTxRollback(t *testing.T, db walletdb.DB) {
	tx, err := db.BeginReadWriteTx()
	require.Nil(t, err)
	require.NotNil(t, tx)

	apple, err := tx.CreateTopLevelBucket([]byte("apple"))
	require.Nil(t, err)
	require.NotNil(t, apple)

	require.NoError(t, apple.Put([]byte("testKey"), []byte("testVal")))

	require.NoError(t, tx.Rollback())
	require.Error(t, walletdb.ErrTxClosed, tx.Commit())
}
