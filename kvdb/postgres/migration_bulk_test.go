//go:build kvdb_postgres

package postgres

import (
	"math"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/stretchr/testify/require"
)

// TestMigrationBulkKVStorePostgres verifies explicit migration capability
// opt-in, bucket sequence preservation, leaf COPY semantics, verification,
// transaction closure, and target truncation.
func TestMigrationBulkKVStorePostgres(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stop())
	}()

	f, err := NewMigrationFixture("")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Db.Close())
	}()

	ctx := t.Context()
	store := f.Db

	empty, err := store.CheckEmpty(ctx)
	require.NoError(t, err)
	require.True(t, empty)

	tx, err := store.BeginBulk(ctx)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()

	// Both nil and non-nil empty bucket keys must follow walletdb's key
	// semantics without poisoning the bulk transaction.
	for _, key := range [][]byte{nil, {}} {
		_, err := tx.InsertBucket(ctx, nil, key, 0)
		require.ErrorIs(t, err, walletdb.ErrBucketNameRequired)
	}

	rootID, err := tx.InsertBucket(
		ctx, nil, []byte("root"), math.MaxUint64,
	)
	require.NoError(t, err)

	maxIntPlusOne := uint64(math.MaxInt64) + 1
	nestedID, err := tx.InsertBucket(
		ctx, &rootID, []byte("nested"), maxIntPlusOne,
	)
	require.NoError(t, err)

	// Leaves must belong to a previously inserted bucket. In particular,
	// the zero-value parent must not be treated as a top-level leaf.
	err = tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{{
		Key:   []byte("top-level"),
		Value: []byte("unsupported"),
	}})
	require.EqualError(t, err, "bulk leaf 0 has invalid parent id 0")

	// As with regular walletdb writes, nil and non-nil empty leaf keys are
	// invalid. The indexed error identifies the bad entry in a batch.
	for _, key := range [][]byte{nil, {}} {
		err = tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{{
			ParentID: rootID,
			Key:      key,
			Value:    []byte("value"),
		}})
		require.ErrorIs(t, err, walletdb.ErrKeyRequired)
		require.ErrorContains(t, err, "bulk leaf 0")
	}

	require.NoError(t, tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{
		{
			ParentID: rootID,
			Key:      []byte("a"),
			Value:    []byte("value"),
		},
		{
			ParentID: nestedID,
			Key:      []byte("empty"),
			Value:    []byte{},
		},
		{
			ParentID: nestedID,
			Key:      []byte("nil"),
			Value:    nil,
		},
	}))

	require.NoError(t, tx.Commit())

	_, err = tx.InsertBucket(ctx, nil, []byte("closed"), 0)
	require.ErrorIs(t, err, walletdb.ErrTxClosed)
	require.ErrorIs(t, tx.InsertLeaves(ctx, nil), walletdb.ErrTxClosed)

	empty, err = store.CheckEmpty(ctx)
	require.NoError(t, err)
	require.False(t, empty)

	verifier, err := store.BeginBulkVerify(ctx)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, verifier.Rollback())
	}()

	top, err := verifier.FetchTopLevel(ctx)
	require.NoError(t, err)
	require.Len(t, top, 1)
	require.Equal(t, rootID, top[0].ID)
	require.Nil(t, top[0].ParentID)
	require.Equal(t, []byte("root"), top[0].Key)
	require.True(t, top[0].IsBucket)
	require.Equal(t, uint64(math.MaxUint64), top[0].Sequence)

	rootChildren, err := verifier.FetchChildren(ctx, []int64{rootID})
	require.NoError(t, err)
	require.Len(t, rootChildren, 2)

	require.Equal(t, []byte("a"), rootChildren[0].Key)
	require.False(t, rootChildren[0].IsBucket)
	require.Equal(t, []byte("value"), rootChildren[0].Value)
	require.NotNil(t, rootChildren[0].ParentID)
	require.Equal(t, rootID, *rootChildren[0].ParentID)

	require.Equal(t, []byte("nested"), rootChildren[1].Key)
	require.True(t, rootChildren[1].IsBucket)
	require.Equal(t, nestedID, rootChildren[1].ID)
	require.Equal(t, maxIntPlusOne, rootChildren[1].Sequence)

	nestedChildren, err := verifier.FetchChildren(ctx, []int64{nestedID})
	require.NoError(t, err)
	require.Len(t, nestedChildren, 2)
	require.Equal(t, []byte("empty"), nestedChildren[0].Key)
	require.False(t, nestedChildren[0].IsBucket)
	require.NotNil(t, nestedChildren[0].Value)
	require.Empty(t, nestedChildren[0].Value)

	require.Equal(t, []byte("nil"), nestedChildren[1].Key)
	require.False(t, nestedChildren[1].IsBucket)
	require.NotNil(t, nestedChildren[1].Value)
	require.Empty(t, nestedChildren[1].Value)

	noChildren, err := verifier.FetchChildren(ctx, nil)
	require.NoError(t, err)
	require.Nil(t, noChildren)

	require.NoError(t, verifier.Rollback())

	_, err = verifier.FetchTopLevel(ctx)
	require.ErrorIs(t, err, walletdb.ErrTxClosed)
	_, err = verifier.FetchChildren(ctx, nil)
	require.ErrorIs(t, err, walletdb.ErrTxClosed)

	require.NoError(t, store.TruncateTargetTable(ctx))
	empty, err = store.CheckEmpty(ctx)
	require.NoError(t, err)
	require.True(t, empty)
}

// TestMigrationBulkKVStoreRollbackPostgres verifies that rollback discards a
// bulk load, remains idempotent, and closes the transaction to further writes.
func TestMigrationBulkKVStoreRollbackPostgres(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stop())
	}()

	f, err := NewMigrationFixture("")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Db.Close())
	}()

	ctx := t.Context()
	store := f.Db

	tx, err := store.BeginBulk(ctx)
	require.NoError(t, err)

	rootID, err := tx.InsertBucket(ctx, nil, []byte("root"), 0)
	require.NoError(t, err)
	require.NoError(t, tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{{
		ParentID: rootID,
		Key:      []byte("leaf"),
		Value:    []byte("value"),
	}}))

	require.NoError(t, tx.Rollback())
	require.NoError(t, tx.Rollback())

	_, err = tx.InsertBucket(ctx, nil, []byte("closed"), 0)
	require.ErrorIs(t, err, walletdb.ErrTxClosed)
	require.ErrorIs(t, tx.InsertLeaves(ctx, nil), walletdb.ErrTxClosed)

	empty, err := store.CheckEmpty(ctx)
	require.NoError(t, err)
	require.True(t, empty)
}

// TestMigrationBulkKVStoreMixedCasePrefixPostgres verifies that Postgres's
// unquoted SQL paths and the quoted COPY identifier resolve the same table
// when the configured prefix contains uppercase characters.
func TestMigrationBulkKVStoreMixedCasePrefixPostgres(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stop())
	}()

	f, err := newFixture(
		"", "MixedCase", false, NewMigrationBackend,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Db.Close())
	}()

	ctx := t.Context()
	tx, err := f.Db.BeginBulk(ctx)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()

	rootID, err := tx.InsertBucket(ctx, nil, []byte("root"), 0)
	require.NoError(t, err)
	require.NoError(t, tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{{
		ParentID: rootID,
		Key:      []byte("leaf"),
		Value:    []byte("value"),
	}}))
	require.NoError(t, tx.Commit())

	empty, err := f.Db.CheckEmpty(ctx)
	require.NoError(t, err)
	require.False(t, empty)
}

// TestMigrationBulkKVStoreGlobalLockPostgres runs a full bulk load and
// verification cycle with the global tx-level lock enabled to exercise the
// lock-guarded write and read paths without deadlocking.
func TestMigrationBulkKVStoreGlobalLockPostgres(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stop())
	}()

	f, err := NewMigrationFixtureWithLock("")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Db.Close())
	}()

	ctx := t.Context()
	store := f.Db

	// Write path: BeginBulk takes the exclusive lock and Commit releases
	// it.
	tx, err := store.BeginBulk(ctx)
	require.NoError(t, err)

	rootID, err := tx.InsertBucket(ctx, nil, []byte("root"), 0)
	require.NoError(t, err)
	require.NoError(t, tx.InsertLeaves(ctx, []sqlbase.MigrationBulkLeaf{{
		ParentID: rootID,
		Key:      []byte("leaf"),
		Value:    []byte("value"),
	}}))
	require.NoError(t, tx.Commit())

	// Read path: CheckEmpty and the verifier take the shared read lock.
	empty, err := store.CheckEmpty(ctx)
	require.NoError(t, err)
	require.False(t, empty)

	verifier, err := store.BeginBulkVerify(ctx)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, verifier.Rollback())
	}()

	top, err := verifier.FetchTopLevel(ctx)
	require.NoError(t, err)
	require.Len(t, top, 1)
	require.Equal(t, rootID, top[0].ID)

	children, err := verifier.FetchChildren(ctx, []int64{rootID})
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.Equal(t, []byte("value"), children[0].Value)
}
